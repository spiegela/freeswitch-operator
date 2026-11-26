package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/fleet"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/uas"
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
	// execDenied refuses fsctl crash, as a runner without pods/exec is refused: FreeSWITCH keeps running.
	execDenied bool
	// blackouts are the nodes taken dark, and until when; deleted, the pods deleted.
	blackouts map[string]fakeBlackout
	deleted   []string
	// restarts counts each pod's FreeSWITCH restarts (an fsctl crash adds one); execs are the commands run, as
	// "pod: command"; recoverOut is what sofia recover answers.
	restarts   map[string]int32
	execs      []string
	recoverOut string
	// spare is the node a pod deleted at once comes back on, Active; moveAddress, when set, is the external
	// address it comes back with.
	spare       string
	moveAddress string
	// unfenced are the nodes unfenced.
	unfenced []string
}

type fakeBlackout struct {
	node  string
	until time.Time
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

// StartBlackout takes the node dark at once: the pod reports having started long enough ago that the runner
// counts the node dark straight away.
func (f *fakeFleet) StartBlackout(_ context.Context, node string, d time.Duration, image string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if image == "" {
		return "", errors.New("no image")
	}
	if f.blackouts == nil {
		f.blackouts = map[string]fakeBlackout{}
	}
	pod := fmt.Sprintf("sipbench-blackout-%d", len(f.blackouts))
	f.blackouts[pod] = fakeBlackout{node: node, until: time.Now().Add(d)}
	return pod, nil
}

func (f *fakeFleet) Blackout(_ context.Context, pod string) (fleet.BlackoutState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blackouts[pod]
	if !ok {
		return fleet.BlackoutState{}, fmt.Errorf("no pod %s", pod)
	}
	st := fleet.BlackoutState{Phase: "Running", StartedAt: time.Now().Add(-fleet.BlackoutDelay - 2*time.Second)}
	if time.Now().After(b.until) {
		st.Phase = "Succeeded"
	}
	return st, nil
}

func (f *fakeFleet) Unfence(_ context.Context, node string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unfenced = append(f.unfenced, node)
	return false, nil
}

func (f *fakeFleet) NodeReady(_ context.Context, node string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.blackouts {
		if b.node == node && time.Now().Before(b.until) {
			return false, nil
		}
	}
	return true, nil
}

func (f *fakeFleet) PodLog(_ context.Context, pod string) (string, error) {
	return "dark eth0\nrestored eth0\n", nil
}

func (f *fakeFleet) DeletePod(_ context.Context, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, pod)
	return nil
}

func (f *fakeFleet) DeletePodNow(_ context.Context, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, pod+" (now)")
	for i, m := range f.status.Members {
		if m.Pod == pod && f.spare != "" {
			f.status.Members[i].Node, f.status.Members[i].State, f.status.Members[i].Address = f.spare, "Active", "10.0.0.9"
			if f.moveAddress != "" {
				f.external[pod] = f.moveAddress
			}
		}
	}
	return nil
}

func (f *fakeFleet) Exec(_ context.Context, pod, container string, cmd ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(cmd, " ")
	f.execs = append(f.execs, pod+": "+line)
	switch {
	case container != fleet.FreeswitchContainer:
		return "", fmt.Errorf("no container %s", container)
	case line == "fs_cli -x fsctl crash":
		if f.execDenied {
			return "", errors.New(`pods "` + pod + `" is forbidden: cannot create resource "pods/exec"`)
		}
		if f.restarts == nil {
			f.restarts = map[string]int32{}
		}
		f.restarts[pod]++
		return "", errors.New("command terminated with exit code 1")
	case line == "fs_cli -x sofia recover":
		return f.recoverOut, nil
	case line == "fs_cli -x sofia recover flush":
		return "Flushing recovery database.\n", nil
	}
	return "", fmt.Errorf("unexpected command %q", line)
}

func (f *fakeFleet) Container(_ context.Context, pod, _ string) (fleet.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fleet.ContainerState{Restarts: f.restarts[pod], Ready: true, Running: true}, nil
}

func (f *fakeFleet) TracksCalls(context.Context) (bool, error) { return true, nil }

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
	if tracks, _ := f.TracksCalls(context.Background()); tracks {
		r.cfg.Caps[scenario.CallTrackingCapability] = true
	}
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

// A node taken dark: the call's node goes NotReady, comes back Ready once the blackout ends, and the blackout pod
// is seen to finish, its log noted, and deleted.
func TestTimelineBlacksOutTheCallsNode(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	// Once the node is dark, the operator fails edge-0 over to the spare, n3.
	f.next = func(_ int, s fleet.Status) fleet.Status {
		if len(f.blackouts) > 0 {
			s.Members[0].Node = "n3"
		}
		return s
	}
	r := withFleet(startMock(t, longRoute), f)
	r.cfg.BlackoutImage = "sipbench:test"
	r.cfg.Caps[scenario.BlackoutCapability] = true
	res := runScenario(t, r, `
name: dark
requires: [mock, fleet, node-blackout]
calls:
  - id: a
    to: long
    steps:
      - tone: {hz: 600, ms: 20000}
    expect: {status: 200}
actions:
  - {afterAnswered: a, afterMs: 300, blackoutNodeOf: {of: a, forMs: 1500}}
  - {waitNode: {of: a, state: NotReady, timeoutMs: 1000}}
  - {waitFailover: {of: a, timeoutMs: 1000}}
  - {waitNode: {of: a, state: Ready, timeoutMs: 3000}}
  - {hangup: a}
`)
	if len(f.unfenced) != 1 || f.unfenced[0] != "n0" {
		t.Errorf("unfenced %v, want the dark node n0 once it was back", f.unfenced)
	}
	if len(f.blackouts) != 1 || f.blackouts["sipbench-blackout-0"].node != "n0" {
		t.Errorf("blackouts %v, want one on n0 (the node at the call's source)", f.blackouts)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "sipbench-blackout-0" {
		t.Errorf("deleted %v, want the blackout pod", f.deleted)
	}
	tl := strings.Join(res.Timeline, "\n  ")
	for _, want := range []string{"n0 is dark for 1.5s", "node n0 (call a) is NotReady", "failed over from n0 to n3", "node n0 (call a) is Ready",
		"blackout sipbench-blackout-0 on n0: restored eth0"} {
		if !strings.Contains(tl, want) {
			t.Errorf("timeline lacks %q:\n  %s", want, tl)
		}
	}
	if res.Status == statusFail {
		t.Errorf("failed: %v", res.Failures)
	}
}

// Without an image to run, a blackout fails the timeline rather than skipping silently.
func TestTimelineBlackoutNeedsAnImage(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	r := withFleet(startMock(t, longRoute), f)
	r.cfg.Caps[scenario.BlackoutCapability] = true
	s, err := scenario.Parse([]byte(`
name: dark
requires: [mock, fleet, node-blackout]
calls:
  - {id: a, to: long, steps: [{tone: {hz: 600, ms: 20000}}], expect: {status: 200}}
actions:
  - {afterAnswered: a, blackoutNodeOf: {of: a, forMs: 1000}}
`))
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), s)
	if res.Status != statusFail || !strings.Contains(strings.Join(res.Failures, "\n"), "-blackout-image") {
		t.Errorf("got %s: %v", res.Status, res.Failures)
	}
}

func TestParseRecovered(t *testing.T) {
	for out, want := range map[string]int{
		"Recovered 2 call(s)\n\n":  2,
		"Recovered 12 call(s)\n":   12,
		"No calls to recover.\n\n": 0,
	} {
		if got, err := parseRecovered(out); err != nil || got != want {
			t.Errorf("parseRecovered(%q) = %d, %v; want %d", out, got, err, want)
		}
	}
	if _, err := parseRecovered("-ERR no reply\n"); err == nil {
		t.Error("parseRecovered took an error for an answer")
	}
}

const crashRecoverScenario = `
name: crash
requires: [mock, fleet, call-tracking]
calls:
  - id: a
    to: long
    steps:
      - tone: {hz: 600, ms: 20000}
    expect: {status: 200}
actions:
  - {afterAnswered: a, afterMs: 300, crashFreeswitchOf: a}
  - {recoverOn: a}
  - {hangup: a}
`

// A crash restarts the call's FreeSWITCH; a recover there that takes its one call's two legs passes.
func TestTimelineCrashesAndRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.external = map[string]string{"edge-0": "192.0.2.10"}
	f.recoverOut = "Recovered 2 call(s)\n"
	r := withFleet(startMock(t, longRoute), f)
	res := runScenario(t, r, crashRecoverScenario)
	want := []string{"edge-0: fs_cli -x fsctl crash", "edge-0: fs_cli -x sofia recover"}
	if fmt.Sprint(f.execs) != fmt.Sprint(want) {
		t.Errorf("ran %q, want %q", f.execs, want)
	}
	tl := strings.Join(res.Timeline, "\n  ")
	for _, s := range []string{"crashed its FreeSWITCH, which restarted (restart 1)", "took 2 call legs: call(s) a"} {
		if !strings.Contains(tl, s) {
			t.Errorf("timeline lacks %q:\n  %s", s, tl)
		}
	}
	if res.Status == statusFail {
		t.Errorf("failed: %v", res.Failures)
	}
}

// A recover that takes more than its own node's calls (other nodes' too) fails the scenario.
func TestTimelineRecoverTakingOtherNodesCallsFails(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.external = map[string]string{"edge-0": "192.0.2.10"}
	f.recoverOut = "Recovered 6 call(s)\n"
	r := withFleet(startMock(t, longRoute), f)
	s, err := scenario.Parse([]byte(crashRecoverScenario))
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), s)
	if res.Status != statusFail || !strings.Contains(strings.Join(res.Failures, "\n"), "took 6 call legs, want 2") {
		t.Errorf("got %s: %v", res.Status, res.Failures)
	}
}

const moveScenario = `
name: move
requires: [mock, fleet, spare-node, call-tracking]
calls:
  - id: a
    to: long
    steps:
      - tone: {hz: 600, ms: 20000}
    expect: {status: 200}
  - id: b
    to: long
    steps:
      - tone: {hz: 600, ms: 20000}
    expect: {status: 200}
actions:
  - {afterAnswered: b, afterMs: 300, moveFreeswitchOf: a}
  - {recoverOn: a}
  - {hangup: a}
  - {hangup: b}
`

// A move cordons the call's node, crashes FreeSWITCH, deletes the pod at once, and waits for the replacement to
// take calls on the spare node with the same address; the recover then runs in the replacement.
func TestTimelineMovesToASpareAndRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.recoverOut = "Recovered 4 call(s)\n"
	f.spare = "n3"
	f.external = map[string]string{"edge-0": "192.0.2.10"}
	r := withFleet(startMock(t, longRoute), f)
	r.cfg.Caps[scenario.SpareNodeCapability] = true
	res := runScenario(t, r, moveScenario)
	want := []string{"edge-0: fs_cli -x fsctl crash", "edge-0: fs_cli -x sofia recover"}
	if fmt.Sprint(f.execs) != fmt.Sprint(want) {
		t.Errorf("ran %q, want %q", f.execs, want)
	}
	if fmt.Sprint(f.deleted) != "[edge-0 (now)]" {
		t.Errorf("deleted %q, want edge-0 at once", f.deleted)
	}
	if f.cordoned["n0"] {
		t.Error("n0 left cordoned")
	}
	tl := strings.Join(res.Timeline, "\n  ")
	for _, s := range []string{"cordoned it, crashed FreeSWITCH and deleted the pod; its replacement takes calls on n3, with the same external address \"192.0.2.10\"",
		"took 4 call legs: call(s) a, b"} {
		if !strings.Contains(tl, s) {
			t.Errorf("timeline lacks %q:\n  %s", s, tl)
		}
	}
	if res.Status == statusFail {
		t.Errorf("failed: %v", res.Failures)
	}
}

// A replacement with another address would run other profiles and recover nothing of the moved pod's: the move
// fails.
func TestTimelineMoveToAnotherAddressFails(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.spare, f.moveAddress = "n3", "192.0.2.11"
	f.external = map[string]string{"edge-0": "192.0.2.10"}
	r := withFleet(startMock(t, longRoute), f)
	r.cfg.Caps[scenario.SpareNodeCapability] = true
	s, err := scenario.Parse([]byte(moveScenario))
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), s)
	if res.Status != statusFail || !strings.Contains(strings.Join(res.Failures, "\n"), `holds "192.0.2.11", not "192.0.2.10"`) {
		t.Errorf("got %s: %v", res.Status, res.Failures)
	}
	// The crashed FreeSWITCH's rows are under the old address's profiles, which a flush in the replacement can't
	// reach: none is run, and the timeline says the rows stay.
	if want := []string{"edge-0: fs_cli -x fsctl crash"}; fmt.Sprint(f.execs) != fmt.Sprint(want) {
		t.Errorf("ran %q, want %q", f.execs, want)
	}
	if tl := strings.Join(res.Timeline, "\n"); !strings.Contains(tl, `cleanup: edge-0's calls stay in the recovery table under profiles named for 192.0.2.10`) {
		t.Errorf("timeline:\n%s", tl)
	}
}

// A move that isn't followed by a recover leaves the crashed FreeSWITCH's calls in the table: cleanup flushes them
// in the replacement, which runs the same profiles.
func TestTimelineMoveWithoutRecoverFlushes(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.spare = "n3"
	f.external = map[string]string{"edge-0": "192.0.2.10"}
	r := withFleet(startMock(t, longRoute), f)
	r.cfg.Caps[scenario.SpareNodeCapability] = true
	runScenario(t, r, strings.Replace(moveScenario, "  - {recoverOn: a}\n", "", 1))
	if want := []string{"edge-0: fs_cli -x fsctl crash", "edge-0: fs_cli -x sofia recover flush"}; fmt.Sprint(f.execs) != fmt.Sprint(want) {
		t.Errorf("ran %q, want %q", f.execs, want)
	}
}

// A crash that never happens (the runner may not exec) says why, and a move then deletes nothing: deleting the pod
// would stop FreeSWITCH gracefully and hang its calls up.
func TestTimelineCrashThatDoesntHappenFails(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	defer func(d time.Duration) { crashSeen = d }(crashSeen)
	crashSeen = time.Second
	for _, tc := range []struct{ name, scenario string }{{"crash", crashRecoverScenario}, {"move", moveScenario}} {
		f := newFakeFleet()
		f.spare, f.execDenied = "n3", true
		f.external = map[string]string{"edge-0": "192.0.2.10"}
		r := withFleet(startMock(t, longRoute), f)
		r.cfg.Caps[scenario.SpareNodeCapability] = true
		s, err := scenario.Parse([]byte(tc.scenario))
		if err != nil {
			t.Fatal(err)
		}
		res := r.Run(context.Background(), s)
		if got := strings.Join(res.Failures, "\n"); res.Status != statusFail || !strings.Contains(got, "didn't crash") || !strings.Contains(got, "forbidden") {
			t.Errorf("%s: got %s: %v", tc.name, res.Status, res.Failures)
		}
		if len(f.deleted) != 0 || slices.ContainsFunc(f.execs, func(e string) bool { return strings.Contains(e, "flush") }) {
			t.Errorf("%s: acted on a crash that didn't happen: deleted %q, ran %q", tc.name, f.deleted, f.execs)
		}
	}
}

// A recover whose answer can't be read isn't known to have run: cleanup still flushes the pod's rows.
func TestTimelineUnreadableRecoverStillFlushes(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.recoverOut = "-ERR no reply\n"
	f.external = map[string]string{"edge-0": "192.0.2.10"}
	r := withFleet(startMock(t, longRoute), f)
	s, err := scenario.Parse([]byte(crashRecoverScenario))
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), s)
	if res.Status != statusFail {
		t.Errorf("got %s", res.Status)
	}
	if want := "edge-0: fs_cli -x sofia recover flush"; !slices.Contains(f.execs, want) {
		t.Errorf("ran %q, want %q among them", f.execs, want)
	}
}

// A pod with no external address can't be moved: there would be nothing to check its replacement against.
func TestTimelineMoveWithoutAnAddressFails(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.spare = "n3"
	r := withFleet(startMock(t, longRoute), f)
	r.cfg.Caps[scenario.SpareNodeCapability] = true
	s, err := scenario.Parse([]byte(moveScenario))
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), s)
	if res.Status != statusFail || !strings.Contains(strings.Join(res.Failures, "\n"), "has no external address") {
		t.Errorf("got %s: %v", res.Status, res.Failures)
	}
	if len(f.execs) != 0 || len(f.deleted) != 0 {
		t.Errorf("acted anyway: ran %q, deleted %q", f.execs, f.deleted)
	}
}
