package calltrace

import "github.com/prometheus/client_golang/prometheus"

const resultLabel = "result"

// Metrics are the tracer's own counters, served at /metrics.
type Metrics struct {
	HEPPackets    *prometheus.CounterVec
	SIPMessages   *prometheus.CounterVec
	Webhooks      *prometheus.CounterVec
	Calls         *prometheus.CounterVec
	ActiveCalls   prometheus.Gauge
	SpansExported prometheus.Counter
	SpansDropped  prometheus.Counter
	ExportErrors  prometheus.Counter
}

// NewMetrics creates the metrics; register them with Collectors.
func NewMetrics() *Metrics {
	return &Metrics{
		HEPPackets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "call_tracer_hep_packets_total",
			Help: "HEP packets received, by result: sip (a SIP message), other (another payload type), bad (undecodable).",
		}, []string{resultLabel}),
		SIPMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "call_tracer_sip_messages_total",
			Help: "SIP messages, by result: traced (part of a call), untraced (keepalives, registrations, unknown dialogs), bad (unparsable).",
		}, []string{resultLabel}),
		Webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "call_tracer_webhooks_total",
			Help: "LiveKit webhooks, by result: traced, untraced (no call to attach them to), ignored (an event not traced), rejected (bad signature or body).",
		}, []string{resultLabel}),
		Calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "call_tracer_calls_total",
			Help: "Calls traced, by how the trace ended: ended (the call finished), timeout (no final answer, or past the longest call), evicted (too many calls at once), shutdown (the tracer stopped).",
		}, []string{"end"}),
		ActiveCalls: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "call_tracer_active_calls",
			Help: "Calls being traced.",
		}),
		SpansExported: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "call_tracer_spans_exported_total",
			Help: "Spans the OTLP endpoint accepted.",
		}),
		SpansDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "call_tracer_spans_dropped_total",
			Help: "Spans dropped: the export queue was full, or the endpoint refused them twice.",
		}),
		ExportErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "call_tracer_export_errors_total",
			Help: "Batches the OTLP endpoint refused or didn't answer, after a retry.",
		}),
	}
}

// Collectors returns the metrics to register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.HEPPackets, m.SIPMessages, m.Webhooks, m.Calls, m.ActiveCalls,
		m.SpansExported, m.SpansDropped, m.ExportErrors}
}
