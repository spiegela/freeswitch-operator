package runner

import (
	"context"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/fleet"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/uas"
)

// fakeFleet is a fleet whose status the test scripts. Evicting a pod marks it Draining.
type fakeFleet struct {
	mu       sync.Mutex
	status   fleet.Status
	cordoned map[string]bool
	evicted  []string
	rolls    []string
	cms      map[string]map[string]string
	// next, when set, replaces status on each read, letting a test move the fleet along.
	next  func(n int, s fleet.Status) fleet.Status
	reads int
	// external is each pod's external address annotation.
	external map[string]string
}

func newFakeFleet() *fakeFleet {
	return &fakeFleet{
		status: fleet.Status{Generation: 1, ObservedGeneration: 1, Want: 3, Ready: 3, Updated: 3, CurrentRevision: "old", UpdateRevision: "old",
			Members: []fleet.Member{
				{Pod: "edge-0", Node: "n0", Address: "127.0.0.1", State: "Active", Revision: "old"},
				{Pod: "edge-1", Node: "n1", Address: "10.0.0.1", State: "Active", Revision: "old"},
				{Pod: "edge-2", Node: "n2", Address: "10.0.0.2", State: "Active", Revision: "old"},
			}},
		cordoned: map[string]bool{},
		cms:      map[string]map[string]string{"profiles": {"trunks.xml": `<param name="sip-port" value="5060"/>`}},
	}
}

func (f *fakeFleet) Name() string { return "telephony/edge" }

func (f *fakeFleet) Status(context.Context) (fleet.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next != nil {
		f.reads++
		f.status = f.next(f.reads, f.status)
	}
	s := f.status
	s.Members = append([]fleet.Member(nil), s.Members...)
	return s, nil
}

func (f *fakeFleet) Cordon(_ context.Context, node string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cordoned[node] = on
	return nil
}

func (f *fakeFleet) Evict(_ context.Context, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evicted = append(f.evicted, pod)
	for i := range f.status.Members {
		if f.status.Members[i].Pod == pod {
			f.status.Members[i].State = "Draining"
			f.status.Ready--
		}
	}
	return nil
}

func (f *fakeFleet) Roll(_ context.Context, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rolls = append(f.rolls, v)
	return nil
}

func (f *fakeFleet) ConfigMapKey(_ context.Context, name, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cms[name][key], nil
}

func (f *fakeFleet) SetConfigMapKey(_ context.Context, name, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cms[name][key] = value
	return nil
}

func (f *fakeFleet) ExternalAddresses(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.external), nil
}

const longRoute = `
detectHz: [600, 1000]
holdMs: 10000
routes:
  long:
    steps:
      - answer: {}
      - tone: {hz: 1000, ms: 20000}
`

func withFleet(r *Runner, f fleet.Fleet) *Runner {
	r.cfg.Fleet = f
	r.cfg.Caps["fleet"] = true
	return r
}

// The drain drill in miniature: the call's pod is found from the mock's record, its node cordoned and the pod
// evicted; once the pod drains the call is hung up, keeping audio both ways throughout.
func TestTimelineDrainsTheCallsNode(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	r := withFleet(startMock(t, longRoute), f)
	res := runScenario(t, r, `
name: drain
requires: [mock, fleet]
calls:
  - id: a
    to: long
    steps:
      - tone: {hz: 600, ms: 20000}
    expect:
      status: 200
      tonesHeard: [1000]
      maxAudioGapMs: 300
      endedBy: local
      backend: {tonesHeard: [600], maxAudioGapMs: 300, endedBy: remote}
actions:
  - {name: drained, afterAnswered: a, afterMs: 300, drainNodeOf: a}
  - {waitMember: {of: a, state: Draining, timeoutMs: 2000}}
  - {afterMs: 1000, hangup: a}
  - {afterEnded: a, uncordon: {}}
`)
	if len(f.evicted) != 1 || f.evicted[0] != "edge-0" {
		t.Errorf("evicted %v, want edge-0 (the pod at the call's source)", f.evicted)
	}
	if on, seen := f.cordoned["n0"]; !seen || on {
		t.Errorf("n0 should have been cordoned, then uncordoned: %v", f.cordoned)
	}
	if len(res.Timeline) != 4 {
		t.Errorf("timeline:\n  %s", strings.Join(res.Timeline, "\n  "))
	}
	if len(res.Calls) == 1 && (res.Calls[0].Error != "" || res.Calls[0].Backend[0].Source == "") {
		t.Errorf("call: error %q, backend source %q", res.Calls[0].Error, res.Calls[0].Backend[0].Source)
	}
}

// A failing action stops the timeline, hangs up the calls rather than letting them run out, and the cleanup
// still puts the config back.
func TestTimelineFailureHangsUpAndRestores(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.next = func(_ int, s fleet.Status) fleet.Status { s.UpdateRevision = "new"; return s }
	r := withFleet(startMock(t, longRoute), f)
	s, err := scenario.Parse([]byte(`
name: broken
requires: [mock, fleet]
calls:
  - {id: a, to: long, steps: [{tone: {hz: 600, ms: 20000}}], expect: {status: 200}}
actions:
  - {afterAnswered: a, breakConfig: {configMap: profiles, key: trunks.xml, match: 'value="[0-9]+"', with: 'value="5061"'}}
  - {waitFleetCurrent: {timeoutMs: 600}}
`))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := r.Run(context.Background(), s)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s: the call should have been hung up when the timeline failed", took)
	}
	if res.Status != statusFail || !strings.Contains(strings.Join(res.Failures, "\n"), "fleet not current") {
		t.Errorf("got %s: %v", res.Status, res.Failures)
	}
	if got := f.cms["profiles"]["trunks.xml"]; got != `<param name="sip-port" value="5060"/>` {
		t.Errorf("config not restored: %s", got)
	}
}

func rollingFleet(advance bool) *fakeFleet {
	f := newFakeFleet()
	f.next = func(n int, s fleet.Status) fleet.Status {
		s.UpdateRevision, s.Updated, s.Ready = "new", 1, 2
		s.Members[2].Revision, s.Members[2].State = "new", "Starting"
		if advance && n > 3 {
			s.Updated = 2
			s.Members[1].Revision = "new"
		}
		return s
	}
	return f
}

func TestRollStuck(t *testing.T) {
	tl := newTimeline(withFleet(New(Config{Caps: map[string]bool{}}), rollingFleet(false)), &scenario.Scenario{Name: "x"}, "k", nil, time.Now())
	detail, err := tl.rollStuck(context.Background(), &scenario.RollStuck{ForMs: 1200})
	if err != nil || !strings.Contains(detail, "stuck at edge-2") {
		t.Errorf("stuck roll: %q %v", detail, err)
	}

	tl = newTimeline(withFleet(New(Config{Caps: map[string]bool{}}), rollingFleet(true)), &scenario.Scenario{Name: "x"}, "k", nil, time.Now())
	if _, err := tl.rollStuck(context.Background(), &scenario.RollStuck{ForMs: 3000}); err == nil || !strings.Contains(err.Error(), "went past its first pod") {
		t.Errorf("a roll that moves on should fail: %v", err)
	}
}

func TestCrossChecks(t *testing.T) {
	calls := []scenario.Call{
		{ID: "a"},
		{ID: "b", Expect: scenario.Expect{Backend: &scenario.EndExpect{OtherNodeThan: "a"}}},
	}
	rec := func(src string) []uas.Call { return []uas.Call{{Source: src}} }
	if out := crossChecks(calls, []CallResult{{Backend: rec("10.0.0.1:5080")}, {Backend: rec("10.0.0.2:5080")}}); len(out) != 0 {
		t.Errorf("different nodes: %v", out)
	}
	if out := crossChecks(calls, []CallResult{{Backend: rec("10.0.0.1:5080")}, {Backend: rec("10.0.0.1:5080")}}); len(out) != 1 {
		t.Errorf("same node should fail: %v", out)
	}
	if out := crossChecks(calls, []CallResult{{Backend: rec("10.0.0.1:5080")}, {}}); len(out) != 1 {
		t.Errorf("no record should fail: %v", out)
	}
}
