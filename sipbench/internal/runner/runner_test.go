package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/mock"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

// These tests point the runner straight at an in-process mock backend, with no edge in between: they check the
// runner and the mock agree on signaling, media, digits and records. The edge itself is tested by the
// integration test and the compose bench.

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func testLog(t *testing.T) *slog.Logger {
	t.Helper()
	if os.Getenv("SIPBENCH_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startMock serves cfgYAML on a loopback port and returns the runner pointed at it.
func startMock(t *testing.T, cfgYAML string) *Runner {
	t.Helper()
	cfg, err := mock.ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	log := testLog(t)
	srv := mock.NewServer(cfg, log)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	port := freeUDPPort(t)
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: port}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000}}),
		diago.WithLogger(log),
	)
	if err := dg.ServeBackground(ctx, srv.HandleCall); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	r := New(Config{
		Edge:    fmt.Sprintf("127.0.0.1:%d", port),
		MockURL: hs.URL,
		Caps:    map[string]bool{"mock": true},
		Log:     log,
		// No transfer target and no event socket: those need an edge.
		CallerBindHost: "127.0.0.1",
	})
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return r
}

const testRoutes = `
detectHz: [600, 1000]
holdMs: 10000
routes:
  agent:
    steps:
      - answer: {}
      - tone: {hz: 1000, ms: 1200}
  busy:
    steps:
      - reject: {code: 486}
  noanswer:
    steps:
      - ring: {ms: 0}
  hangup:
    steps:
      - answer: {}
      - tone: {hz: 1000, ms: 800}
      - hangup: {}
  dtmf:
    steps:
      - answer: {}
      - wait: {ms: 600}
      - digits: {digits: "4321", gapMs: 150}
`

func runScenario(t *testing.T, r *Runner, yamlText string) Result {
	t.Helper()
	s, err := scenario.Parse([]byte(yamlText))
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), s)
	if res.Status != "pass" {
		t.Errorf("%s: %s %s\n  %s", s.Name, res.Status, res.Reason, strings.Join(res.Failures, "\n  "))
	}
	return res
}

func TestDirectToMock(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	r := startMock(t, testRoutes)

	t.Run("two-way", func(t *testing.T) {
		t.Parallel()
		res := runScenario(t, r, `
name: two-way
calls:
  - id: a
    to: agent
    headers: {X-Conversation-Id: abc-123}
    steps:
      - tone: {hz: 600, ms: 1500}
      - hangup: {}
    expect:
      status: 200
      tonesHeard: [1000]
      endedBy: local
      backend:
        answered: true
        tonesHeard: [600]
        headers: {X-Conversation-Id: abc-123}
        endedBy: remote
`)
		if res.Duration <= 0 {
			t.Errorf("duration not recorded")
		}
	})
	t.Run("reject", func(t *testing.T) {
		t.Parallel()
		runScenario(t, r, `
name: reject
calls:
  - id: a
    to: busy
    expect: {status: 486, backend: {answered: false}}
`)
	})
	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		runScenario(t, r, `
name: cancel
calls:
  - id: a
    to: noanswer
    cancelAfterMs: 800
    expect: {status: 487, backend: {answered: false}}
`)
	})
	t.Run("remote-hangup", func(t *testing.T) {
		t.Parallel()
		runScenario(t, r, `
name: remote-hangup
calls:
  - id: a
    to: hangup
    maxDurationMs: 5000
    expect: {status: 200, tonesHeard: [1000], endedBy: remote, backend: {endedBy: local}}
`)
	})
	t.Run("dtmf", func(t *testing.T) {
		t.Parallel()
		runScenario(t, r, `
name: dtmf
calls:
  - id: a
    to: dtmf
    steps:
      - wait: {ms: 300}
      - digits: {digits: "1234#", gapMs: 150}
      - wait: {ms: 1500}
      - hangup: {}
    expect: {status: 200, digits: "4321", backend: {digits: "1234#"}}
`)
	})
	t.Run("skip", func(t *testing.T) {
		t.Parallel()
		s, err := scenario.Parse([]byte(`
name: needs-g722
requires: [codec:g722]
calls: [{id: a, to: agent, expect: {status: 200}}]
`))
		if err != nil {
			t.Fatal(err)
		}
		if res := r.Run(context.Background(), s); res.Status != "skip" || res.Reason != "needs codec:g722" {
			t.Errorf("got %s %q, want skip", res.Status, res.Reason)
		}
	})
}
