package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/spiegela/freeswitch-operator/internal/calltrace"
)

// TestRun plays a call into a running tracer, over UDP and HTTP as the edge would, and checks the trace it
// exports. With CALL_TRACER_TEST_OTLP set (say http://localhost:10428/insert/opentelemetry/v1/traces, a
// VictoriaTraces), it exports there instead, for a look in a real trace store.
func TestRun(t *testing.T) {
	var mu sync.Mutex
	var spans []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &collectorpb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					spans = append(spans, rs.Resource.Attributes[0].Value.GetStringValue()+"/"+s.Name)
				}
			}
		}
	}))
	defer srv.Close()
	otlp := srv.URL
	if v := os.Getenv("CALL_TRACER_TEST_OTLP"); v != "" {
		otlp = v
	}
	hepAddr, httpAddr := freeAddr(t, "udp"), freeAddr(t, "tcp")
	t.Setenv("LIVEKIT_KEYS", "APItest: testsecret")
	done := make(chan error, 1)
	go func() {
		done <- run(slog.New(slog.DiscardHandler), hepAddr, httpAddr, otlp, "1=kamailio,2=freeswitch", "X-CID",
			time.Second, time.Minute, time.Hour, 100)
	}()
	waitFor(t, func() bool {
		r, err := http.Get("http://" + httpAddr + "/healthz")
		return err == nil && r.StatusCode == 200
	})

	conn, err := net.Dial("udp", hepAddr)
	if err != nil {
		t.Fatal(err)
	}
	callID := fmt.Sprintf("test-%d@carrier", time.Now().UnixNano())
	leg := "leg-" + callID
	start := time.Now()
	carrier, kam := netip.MustParseAddrPort("198.51.100.7:5060"), netip.MustParseAddrPort("10.0.1.5:5060")
	fsIn, fsOut, lk := netip.MustParseAddrPort("10.0.1.5:5062"), netip.MustParseAddrPort("10.0.1.5:5080"),
		netip.MustParseAddrPort("10.96.4.4:5060")
	sip := func(ms int, src, dst netip.AddrPort, msg string, ids ...uint32) {
		for _, id := range ids {
			p := calltrace.Packet{Time: start.Add(time.Duration(ms) * time.Millisecond), Src: src, Dst: dst,
				Transport: 17, PayloadType: 1, CaptureID: id,
				Payload: []byte(strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n\r\n")}
			if _, err := conn.Write(calltrace.EncodeHEP(p)); err != nil {
				t.Fatal(err)
			}
		}
	}
	hook := func(body string, signed ...string) int {
		if len(signed) == 0 {
			signed = []string{body}
		}
		req, _ := http.NewRequest(http.MethodPost, "http://"+httpAddr+"/livekit", bytes.NewBufferString(body))
		req.Header.Set("Authorization", calltrace.SignLiveKitWebhook("APItest", "testsecret", []byte(signed[0]), time.Now()))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	inv := func(id, cseq, extra string) string {
		return "INVITE sip:+15550100@edge SIP/2.0\nFrom: <sip:+15550199@carrier>;tag=caller\nTo: <sip:+15550100@edge>" +
			"\nCall-ID: " + id + "\nCSeq: " + cseq + " INVITE" + extra
	}
	ok := func(id, cseq string) string {
		return "SIP/2.0 200 OK\nFrom: <sip:a@b>;tag=caller\nTo: <sip:c@d>;tag=callee\nCall-ID: " + id + "\nCSeq: " + cseq
	}
	sip(0, carrier, kam, inv(callID, "1", ""), 1)
	sip(2, kam, fsIn, inv(callID, "1", ""), 1, 2)
	sip(10, fsOut, lk, inv(leg, "1", "\nX-CID: "+callID), 2)
	time.Sleep(50 * time.Millisecond) // the INVITEs before the webhook, as on a real edge
	room := `"room":{"sid":"RM_t","name":"room-` + callID + `"}`
	joined := `{"event":"participant_joined",` + room + `,"participant":{"sid":"PA_1","identity":"caller",` +
		`"kind":"SIP","attributes":{"sip.callIDFull":"` + leg + `"}}}`
	if code := hook(joined); code != 200 {
		t.Fatalf("webhook: %d", code)
	}
	if code := hook(`{"event":"room_finished",`+room+`}`, `{}`); code != http.StatusUnauthorized {
		t.Errorf("a body that doesn't match its signature: %d, want 401", code)
	}
	sip(900, lk, fsOut, ok(leg, "1 INVITE"), 2)
	sip(905, fsIn, kam, ok(callID, "1 INVITE"), 2, 1)
	sip(910, kam, carrier, ok(callID, "1 INVITE"), 1)
	byeMsg := "BYE sip:x SIP/2.0\nFrom: <sip:a@b>;tag=caller\nTo: <sip:c@d>;tag=callee\nCall-ID: " + callID +
		"\nCSeq: 2 BYE"
	sip(1500, carrier, kam, byeMsg, 1)
	sip(1510, kam, carrier, ok(callID, "2 BYE"), 1)
	time.Sleep(100 * time.Millisecond)

	if r, err := http.Get("http://" + httpAddr + "/metrics"); err != nil || r.StatusCode != 200 {
		t.Fatalf("metrics: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil { // as Kubernetes stops it: flushes the call
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Logf("trace %x", calltrace.TraceIDFor(callID))
	if otlp != srv.URL {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	got := strings.Join(spans, " ")
	for _, want := range []string{"kamailio/call", "kamailio/INVITE", "kamailio/INVITE to freeswitch",
		"freeswitch/INVITE", "kamailio/BYE", "livekit/room", "livekit/sip participant"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("no span %s in %s", want, got)
		}
	}
}

func freeAddr(t *testing.T, network string) string {
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		return c.LocalAddr().String()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func waitFor(t *testing.T, f func() bool) {
	for range 100 {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}
