package runner

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/netem"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

const impairRoutes = `
detectHz: [600, 1000]
holdMs: 10000
routes:
  agent:
    steps:
      - answer: {}
      - tone: {hz: 1000, ms: 3000}
  echo:
    steps:
      - answer: {}
      - echo: {ms: 3000}
  hangup:
    steps:
      - answer: {}
      - tone: {hz: 1000, ms: 800}
      - hangup: {}
`

func mustScenario(t *testing.T, text string) *scenario.Scenario {
	t.Helper()
	s, err := scenario.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSIPLayerImpairments drops named messages in the caller's SIP layer, against the in-process mock, and checks
// the trace and the recovery.
func TestSIPLayerImpairments(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	r := startMock(t, impairRoutes)

	t.Run("200-lost", func(t *testing.T) {
		t.Parallel()
		res := runScenario(t, r, `
name: 200-lost
requires: [sip-trace]
impairments:
  - {link: carrier-edge, direction: from-edge, traffic: sip, drop: first 200 to INVITE}
calls:
  - id: a
    to: agent
    steps: [{tone: {hz: 600, ms: 1500}}, {hangup: {}}]
    expect:
      status: 200
      tonesHeard: [1000]
      endedBy: local
      sip:
        - {in: 200 INVITE, min: 2, deliveredWithinMs: 1500, oneToTag: true}
        - {out: ACK, min: 1}
      backend: {answered: true, tonesHeard: [600]}
`)
		var drops []string
		for _, e := range res.Calls[0].SIP {
			if e.Impaired != "" {
				drops = append(drops, e.Dir+" "+e.Msg+" "+e.Impaired)
			}
		}
		if !slices.Equal(drops, []string{"in 200 INVITE dropped"}) {
			t.Errorf("impaired messages %q", drops)
		}
	})
	t.Run("invite-lost", func(t *testing.T) {
		t.Parallel()
		res := runScenario(t, r, `
name: invite-lost
requires: [sip-trace]
impairments:
  - {link: carrier-edge, direction: to-edge, traffic: sip, drop: first INVITE}
calls:
  - id: a
    to: agent
    steps: [{wait: {ms: 300}}, {hangup: {}}]
    expect:
      status: 200
      maxSetupMs: 1500
      sip:
        - {out: INVITE, min: 2, max: 2, deliveredWithinMs: 700}
      backend: {calls: 1}
`)
		if ms := res.Calls[0].SetupMs; ms < 400 {
			t.Errorf("setup took %d ms; the lost INVITE should have cost about 500", ms)
		}
	})
	t.Run("bye-lost-out", func(t *testing.T) {
		t.Parallel()
		runScenario(t, r, `
name: bye-lost-out
requires: [sip-trace]
impairments:
  - {link: carrier-edge, direction: from-edge, traffic: sip, drop: first BYE}
calls:
  - id: a
    to: hangup
    maxDurationMs: 5000
    expect:
      status: 200
      endedBy: remote
      sip:
        - {in: BYE, min: 2, deliveredWithinMs: 1000}
        - {out: 200 BYE, min: 1, max: 1}
`)
	})
	t.Run("ack-held", func(t *testing.T) {
		t.Parallel()
		res := runScenario(t, r, `
name: ack-held
requires: [sip-trace]
impairments:
  - {link: carrier-edge, direction: to-edge, traffic: sip, match: ACK, delay: 300ms}
calls:
  - id: a
    to: agent
    steps: [{wait: {ms: 800}}, {hangup: {}}]
    expect: {status: 200, sip: [{out: ACK, min: 1}]}
`)
		if !slices.ContainsFunc(res.Calls[0].SIP, func(e netem.TraceEntry) bool { return e.Msg == "ACK" && e.Impaired == "delayed 300ms" }) {
			t.Errorf("no held ACK in %+v", res.Calls[0].SIP)
		}
	})
	t.Run("checks-fail", func(t *testing.T) {
		t.Parallel()
		s := mustScenario(t, `
name: checks-fail
requires: [sip-trace]
calls:
  - id: a
    to: agent
    steps: [{wait: {ms: 300}}, {hangup: {}}]
    expect:
      status: 200
      sip: [{in: 200 INVITE, min: 2}, {in: BYE, min: 1}]
`)
		res := r.Run(context.Background(), s)
		if res.Status != "fail" || len(res.Failures) != 2 {
			t.Errorf("got %s with %q, want two sip failures", res.Status, res.Failures)
		}
	})
}

// TestMeasures checks the media measurements against the in-process mock, with no impairment.
func TestMeasures(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	r := startMock(t, impairRoutes)
	res := runScenario(t, r, `
name: measures
calls:
  - id: echo
    to: echo
    steps: [{tone: {hz: 600, ms: 1500}}, {hangup: {}}]
    expect: {status: 200, tonesHeard: [600], minToneCoverage: 0.95, echoMs: {max: 200}, rtpLossPct: {max: 0}}
  - id: tone
    to: agent
    repeat: {count: 3, everyMs: 200, setupP95Ms: 1000}
    steps: [{wait: {ms: 1500}}, {hangup: {}}]
    expect: {status: 200, tonesHeard: [1000], minToneCoverage: 0.95, durationMs: {min: 1400, max: 2500}}
`)
	if len(res.Calls) != 4 || res.Calls[3].ID != "tone-3" {
		t.Fatalf("calls %d, last %q: want echo and tone-1..3", len(res.Calls), res.Calls[len(res.Calls)-1].ID)
	}
	if rtp := res.Calls[0].RTP; rtp == nil || rtp.Packets < 50 {
		t.Errorf("rtp stats %+v", rtp)
	}
}

// TestTCImpairments checks the tc commands a scenario's impairments run, and when, with tc itself faked.
func TestTCImpairments(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	r := startMock(t, impairRoutes)
	var mu sync.Mutex
	var ran []string
	start := time.Now()
	r.cfg.TCExec = func(_ context.Context, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, time.Since(start).Truncate(100*time.Millisecond).String()+" "+strings.Join(args, " "))
		return nil
	}
	r.cfg.Caps["netem"] = true
	res := runScenario(t, r, `
name: tc
requires: [netem]
impairments:
  - {link: carrier-edge, direction: to-edge, traffic: rtp, loss: 5%}
  - {link: carrier-edge, direction: both, traffic: all, loss: 100%, at: 500ms, for: 500ms}
calls:
  - id: a
    to: agent
    steps: [{wait: {ms: 1500}}, {hangup: {}}]
    expect: {status: 200}
`)
	mu.Lock()
	defer mu.Unlock()
	lo, _, err := netem.IfaceToward(r.cfg.Edge) // lo, or lo0 on macOS
	if err != nil {
		t.Fatal(err)
	}
	var nets []string
	for _, c := range ran {
		_, cmd, _ := strings.Cut(c, " ")
		if strings.HasPrefix(cmd, "tc qdisc") && strings.Contains(cmd, " parent 1:") {
			nets = append(nets, cmd)
		}
	}
	want := []string{
		"tc qdisc add dev " + lo + " parent 1:2 netem loss 5%",
		"tc qdisc add dev " + lo + " parent 1:3 netem loss 100%",
		"tc qdisc add dev sbnetem0 parent 1:3 netem loss 100%",
		"tc qdisc del dev " + lo + " parent 1:3",
		"tc qdisc del dev sbnetem0 parent 1:3",
	}
	if !slices.Equal(nets, want) {
		t.Errorf("netem commands:\n%s\nwant:\n%s", strings.Join(nets, "\n"), strings.Join(want, "\n"))
	}
	if last := ran[len(ran)-1]; !strings.HasSuffix(last, "ip link del sbnetem0") {
		t.Errorf("last command %q, want the teardown", last)
	}
	if len(res.Impairments) != 2 || !slices.ContainsFunc(res.Timeline, func(l string) bool { return strings.Contains(l, "impairment off") }) {
		t.Errorf("impairments %q, timeline %q", res.Impairments, res.Timeline)
	}
}

// An in-dialog request carries no local address (sipgo sets one only on requests it builds itself). Sent to an
// address the call's socket has not heard from, as an ACK to Kamailio's Record-Route is, it must still leave from
// the call's socket, through the SIP layer.
func TestPinnedClient(t *testing.T) {
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ua.Close() }()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	trace := netem.NewTrace()
	conn := netem.NewConn(pc, nil, trace)
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeUDP(conn) }()
	<-conn.Serving()

	far, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = far.Close() }()
	local := pc.LocalAddr().(*net.UDPAddr)
	client, err := pinnedClient(ua, slog.Default(), sip.Addr{IP: local.IP, Port: local.Port})
	if err != nil {
		t.Fatal(err)
	}

	farPort := far.LocalAddr().(*net.UDPAddr).Port
	ack := sip.NewRequest(sip.ACK, sip.Uri{Scheme: "sip", User: "agent", Host: "127.0.0.1", Port: farPort})
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.ACK})
	// With an option, as a dialog passes one, the client builds nothing itself and sets no local address.
	if err := client.WriteRequest(ack, sipgo.ClientRequestAddVia); err != nil {
		t.Fatal(err)
	}
	_ = far.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	_, from, err := far.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := from.(*net.UDPAddr).Port; got != local.Port {
		t.Errorf("ACK left from port %d, want the call's socket %d", got, local.Port)
	}
	if e := trace.Entries(); len(e) != 1 || e[0].Msg != "ACK" || e[0].Dir != netem.DirOut {
		t.Errorf("trace %+v, want one out ACK", e)
	}
}
