package calltrace

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestDecodeKamailioCapture(t *testing.T) {
	// Two packets Kamailio 6.0 sent with spec.capture set (make check-kamailio-capture, HEP_OUT=...): an OPTIONS
	// keepalive and its 503.
	b, err := os.ReadFile("testdata/kamailio-options.hep")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(b) > 0 {
		n := int(binary.BigEndian.Uint16(b[4:6]))
		p, err := DecodeHEP(b[:n])
		if err != nil {
			t.Fatal(err)
		}
		b = b[n:]
		if p.CaptureID != 7 || p.PayloadType != hepProtoSIP || p.Transport != 17 || p.Time.Year() < 2026 {
			t.Errorf("packet fields: %+v", p)
		}
		m, err := ParseSIP(p.Payload)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s->%s %v %s %d %s", p.Src, p.Dst, m.Request, m.Method, m.Status, m.CallID))
	}
	want := []string{
		// Kamailio listens on 0.0.0.0 here, and captures its own address as that.
		"172.31.0.20:5099->0.0.0.0:5060 true OPTIONS 0 capture-1",
		"0.0.0.0:5060->172.31.0.20:5099 false OPTIONS 503 capture-1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestHEPRoundTrip(t *testing.T) {
	p := Packet{Time: time.Unix(1700000000, 123456000), Src: netip.MustParseAddrPort("[2001:db8::1]:5060"),
		Dst: netip.MustParseAddrPort("[2001:db8::2]:5080"), Transport: 6, PayloadType: 1, CaptureID: 42,
		Payload: []byte("hello")}
	got, err := DecodeHEP(EncodeHEP(p))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(p.Time) || got.Src != p.Src || got.Dst != p.Dst || got.CaptureID != 42 ||
		got.Transport != 6 || string(got.Payload) != "hello" {
		t.Errorf("round trip: %+v", got)
	}
	for _, bad := range [][]byte{nil, []byte("HEP2xxxx"), []byte("HEP3\x00\xff"), EncodeHEP(p)[:20]} {
		if _, err := DecodeHEP(bad); err == nil {
			t.Errorf("decoded %q", bad)
		}
	}
}

func TestParseSIP(t *testing.T) {
	m, err := ParseSIP([]byte("INVITE sip:+15550100@edge.example.com;transport=udp SIP/2.0\r\n" +
		"v: SIP/2.0/UDP 192.0.2.1;branch=z9hG4bK1\r\n" +
		"f: \"Alice\" <sip:+15550199@carrier.example.net>;tag=abc\r\n" +
		"t: <sip:+15550100@edge.example.com>\r\n" +
		"i: call-1@carrier\r\nCSeq: 7 INVITE\r\nX-CID: root-1\r\nSubject: a\r\n folded\r\n\r\nv=0\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Request || m.Method != "INVITE" || m.CallID != "call-1@carrier" || m.CSeq != 7 || m.FromTag != "abc" ||
		m.ToTag != "" || m.FromUser != "+15550199" || m.ToUser != "+15550100" || m.Header("x-cid") != "root-1" ||
		m.Header("Subject") != "a folded" || uriHost(m.RequestURI) != "edge.example.com" {
		t.Errorf("parsed: %+v", m)
	}
	r, err := ParseSIP([]byte("SIP/2.0 486 Busy Here\r\nCall-ID: x\r\nCSeq: 7 INVITE\r\nTo: <sip:b@h>;tag=t2\r\n\r\n"))
	if err != nil || r.Request || r.Status != 486 || r.Phrase != "Busy Here" || r.Method != "INVITE" ||
		r.ToTag != "t2" || !r.Final() {
		t.Errorf("parsed response: %+v %v", r, err)
	}
	for _, bad := range []string{"", "hello", "SIP/2.0 99 x\r\nCall-ID: x\r\nCSeq: 1 A\r\n", "OPTIONS sip:x SIP/2.0\r\nCSeq: 1 OPTIONS\r\n"} {
		if _, err := ParseSIP([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

// bench plays a call through the edge: a carrier, Kamailio (capture ID 1), FreeSWITCH (2) bridging to LiveKit.
type bench struct {
	t     *testing.T
	now   time.Time
	tr    *Tracker
	spans []Span
}

var (
	carrier = netip.MustParseAddrPort("198.51.100.7:5060")
	kam     = netip.MustParseAddrPort("10.0.1.5:5060")
	fsIn    = netip.MustParseAddrPort("10.0.1.5:5062")
	fs0In   = netip.MustParseAddrPort("10.0.1.9:5062")
	fsOut   = netip.MustParseAddrPort("10.0.1.5:5080")
	lk      = netip.MustParseAddrPort("10.96.4.4:5060")
)

func newBench(t *testing.T) *bench {
	b := &bench{t: t, now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	b.tr = NewTracker(Config{CaptureNames: map[uint32]string{1: "kamailio", 2: "freeswitch"}}, nil,
		func() time.Time { return b.now })
	return b
}

func (b *bench) at(ms int) time.Time { return b.now.Add(time.Duration(ms) * time.Millisecond) }

// send has the components with the given capture IDs capture msg on its way from src to dst, ms after start
// (the second component a millisecond later). Kamailio, on 0.0.0.0, captures its own address as that.
func (b *bench) send(ms int, src, dst netip.AddrPort, msg string, captures ...uint32) {
	for i, id := range captures {
		s, d := src, dst
		if id == 1 && s == kam {
			s = netip.AddrPortFrom(netip.IPv4Unspecified(), kam.Port())
		}
		if id == 1 && d == kam {
			d = netip.AddrPortFrom(netip.IPv4Unspecified(), kam.Port())
		}
		p := Packet{Time: b.at(ms + i), Src: s, Dst: d, Transport: 17, PayloadType: 1, CaptureID: id,
			Payload: []byte(strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n\r\n")}
		b.spans = append(b.spans, b.tr.HandleHEP(EncodeHEP(p))...)
	}
}

func (b *bench) webhook(ms int, ev string) {
	var e LiveKitEvent
	if err := json.Unmarshal([]byte(ev), &e); err != nil {
		b.t.Fatal(err)
	}
	saved := b.now
	b.now = b.at(ms)
	b.tr.HandleLiveKit(&e)
	b.now = saved
}

func (b *bench) sweep(ms int) {
	saved := b.now
	b.now = b.at(ms)
	b.spans = append(b.spans, b.tr.Sweep()...)
	b.now = saved
}

func invite(callID, cseq, extra string) string {
	return "INVITE sip:+15550100@edge SIP/2.0\nVia: SIP/2.0/UDP x;branch=z9hG4bK" + callID + cseq +
		"\nFrom: <sip:+15550199@carrier>;tag=caller\nTo: <sip:+15550100@edge>\nCall-ID: " + callID +
		"\nCSeq: " + cseq + " INVITE" + extra
}

func reply(code, callID, cseq string) string {
	return "SIP/2.0 " + code + "\nFrom: <sip:+15550199@carrier>;tag=caller\nTo: <sip:+15550100@edge>;tag=callee" +
		"\nCall-ID: " + callID + "\nCSeq: " + cseq
}

func bye(callID, cseq, fromTag string) string {
	return "BYE sip:x SIP/2.0\nFrom: <sip:a@b>;tag=" + fromTag + "\nTo: <sip:c@d>;tag=callee\nCall-ID: " + callID +
		"\nCSeq: " + cseq + " BYE\nReason: Q.850;cause=16;text=\"NORMAL_CLEARING\""
}

func (b *bench) byName() map[string]Span {
	m := map[string]Span{}
	for _, s := range b.spans {
		key := s.Service + "/" + s.Name
		for i := 2; ; i++ {
			if _, dup := m[key]; !dup {
				break
			}
			key = s.Service + "/" + s.Name + "#" + fmt.Sprint(i)
		}
		m[key] = s
	}
	return m
}

func attr(s Span, key string) any {
	for _, a := range s.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

const a, leg = "carrier-call-1", "fs-leg-1"

// playCall plays an answered call: the carrier's INVITE through Kamailio (failing over once) and FreeSWITCH to
// LiveKit, an agent joining the room, and the caller hanging up a minute later.
func playCall(t *testing.T, b *bench) {
	// Kamailio takes the carrier's INVITE, tries a FreeSWITCH node that never answers, fails over to another.
	b.send(0, carrier, kam, invite(a, "1", ""), 1)
	b.send(1, kam, carrier, reply("100 Trying", a, "1 INVITE"), 1)
	b.send(2, kam, fs0In, invite(a, "1", ""), 1)
	b.send(5002, kam, fsIn, invite(a, "1", ""), 1, 2)
	b.send(5004, fsIn, kam, reply("100 Trying", a, "1 INVITE"), 2, 1)
	// FreeSWITCH bridges to LiveKit with the carrier's Call-ID in X-CID.
	b.send(5010, fsOut, lk, invite(leg, "101", "\nX-CID: "+a), 2)
	b.send(5011, fsOut, lk, invite(leg, "101", "\nX-CID: "+a), 2) // a retransmission
	b.webhook(5030, `{"event":"room_started","room":{"sid":"RM_1","name":"call-room"}}`)
	b.webhook(5040, `{"event":"participant_joined","room":{"sid":"RM_1","name":"call-room"},
		"participant":{"sid":"PA_sip","identity":"sip_+15550199","kind":"SIP","joinedAtMs":"`+
		fmt.Sprint(b.at(5035).UnixMilli())+`","attributes":{"sip.callIDFull":"`+leg+`","sip.trunkID":"ST_1","sip.h.x-cid":"`+a+`"}}}`)
	b.webhook(5500, `{"event":"participant_joined","room":{"sid":"RM_1","name":"call-room"},
		"participant":{"sid":"PA_agent","identity":"agent-1","kind":"AGENT"}}`)
	b.webhook(5600, `{"event":"track_published","room":{"sid":"RM_1","name":"call-room"},
		"participant":{"sid":"PA_agent","identity":"agent-1","kind":"AGENT"},"track":{"sid":"TR_1","type":"AUDIO","source":"MICROPHONE"}}`)
	b.send(6000, lk, fsOut, reply("200 OK", leg, "101 INVITE"), 2)
	b.send(6005, fsIn, kam, reply("200 OK", a, "1 INVITE"), 2, 1)
	b.send(6008, kam, carrier, reply("200 OK", a, "1 INVITE"), 1)
	b.send(6020, carrier, kam, "ACK sip:x SIP/2.0\nCall-ID: "+a+"\nCSeq: 1 ACK\nFrom: <sip:a@b>;tag=caller\nTo: <sip:c@d>;tag=callee", 1)
	b.sweep(9000) // the answered hops go out; the call is still up
	if n := len(b.spans); n != 3 {
		t.Fatalf("after answer: %d spans, want the three answered INVITE hops", n)
	}
	// The caller hangs up.
	b.hangUp()
}

func (b *bench) hangUp() {
	b.send(60000, carrier, kam, bye(a, "2", "caller"), 1)
	b.send(60002, kam, fsIn, bye(a, "2", "caller"), 1, 2)
	b.send(60004, fsIn, kam, reply("200 OK", a, "2 BYE"), 2, 1)
	b.send(60006, kam, carrier, reply("200 OK", a, "2 BYE"), 1)
	b.webhook(60100, `{"event":"participant_left","room":{"sid":"RM_1","name":"call-room"},"participant":{"sid":"PA_sip","kind":"SIP"}}`)
	b.webhook(60200, `{"event":"participant_left","room":{"sid":"RM_1","name":"call-room"},"participant":{"sid":"PA_agent","kind":"AGENT"}}`)
	b.webhook(80000, `{"event":"room_finished","room":{"sid":"RM_1","name":"call-room"}}`)
	b.sweep(130000)
}

func TestTrackerCall(t *testing.T) {
	b := newBench(t)
	playCall(t, b)
	if n := len(b.tr.calls) + len(b.tr.byCallID) + len(b.tr.byRoom); n != 0 {
		t.Errorf("state left after the call closed: %d", n)
	}

	m := b.byName()
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	want := []string{
		"kamailio/INVITE", "kamailio/INVITE#2", "kamailio/INVITE to freeswitch", "freeswitch/INVITE",
		"kamailio/BYE", "kamailio/BYE to freeswitch", "livekit/room", "livekit/sip participant",
		"livekit/agent participant", "kamailio/call",
	}
	if len(m) != len(want) {
		t.Errorf("spans %v, want %v", names, want)
	}
	for _, w := range want {
		if _, ok := m[w]; !ok {
			t.Errorf("no span %s (have %v)", w, names)
		}
	}
	trace := TraceIDFor(a)
	for _, s := range b.spans {
		if s.Trace != trace {
			t.Errorf("%s/%s in another trace", s.Service, s.Name)
		}
	}
	checkHops(t, b, m)
	checkLiveKitAndRoot(t, b, m)
}

func checkHops(t *testing.T, b *bench, m map[string]Span) {
	root := m["kamailio/call"]
	first := m["kamailio/INVITE"] // the carrier's: kamailio received it (SERVER)
	if first.Parent != root.ID || first.Kind != 2 || attr(first, "sip.status_code") != 200 {
		t.Errorf("first hop: parent %x kind %v status %v", first.Parent, first.Kind, attr(first, "sip.status_code"))
	}
	failover := m["kamailio/INVITE#2"] // to the silent node: ended when the next branch started
	if failover.Parent != first.ID || !failover.Error || !failover.End.Equal(b.at(5002)) {
		t.Errorf("failover branch: parent %x error %v end %v", failover.Parent, failover.Error, failover.End)
	}
	toFS := m["kamailio/INVITE to freeswitch"]
	if toFS.Parent != first.ID || attr(toFS, "sip.capture_points") != 2 || !toFS.Start.Equal(b.at(5002)) {
		t.Errorf("hop to FreeSWITCH: parent %x captures %v start %v", toFS.Parent, attr(toFS, "sip.capture_points"), toFS.Start)
	}
	toLK := m["freeswitch/INVITE"]
	if toLK.Parent != toFS.ID || attr(toLK, "sip.retransmissions") != 1 {
		t.Errorf("hop to LiveKit: parent %x retransmissions %v", toLK.Parent, attr(toLK, "sip.retransmissions"))
	}
	if bye := m["kamailio/BYE"]; bye.Parent != root.ID || attr(bye, "sip.reason") == nil {
		t.Errorf("BYE: parent %x reason %v", bye.Parent, attr(bye, "sip.reason"))
	}
	ack := false
	for _, e := range first.Events {
		ack = ack || e.Name == "ACK"
	}
	if !ack || first.Events[0].Name != "100 Trying" {
		t.Errorf("first hop events: %+v", first.Events)
	}
}

func checkLiveKitAndRoot(t *testing.T, b *bench, m map[string]Span) {
	root, toLK := m["kamailio/call"], m["freeswitch/INVITE"]
	rm := m["livekit/room"]
	if rm.Parent != toLK.ID || !rm.Start.Equal(b.at(5030)) || !rm.End.Equal(b.at(80000)) {
		t.Errorf("room: parent %x start %v end %v", rm.Parent, rm.Start, rm.End)
	}
	sip := m["livekit/sip participant"]
	if sip.Parent != rm.ID || !sip.Start.Equal(b.at(5035)) || attr(sip, "livekit.attr.sip.trunkID") != "ST_1" ||
		attr(sip, "livekit.attr.sip.h.x-cid") != nil {
		t.Errorf("SIP participant: %+v", sip)
	}
	if ag := m["livekit/agent participant"]; len(ag.Events) != 1 || ag.Events[0].Name != "track published" {
		t.Errorf("agent participant events: %+v", ag.Events)
	}
	if root.Parent != (SpanID{}) || !root.Start.Equal(b.at(0).Add(-rootStartLead)) || !root.End.Equal(b.at(80000)) || root.Error ||
		attr(root, "call.outcome") != "answered" || attr(root, "call.hangup") != "caller" ||
		attr(root, "call.answer_ms") != int64(6008) || attr(root, "call.agent_join_ms") != int64(5500) ||
		attr(root, "call.legs") != 2 || attr(root, "sip.to.user") != "+15550100" {
		t.Errorf("root: %+v", root)
	}
}

func TestTrackerRefusedAndUntraced(t *testing.T) {
	b := newBench(t)
	b.send(0, carrier, kam, "OPTIONS sip:edge SIP/2.0\nCall-ID: ka\nCSeq: 1 OPTIONS\nFrom: <sip:a@b>;tag=1\nTo: <sip:edge>", 1)
	b.send(1, carrier, kam, bye("unknown", "1", "x"), 1)
	b.send(2, carrier, kam, invite("busy", "1", ""), 1)
	b.send(3, kam, fsIn, invite("busy", "1", ""), 1, 2)
	b.send(10, fsIn, kam, reply("486 Busy Here", "busy", "1 INVITE"), 2, 1)
	b.send(11, kam, carrier, reply("486 Busy Here", "busy", "1 INVITE"), 1)
	b.sweep(120000)
	m := b.byName()
	root, ok := m["kamailio/call"]
	if !ok || len(b.spans) != 3 {
		t.Fatalf("spans: %v", m)
	}
	if !root.Error || attr(root, "call.outcome") != "refused" || attr(root, "sip.status_code") != 486 {
		t.Errorf("refused call root: %+v", root)
	}
	if len(b.tr.calls) != 0 {
		t.Error("the keepalive or the stray BYE started a call")
	}

	// An INVITE nobody answers is closed after SetupTimeout.
	b.spans = nil
	b.send(200000, carrier, kam, invite("silent", "1", ""), 1)
	b.sweep(200000 + 179000)
	if len(b.spans) != 0 {
		t.Fatal("closed before the setup timeout")
	}
	b.sweep(200000 + 181000)
	root = b.byName()["kamailio/call"]
	if attr(root, "call.trace_end") != "timeout" || attr(root, "call.outcome") != "no_answer" || !root.Error {
		t.Errorf("silent call root: %+v", root)
	}
}

func TestTrackerEvictsOldest(t *testing.T) {
	b := newBench(t)
	b.tr.cfg.MaxCalls = 2
	for i := range 3 {
		b.send(i, carrier, kam, invite(fmt.Sprint("c", i), "1", ""), 1)
	}
	if len(b.tr.calls) != 2 || b.tr.calls[TraceIDFor("c0")] != nil {
		t.Errorf("calls kept: %d", len(b.tr.calls))
	}
	if root := b.byName()["kamailio/call"]; attr(root, "call.trace_end") != "evicted" || attr(root, "sip.call_id") != "c0" {
		t.Errorf("evicted root: %+v", root)
	}
}

func TestLiveKitVerify(t *testing.T) {
	now := time.Unix(1700000000, 0)
	v := &LiveKitVerifier{Keys: map[string]string{"APIkey": "secret"}, Now: func() time.Time { return now }}
	body := []byte(`{"event":"room_started","room":{"name":"r","creationTime":"1700000000"},"createdAt":"1700000001"}`)
	ev, err := v.Verify(SignLiveKitWebhook("APIkey", "secret", body, now), body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != "room_started" || ev.Room.Name != "r" || ev.Room.CreationTime != 1700000000 || ev.CreatedAt != 1700000001 {
		t.Errorf("event: %+v", ev)
	}
	for name, auth := range map[string]string{
		"wrong secret": SignLiveKitWebhook("APIkey", "other", body, now),
		"unknown key":  SignLiveKitWebhook("other", "secret", body, now),
		"other body":   SignLiveKitWebhook("APIkey", "secret", []byte("{}"), now),
		"expired":      SignLiveKitWebhook("APIkey", "secret", body, now.Add(-time.Hour)),
		"none":         "",
	} {
		if _, err := v.Verify(auth, body); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	keys, err := ParseLiveKitKeys("k1: s1\nk2:s2, k3: s3")
	if err != nil || len(keys) != 3 || keys["k2"] != "s2" {
		t.Errorf("keys %v %v", keys, err)
	}
	if _, err := ParseLiveKitKeys("nosecret"); err == nil {
		t.Error("parsed a key without a secret")
	}
}

func TestExporter(t *testing.T) {
	var mu sync.Mutex
	var got []*collectorpb.ExportTraceServiceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &collectorpb.ExportTraceServiceRequest{}
		if r.Header.Get("Content-Type") != "application/x-protobuf" || proto.Unmarshal(body, req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
	}))
	defer srv.Close()
	m := NewMetrics()
	e := NewExporter(srv.URL, slogDiscard(), m)
	trace := TraceIDFor("x")
	root := Span{Trace: trace, ID: spanIDFor(trace, "call"), Service: "kamailio", Name: "call",
		Start: time.Unix(1, 0), End: time.Unix(2, 0), Attrs: []Attr{{"a", "b"}, {"n", 3}, {"empty", ""}},
		Events: []Event{{Time: time.Unix(1, 5), Name: "100 Trying"}}}
	child := Span{Trace: trace, ID: spanIDFor(trace, "x"), Parent: root.ID, Service: "livekit", Name: "room",
		Start: time.Unix(1, 0), End: time.Unix(2, 0), Error: true, Status: "boom"}
	ctx, cancel := contextWithCancel()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	e.Export([]Span{root, child})
	cancel()
	<-done
	if len(got) != 1 || len(got[0].ResourceSpans) != 2 {
		t.Fatalf("requests: %v", got)
	}
	rs := got[0].ResourceSpans[0]
	if rs.Resource.Attributes[0].Value.GetStringValue() != "kamailio" {
		t.Errorf("first resource: %v", rs.Resource)
	}
	s := rs.ScopeSpans[0].Spans[0]
	if len(s.Attributes) != 2 || len(s.Events) != 1 || len(s.ParentSpanId) != 0 || len(s.TraceId) != 16 {
		t.Errorf("root span: %v", s)
	}
	c := got[0].ResourceSpans[1].ScopeSpans[0].Spans[0]
	if string(c.ParentSpanId) != string(root.ID[:]) || c.Status.Message != "boom" {
		t.Errorf("child span: %v", c)
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
