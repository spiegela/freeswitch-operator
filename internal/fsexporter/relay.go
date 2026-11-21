package fsexporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// BargeInContentType is the body type of the SIP INFO requests the relay sends to the agent.
const BargeInContentType = "application/x-barge-in+json"

// The voice detectors' events: mod_vad_silero's and mod_vad_detect's (WebRTC's detector).
const (
	subclassSilero = "vad_silero:detect"
	subclassWebRTC = "vad_detect:detection"
)

// The media bugs the edge's dialplan names its detectors (sipbench/freeswitch/conf/dialplan/trunks.xml): one on
// what the caller sends, on the caller's leg, and one on what the agent sends, on the agent's leg.
const (
	partyCaller = "caller"
	partyAgent  = "agent"
)

// agentLegVar is the caller leg's variable naming the agent leg's uuid, set by the dialplan before the bridge.
const agentLegVar = "variable_barge_in_agent_uuid"

// Detection is the body of one INFO: a detector deciding someone started or stopped talking.
type Detection struct {
	// Event is speech-started or speech-stopped.
	Event string `json:"event"`
	// Party is whose audio it was heard in: caller or agent.
	Party string `json:"party"`
	// Engine is the detector: silero or webrtc.
	Engine string `json:"engine"`
	// Probability is Silero's speech probability for the window that decided; WebRTC's detector gives none.
	Probability *float64 `json:"probability,omitempty"`
	// At is when the speech started or stopped, in Unix milliseconds, as the detector dates it. Silero dates it
	// back to the speech itself, so At is earlier than the detection by its minimum speech (a start, 250 ms by
	// default) or silence (a stop, 100 ms); WebRTC's detector gives no date, so it is when its event fired.
	At int64 `json:"at"`
}

// Relay sends the edge's barge-in voice detections to the agent: each start or stop of speech FreeSWITCH's
// detectors report on a call becomes a SIP INFO (BargeInContentType, a Detection) on that call's agent leg,
// whichever party it was heard in. It keeps one event socket connection subscribed to the detectors' events and
// another to send the INFO through (uuid_send_info), and reconnects when either breaks. A call whose detection
// runs at the agent (barge_in_vad livekit) has no detectors, so nothing is sent for it.
//
// uuid_send_info returns once the INFO is queued, not when it is answered: an agent refusing it (as diago does
// any INFO but keypad tones, with 406) is not seen here.
type Relay struct {
	Address  string        // host:port of the event socket
	Password string        // event socket password
	Timeout  time.Duration // for connecting and for each command
	Retry    time.Duration // between reconnects; default 2 s
	Log      *slog.Logger

	connected  prometheus.Gauge
	detections *prometheus.CounterVec
}

// The outcomes counted for each detection.
const (
	resultRelayed  = "relayed"   // the INFO was queued on the agent leg
	resultNoTarget = "no_target" // a caller detection on a leg that names no agent leg
	resultFailed   = "failed"    // uuid_send_info failed (the agent leg was gone, say)
)

// NewRelay returns a relay; register its Collectors and call Run.
func NewRelay(address, password string, timeout time.Duration, log *slog.Logger) *Relay {
	return &Relay{
		Address: address, Password: password, Timeout: timeout, Log: log,
		connected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "freeswitch_barge_in_relay_connected",
			Help: "1 while the barge-in relay is subscribed to FreeSWITCH's voice detector events, 0 otherwise.",
		}),
		detections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "freeswitch_barge_in_detections_total",
			Help: "Voice detections seen by the barge-in relay, by party (caller or agent), event (speech-started " +
				"or speech-stopped) and result: relayed (sent to the agent as SIP INFO), no_target (no agent leg " +
				"to send it on) or failed.",
		}, []string{"party", "event", "result"}),
	}
}

// Collectors are the relay's metrics.
func (r *Relay) Collectors() []prometheus.Collector {
	return []prometheus.Collector{r.connected, r.detections}
}

// Run relays until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	retry := r.Retry
	if retry <= 0 {
		retry = 2 * time.Second
	}
	for ctx.Err() == nil {
		err := r.session(ctx)
		if ctx.Err() != nil {
			return
		}
		r.logger().Debug("barge-in relay disconnected; reconnecting", "address", r.Address, "error", err, "in", retry)
		select {
		case <-ctx.Done():
		case <-time.After(retry):
		}
	}
}

// session subscribes and relays until a connection breaks or ctx ends.
func (r *Relay) session(ctx context.Context) error {
	events, err := dialESL(r.Address, r.Password, r.Timeout)
	if err != nil {
		return err
	}
	defer func() { _ = events.Close() }()
	cmds, err := dialESL(r.Address, r.Password, r.Timeout)
	if err != nil {
		return err
	}
	defer func() { _ = cmds.Close() }()
	if _, err := events.Command("event plain CUSTOM "+subclassSilero+" "+subclassWebRTC, r.Timeout); err != nil {
		return err
	}
	r.connected.Set(1)
	defer r.connected.Set(0)
	r.logger().Info("relaying barge-in detections as SIP INFO", "eventSocket", r.Address)
	stop := context.AfterFunc(ctx, func() { _ = events.Close() })
	defer stop()
	for {
		h, body, err := events.read()
		if err != nil {
			return err
		}
		switch h.Get("Content-Type") {
		case "text/disconnect-notice":
			return errors.New("FreeSWITCH closed the event socket")
		case "text/event-plain":
			if err := r.relay(cmds, body); err != nil {
				return err
			}
		}
	}
}

// relay sends one event's detection, if it is one. It returns an error only when the command connection broke.
func (r *Relay) relay(cmds *eslClient, event []byte) error {
	h, body := parseEvent(event)
	d, target, ok := detectionOf(h, body)
	if !ok {
		return nil
	}
	if target == "" {
		r.detections.WithLabelValues(d.Party, d.Event, resultNoTarget).Inc()
		r.logger().Warn("barge-in detection with no agent leg to send it to", "uuid", h.Get("Unique-ID"), "party", d.Party)
		return nil
	}
	b, _ := json.Marshal(d) // no spaces: uuid_send_info's message is the rest of its arguments, as given
	mime, sub, _ := strings.Cut(BargeInContentType, "/")
	out, err := cmds.API("uuid_send_info "+target+" "+mime+" "+sub+" "+string(b), r.Timeout)
	if err != nil {
		r.detections.WithLabelValues(d.Party, d.Event, resultFailed).Inc()
		r.logger().Warn("barge-in detection not sent", "agentLeg", target, "party", d.Party, "error", err)
		if out == "" { // the connection, not FreeSWITCH, failed
			return err
		}
		return nil
	}
	r.detections.WithLabelValues(d.Party, d.Event, resultRelayed).Inc()
	r.logger().Debug("barge-in detection sent", "agentLeg", target, "detection", string(b))
	return nil
}

// detectionOf reads a detector event: the detection, the agent leg's uuid to send it on ("" when the caller's leg
// names none), and false for anything that isn't one of the edge's barge-in detections.
func detectionOf(h textproto.MIMEHeader, body []byte) (Detection, string, bool) {
	var d Detection
	switch h.Get("Event-Subclass") {
	case subclassSilero:
		d.Engine = "silero"
	case subclassWebRTC:
		d.Engine = "webrtc"
	default:
		return d, "", false
	}
	switch h.Get("detected-event") {
	case "start_talking":
		d.Event = "speech-started"
	case "stop_talking":
		d.Event = "speech-stopped"
	default:
		return d, "", false
	}
	var target string
	switch d.Party = h.Get("media-bugname"); d.Party {
	case partyCaller:
		target = h.Get(agentLegVar)
	case partyAgent:
		target = h.Get("Unique-ID")
	default:
		return d, "", false // someone else's detector
	}
	if us, err := strconv.ParseInt(h.Get("Event-Date-Timestamp"), 10, 64); err == nil {
		d.At = us / 1000
	}
	// Silero says more in a JSON body: the deciding window's probability, and its own timestamp.
	var silero struct {
		Probability *float64 `json:"probability"`
		Timestamp   int64    `json:"timestamp"`
	}
	if len(body) > 0 && json.Unmarshal(body, &silero) == nil {
		d.Probability = silero.Probability
		if silero.Timestamp > 0 {
			d.At = silero.Timestamp
		}
	}
	return d, target, true
}

// parseEvent splits a text/event-plain event into its headers (URL-decoded) and its body.
func parseEvent(event []byte) (textproto.MIMEHeader, []byte) {
	head, body, _ := bytes.Cut(event, []byte("\n\n"))
	h := textproto.MIMEHeader{}
	for line := range strings.SplitSeq(string(head), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if dv, err := url.PathUnescape(v); err == nil {
			v = dv
		}
		h.Add(k, v)
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n >= 0 && n <= len(body) {
		body = body[:n]
	}
	return h, body
}

func (r *Relay) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}
