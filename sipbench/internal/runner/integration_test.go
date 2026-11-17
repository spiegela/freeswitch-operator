//go:build integration

package runner

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

// TestScenarios runs every scenario in scenarios/ against a live edge, one subtest each. It must run where the
// edge can reach it back as SIPBENCH_TARGET for transfers; the compose file's integration service does that:
//
//	docker compose up -d --build
//	docker compose run --rm integration
func TestScenarios(t *testing.T) {
	edge := os.Getenv("SIPBENCH_EDGE")
	if edge == "" {
		t.Skip("SIPBENCH_EDGE is not set")
	}
	caps := map[string]bool{}
	for _, c := range strings.Split(envOr("SIPBENCH_CAPS", "mock,esl,codec:pcmu,codec:pcma"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			caps[c] = true
		}
	}
	r := New(Config{
		Edge:         edge,
		MockURL:      os.Getenv("SIPBENCH_MOCK"),
		ESL:          os.Getenv("SIPBENCH_ESL"),
		ESLPassword:  envOr("SIPBENCH_ESL_PASSWORD", "ClueCon"),
		TargetListen: envOr("SIPBENCH_TARGET", "0.0.0.0:5090"),
		Caps:         caps,
		Log:          testLog(t),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.WaitReady(ctx, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	ss, err := scenario.LoadDir("../../scenarios")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		// Scenarios share the mock and the edge, so they run one at a time.
		t.Run(s.Name, func(t *testing.T) {
			res := r.Run(ctx, s)
			switch res.Status {
			case "skip":
				t.Skip(res.Reason)
			case "pass":
			default:
				t.Errorf("%s %s\n  %s", res.Status, res.Reason, strings.Join(res.Failures, "\n  "))
			}
		})
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
