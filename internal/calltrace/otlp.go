package calltrace

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// TraceID is a call's trace ID: the MD5 of the Call-ID the call arrived with, so anything that knows the Call-ID
// can find the trace (and an MD5 is exactly an OpenTelemetry trace ID's 16 bytes).
type TraceID [16]byte

// SpanID is a span's ID. The tracer derives them from what the span describes, so a span is the same span however
// many times its messages are captured.
type SpanID [8]byte

// TraceIDFor returns the trace ID of the call that arrived with callID.
func TraceIDFor(callID string) TraceID { return md5.Sum([]byte(callID)) }

func spanIDFor(t TraceID, parts ...string) SpanID {
	h := md5.New()
	h.Write(t[:])
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	var id SpanID
	copy(id[:], h.Sum(nil))
	return id
}

// SpanKind is an OpenTelemetry span kind.
type SpanKind = tracepb.Span_SpanKind

// Span is a finished span, ready to export.
type Span struct {
	Trace   TraceID
	ID      SpanID
	Parent  SpanID // zero for the root
	Service string
	Name    string
	Kind    SpanKind
	Start   time.Time
	End     time.Time
	Attrs   []Attr
	Events  []Event
	Error   bool
	Status  string
}

// Attr is a span or event attribute: Value is a string, int64, int or bool.
type Attr struct {
	Key   string
	Value any
}

// Event is a timed annotation on a span.
type Event struct {
	Time  time.Time
	Name  string
	Attrs []Attr
}

// ExportRequest builds the OTLP request for spans, grouped by service.
func ExportRequest(spans []Span) *collectorpb.ExportTraceServiceRequest {
	by := map[string][]*tracepb.Span{}
	for i := range spans {
		by[spans[i].Service] = append(by[spans[i].Service], spans[i].proto())
	}
	services := make([]string, 0, len(by))
	for s := range by {
		services = append(services, s)
	}
	slices.Sort(services)
	req := &collectorpb.ExportTraceServiceRequest{}
	for _, s := range services {
		req.ResourceSpans = append(req.ResourceSpans, &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: attrs([]Attr{{"service.name", s}})},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "github.com/spiegela/freeswitch-operator/call-tracer"},
				Spans: by[s],
			}},
		})
	}
	return req
}

func (s *Span) proto() *tracepb.Span {
	p := &tracepb.Span{
		TraceId:           append([]byte(nil), s.Trace[:]...),
		SpanId:            append([]byte(nil), s.ID[:]...),
		Name:              s.Name,
		Kind:              s.Kind,
		StartTimeUnixNano: uint64(s.Start.UnixNano()),
		EndTimeUnixNano:   uint64(s.End.UnixNano()),
		Attributes:        attrs(s.Attrs),
	}
	if s.Parent != (SpanID{}) {
		p.ParentSpanId = append([]byte(nil), s.Parent[:]...)
	}
	for _, e := range s.Events {
		p.Events = append(p.Events, &tracepb.Span_Event{
			TimeUnixNano: uint64(e.Time.UnixNano()), Name: e.Name, Attributes: attrs(e.Attrs),
		})
	}
	if s.Error {
		p.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: s.Status}
	} else {
		p.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
	}
	return p
}

func attrs(in []Attr) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(in))
	for _, a := range in {
		var v *commonpb.AnyValue
		switch x := a.Value.(type) {
		case string:
			if x == "" {
				continue
			}
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: x}}
		case int:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(x)}}
		case int64:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: x}}
		case bool:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: x}}
		default:
			v = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprint(x)}}
		}
		out = append(out, &commonpb.KeyValue{Key: a.Key, Value: v})
	}
	return out
}

// Exporter sends spans to an OTLP/HTTP traces endpoint in batches.
type Exporter struct {
	// URL is the full traces endpoint, such as http://collector:4318/v1/traces.
	URL     string
	Client  *http.Client
	Log     *slog.Logger
	Metrics *Metrics

	queue chan []Span
}

// NewExporter returns an exporter; Run sends what Export queues.
func NewExporter(url string, log *slog.Logger, m *Metrics) *Exporter {
	return &Exporter{URL: url, Client: &http.Client{Timeout: 10 * time.Second}, Log: log, Metrics: m,
		queue: make(chan []Span, 1024)}
}

// Export queues spans. It never blocks: when the queue is full the spans are dropped and counted.
func (e *Exporter) Export(spans []Span) {
	if len(spans) == 0 {
		return
	}
	select {
	case e.queue <- spans:
	default:
		e.Metrics.SpansDropped.Add(float64(len(spans)))
	}
}

// Run sends queued spans every second, or sooner once 512 are waiting, until ctx ends; then sends what is left.
func (e *Exporter) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var batch []Span
	flush := func() {
		if len(batch) > 0 {
			e.send(batch)
			batch = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case s := <-e.queue:
					batch = append(batch, s...)
				default:
					flush()
					return
				}
			}
		case s := <-e.queue:
			batch = append(batch, s...)
			if len(batch) >= 512 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (e *Exporter) send(spans []Span) {
	body, err := proto.Marshal(ExportRequest(spans))
	if err != nil {
		e.Log.Error("encoding spans", "error", err)
		e.Metrics.SpansDropped.Add(float64(len(spans)))
		return
	}
	for range 2 {
		if err = e.post(body); err == nil {
			e.Metrics.SpansExported.Add(float64(len(spans)))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.Log.Warn("exporting spans", "spans", len(spans), "error", err)
	e.Metrics.ExportErrors.Inc()
	e.Metrics.SpansDropped.Add(float64(len(spans)))
}

func (e *Exporter) post(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
