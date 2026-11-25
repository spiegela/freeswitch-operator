package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/fleet"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
)

// Fleet waits poll this often, and give up after these defaults.
const (
	pollEvery          = 500 * time.Millisecond
	defaultFleetWait   = 300 * time.Second
	defaultRollStarted = 120 * time.Second
	cleanupTimeout     = 60 * time.Second
	// A blackout pod must be running within this, and seen to finish within blackoutGrace of its end.
	blackoutStart = 60 * time.Second
	blackoutGrace = 3 * time.Minute
	// A crashed FreeSWITCH must be restarted and ready again within this.
	crashRestart = 2 * time.Minute
	// Commands run in a FreeSWITCH container (fs_cli) must answer within this.
	execTimeout = 30 * time.Second
	// moveWait bounds moveFreeswitchOf's wait for the replacement pod: scheduling, the address agent binding its
	// address, FreeSWITCH starting.
	moveWait = 3 * time.Minute
)

// callState is what the timeline can see of, and do to, one call while it runs.
type callState struct {
	bench    string
	answered chan struct{}
	ended    chan struct{}
	stop     chan struct{}

	answerOnce, endOnce, stopOnce sync.Once
}

func newCallState(bench string) *callState {
	return &callState{bench: bench, answered: make(chan struct{}), ended: make(chan struct{}), stop: make(chan struct{})}
}

func (c *callState) answer() { c.answerOnce.Do(func() { close(c.answered) }) }
func (c *callState) end()    { c.endOnce.Do(func() { close(c.ended) }) }
func (c *callState) hangUp() { c.stopOnce.Do(func() { close(c.stop) }) }

// timeline runs a scenario's actions, one after another, and undoes what they leave behind.
type timeline struct {
	r     *Runner
	s     *scenario.Scenario
	key   string
	calls map[string]*callState
	start time.Time

	// done closes per named action as it finishes; aborted closes when the timeline fails, so calls waiting on
	// an action give up.
	done    map[string]chan struct{}
	aborted chan struct{}

	// What to undo: nodes cordoned, each edited ConfigMap key's original value, and blackout pods to see finish.
	cordoned  []string
	broken    []brokenKey
	blackouts []blackoutPod
	// pods remembers which pod carried each call, once looked up.
	pods map[string]fleet.Member
	// unrecovered are the pods whose FreeSWITCH this run crashed and hasn't run sofia recover in since. Their calls
	// stay in the shared recovery table, where a later run's recover would count them.
	unrecovered []crashedPod

	mu       sync.Mutex
	log      []string
	failures []string
}

type brokenKey struct{ configMap, key, original string }

// crashedPod is a pod whose FreeSWITCH crashed, and the external address its SIP profiles were named for then
// ("" when unknown).
type crashedPod struct{ pod, address string }

// blackoutPod is a node taken dark, until about the time until.
type blackoutPod struct {
	pod, node string
	until     time.Time
}

func newTimeline(r *Runner, s *scenario.Scenario, key string, calls map[string]*callState, start time.Time) *timeline {
	t := &timeline{
		r: r, s: s, key: key, calls: calls, start: start,
		done: map[string]chan struct{}{}, aborted: make(chan struct{}), pods: map[string]fleet.Member{},
	}
	for _, a := range s.Actions {
		if a.Name != "" {
			t.done[a.Name] = make(chan struct{})
		}
	}
	return t
}

func (t *timeline) note(format string, args ...any) {
	line := fmt.Sprintf("+%.1fs ", time.Since(t.start).Seconds()) + fmt.Sprintf(format, args...)
	t.r.log.Info("timeline", "scenario", t.s.Name, "event", line)
	t.mu.Lock()
	t.log = append(t.log, line)
	t.mu.Unlock()
}

func (t *timeline) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	t.note("FAILED %s", msg)
	t.mu.Lock()
	t.failures = append(t.failures, msg)
	t.mu.Unlock()
}

// waitFor blocks until the named action finishes, the timeline fails, or ctx ends.
func (t *timeline) waitFor(ctx context.Context, name string) error {
	select {
	case <-t.done[name]:
		return nil
	case <-t.aborted:
		return fmt.Errorf("the timeline failed before action %q", name)
	case <-ctx.Done():
		return fmt.Errorf("action %q: %w", name, ctx.Err())
	}
}

// run runs every action in order. On a failure it stops, and hangs up every call so the scenario ends.
func (t *timeline) run(ctx context.Context) {
	for i := range t.s.Actions {
		a := &t.s.Actions[i]
		label := a.Kind()
		if a.Name != "" {
			label = a.Name + " (" + label + ")"
		}
		if err := t.trigger(ctx, a); err != nil {
			t.fail("action %s: %v", label, err)
			t.abort()
			return
		}
		detail, err := t.do(ctx, a)
		if err != nil {
			t.fail("action %s: %v", label, err)
			t.abort()
			return
		}
		t.note("%s: %s", label, detail)
		if a.Name != "" {
			close(t.done[a.Name])
		}
	}
}

func (t *timeline) abort() {
	close(t.aborted)
	for _, c := range t.calls {
		c.hangUp()
	}
}

func (t *timeline) trigger(ctx context.Context, a *scenario.Action) error {
	if a.AfterAnswered != "" {
		c := t.calls[a.AfterAnswered]
		select {
		case <-c.answered:
		case <-c.ended:
			return fmt.Errorf("call %s ended without being answered", a.AfterAnswered)
		case <-ctx.Done():
			return fmt.Errorf("waiting for call %s to be answered: %w", a.AfterAnswered, ctx.Err())
		}
	}
	if a.AfterEnded != "" {
		select {
		case <-t.calls[a.AfterEnded].ended:
		case <-ctx.Done():
			return fmt.Errorf("waiting for call %s to end: %w", a.AfterEnded, ctx.Err())
		}
	}
	if a.AfterMs > 0 {
		select {
		case <-time.After(time.Duration(a.AfterMs) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (t *timeline) do(ctx context.Context, a *scenario.Action) (string, error) { //nolint:gocyclo // one case per action
	f := t.r.cfg.Fleet
	switch {
	case a.Hangup != "":
		t.calls[a.Hangup].hangUp()
		return "hung up call " + a.Hangup, nil

	case a.DrainNodeOf != "":
		m, err := t.memberOf(ctx, a.DrainNodeOf)
		if err != nil {
			return "", err
		}
		if err := f.Cordon(ctx, m.Node, true); err != nil {
			return "", fmt.Errorf("cordoning %s: %w", m.Node, err)
		}
		t.cordoned = append(t.cordoned, m.Node)
		ectx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := f.Evict(ectx, m.Pod); err != nil {
			return "", err
		}
		return fmt.Sprintf("call %s is on %s; cordoned %s and evicted %s", a.DrainNodeOf, m.Pod, m.Node, m.Pod), nil

	case a.Uncordon != nil:
		nodes, err := t.uncordon(ctx)
		return "uncordoned " + orNone(nodes), err

	case a.RollFleet != nil:
		if err := f.Roll(ctx, t.key); err != nil {
			return "", err
		}
		return fmt.Sprintf("set %s=%s on %s", fleet.RollEnv, t.key, f.Name()), nil

	case a.BreakConfig != nil:
		b := a.BreakConfig
		orig, err := f.ConfigMapKey(ctx, b.ConfigMap, b.Key)
		if err != nil {
			return "", err
		}
		broken, err := b.Apply(orig)
		if err != nil {
			return "", fmt.Errorf("ConfigMap %s key %s: %w", b.ConfigMap, b.Key, err)
		}
		if err := f.SetConfigMapKey(ctx, b.ConfigMap, b.Key, broken); err != nil {
			return "", err
		}
		t.broken = append(t.broken, brokenKey{configMap: b.ConfigMap, key: b.Key, original: orig})
		return fmt.Sprintf("ConfigMap %s key %s: %q -> %q", b.ConfigMap, b.Key, b.Target(), b.With), nil

	case a.RestoreConfig != nil:
		keys, err := t.restore(ctx)
		return "restored " + orNone(keys), err

	case a.WaitMember != nil:
		return t.waitMember(ctx, a.WaitMember)

	case a.WaitFleetCurrent != nil:
		var last fleet.Status
		err := t.poll(ctx, msOr(a.WaitFleetCurrent.TimeoutMs, defaultFleetWait), func(s fleet.Status) (bool, error) {
			last = s
			return s.Current(), nil
		})
		if err != nil {
			return "", fmt.Errorf("fleet not current: %w (%s)", err, describe(last))
		}
		return "fleet current: " + describe(last), nil

	case a.ExpectRollStuck != nil:
		return t.rollStuck(ctx, a.ExpectRollStuck)

	case a.BlackoutNodeOf != nil:
		return t.blackout(ctx, a.BlackoutNodeOf)

	case a.WaitNode != nil:
		return t.waitNode(ctx, a.WaitNode)

	case a.CrashFreeswitchOf != "":
		return t.crashFreeswitch(ctx, a.CrashFreeswitchOf)

	case a.RecoverOn != "":
		return t.recoverOn(ctx, a.RecoverOn)

	case a.MoveFreeswitchOf != "":
		return t.moveFreeswitch(ctx, a.MoveFreeswitchOf)
	}
	return "", errors.New("no action")
}

// crashFreeswitch crashes FreeSWITCH in the pod that carries a call and waits for the kubelet to restart its
// container and for it to be ready again.
func (t *timeline) crashFreeswitch(ctx context.Context, call string) (string, error) {
	f := t.r.cfg.Fleet
	m, err := t.memberOf(ctx, call)
	if err != nil {
		return "", err
	}
	addr, err := t.addressOf(ctx, m.Pod, call)
	if err != nil {
		return "", err
	}
	before, err := t.crash(ctx, m.Pod, addr)
	if err != nil {
		return "", fmt.Errorf("call %s: %w", call, err)
	}
	deadline := time.Now().Add(crashRestart)
	var now fleet.ContainerState
	for {
		now, err = f.Container(ctx, m.Pod, fleet.FreeswitchContainer)
		if err == nil && now.Restarts > before.Restarts && now.Ready {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("FreeSWITCH in %s (call %s) not restarted and ready within %s (restarts %d -> %d, ready %t, err %v)",
				m.Pod, call, crashRestart, before.Restarts, now.Restarts, now.Ready, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollEvery):
		}
	}
	return fmt.Sprintf("call %s is on %s; crashed its FreeSWITCH, which restarted (restart %d) and is ready", call, m.Pod,
		now.Restarts), nil
}

// moveFreeswitch moves the pod that carries a call to a spare edge node, the way a lost node's pod would move:
// cordon the node, crash FreeSWITCH (fsctl crash: no drain, no BYE, its calls left in the recovery table), and
// delete the pod at once. Deleting it alone would stop FreeSWITCH gracefully, which hangs every call up. It waits
// for the StatefulSet's replacement to take calls on another node, and checks it holds the moved pod's external
// address: its SIP profiles are named for it, and recovery takes only the calls of profiles it runs.
func (t *timeline) moveFreeswitch(ctx context.Context, call string) (string, error) {
	f := t.r.cfg.Fleet
	m, err := t.memberOf(ctx, call)
	if err != nil {
		return "", err
	}
	// Find every live call's pod now: a call is traced to its pod by the node address it came from, which the
	// moved pod leaves behind.
	for _, id := range slices.Sorted(maps.Keys(t.calls)) {
		if c := t.calls[id]; closed(c.answered) && !closed(c.ended) {
			if _, err := t.memberOf(ctx, id); err != nil {
				return "", err
			}
		}
	}
	addr, err := t.addressOf(ctx, m.Pod, call)
	if err != nil {
		return "", err
	}
	if err := f.Cordon(ctx, m.Node, true); err != nil {
		return "", fmt.Errorf("cordoning %s: %w", m.Node, err)
	}
	t.cordoned = append(t.cordoned, m.Node)
	// Only once FreeSWITCH is seen to have died: deleting a pod whose FreeSWITCH still runs stops it gracefully,
	// which hangs every call up.
	if _, err := t.crash(ctx, m.Pod, addr); err != nil {
		return "", fmt.Errorf("call %s: %w", call, err)
	}
	if err := f.DeletePodNow(ctx, m.Pod); err != nil {
		return "", fmt.Errorf("deleting %s: %w", m.Pod, err)
	}
	var now fleet.Member
	err = t.poll(ctx, moveWait, func(s fleet.Status) (bool, error) {
		cur, ok := s.Member(m.Pod)
		if ok {
			now = cur
		}
		return ok && cur.Node != "" && cur.Node != m.Node && cur.State == "Active", nil
	})
	if err != nil {
		return "", fmt.Errorf("%s (call %s) not taking calls on another node than %s (now %+v): %w", m.Pod, call, m.Node, now, err)
	}
	addrs, err := f.ExternalAddresses(ctx)
	if err != nil {
		return "", err
	}
	if addrs[m.Pod] != addr {
		// The pool gives a pod the address it last used only on a node holding none: a spare that kept an idle
		// address of its own (a node that once ran a pod, on a fleet scaled down in place) keeps that one.
		return "", fmt.Errorf("%s moved from %s to %s, but holds %q, not %q: its calls' profiles are named for the old "+
			"address (does %s hold an idle address of its own?)", m.Pod, m.Node, now.Node, addrs[m.Pod], addr, now.Node)
	}
	return fmt.Sprintf("call %s was on %s on %s; cordoned it, crashed FreeSWITCH and deleted the pod; its replacement "+
		"takes calls on %s, with the same external address %q", call, m.Pod, m.Node, now.Node, addr), nil
}

// recovered reads sofia recover's answer: "Recovered 2 call(s)" or "No calls to recover.". FreeSWITCH counts call
// legs, so a bridged call is two.
var recovered = regexp.MustCompile(`Recovered (\d+) call`)

func parseRecovered(out string) (int, error) {
	if strings.Contains(out, "No calls to recover") {
		return 0, nil
	}
	if m := recovered.FindStringSubmatch(out); m != nil {
		return strconv.Atoi(m[1])
	}
	return 0, fmt.Errorf("unexpected answer %q", strings.TrimSpace(out))
}

// recoverOn runs sofia recover on the pod that carries a call, and checks it took over exactly the legs of the
// scenario's live calls on that pod (two each). The scenario's calls are the only ones on the fleet's nodes while
// it runs alone: a recover that also took another node's would count more, and break those calls.
func (t *timeline) recoverOn(ctx context.Context, call string) (string, error) {
	m, err := t.memberOf(ctx, call)
	if err != nil {
		return "", err
	}
	if _, err := t.addressOf(ctx, m.Pod, call); err != nil {
		return "", err
	}
	var here []string
	for _, id := range slices.Sorted(maps.Keys(t.calls)) {
		c := t.calls[id]
		if !closed(c.answered) || closed(c.ended) {
			continue
		}
		cm, err := t.memberOf(ctx, id)
		if err != nil {
			return "", err
		}
		if cm.Pod == m.Pod {
			here = append(here, id)
		}
	}
	ectx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	out, err := t.r.cfg.Fleet.Exec(ectx, m.Pod, fleet.FreeswitchContainer, "fs_cli", "-x", "sofia recover")
	if err != nil {
		return "", err
	}
	n, err := parseRecovered(out)
	if err != nil {
		// Not known to have run: cleanup still flushes the pod's rows.
		return "", fmt.Errorf("sofia recover on %s: %w", m.Pod, err)
	}
	// It ran: it took (and deleted) every row of the pod's profiles, whatever the count says.
	t.mu.Lock()
	t.unrecovered = slices.DeleteFunc(t.unrecovered, func(c crashedPod) bool { return c.pod == m.Pod })
	t.mu.Unlock()
	if want := 2 * len(here); n != want {
		return "", fmt.Errorf("sofia recover on %s took %d call legs, want %d: the legs of call(s) %s, the ones on it",
			m.Pod, n, want, orNone(here))
	}
	return fmt.Sprintf("sofia recover on %s took %d call legs: call(s) %s", m.Pod, n, orNone(here)), nil
}

// crashSeen bounds the wait for a crashed FreeSWITCH's container to be seen stopped or restarted (a variable for
// the tests).
var crashSeen = 30 * time.Second

// addressOf is the external address of the pod that carries a call. Crashing and recovering need one: FreeSWITCH's
// profiles are named for it, and without one every such node runs the same plain names, so a recover would take
// every node's calls and a flush would delete them.
func (t *timeline) addressOf(ctx context.Context, pod, call string) (string, error) {
	addrs, err := t.r.cfg.Fleet.ExternalAddresses(ctx)
	if err != nil {
		return "", err
	}
	if addrs[pod] == "" {
		return "", fmt.Errorf("%s (call %s) has no external address: its profiles have plain names, which every such node shares", pod, call)
	}
	return addrs[pod], nil
}

// crash crashes FreeSWITCH in pod (fsctl crash: no drain, no BYE, its calls left in the recovery table) and waits
// until it is seen to have died, then returns how the container was before. FreeSWITCH dies under the command, so
// fs_cli's connection drops and the exec fails whether it crashed or not: if it didn't, that error says why.
func (t *timeline) crash(ctx context.Context, pod, addr string) (fleet.ContainerState, error) {
	f := t.r.cfg.Fleet
	before, err := f.Container(ctx, pod, fleet.FreeswitchContainer)
	if err != nil {
		return before, err
	}
	ectx, cancel := context.WithTimeout(ctx, execTimeout)
	_, execErr := f.Exec(ectx, pod, fleet.FreeswitchContainer, "fs_cli", "-x", "fsctl crash")
	cancel()
	deadline := time.Now().Add(crashSeen)
	for {
		now, err := f.Container(ctx, pod, fleet.FreeswitchContainer)
		if err == nil && (now.Restarts > before.Restarts || !now.Running) {
			t.crashed(pod, addr)
			return before, nil
		}
		if time.Now().After(deadline) {
			return before, fmt.Errorf("FreeSWITCH in %s didn't crash within %s (fsctl crash: %v)", pod, crashSeen, execErr)
		}
		select {
		case <-ctx.Done():
			return before, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// crashed records that a pod's FreeSWITCH left its calls in the recovery table.
func (t *timeline) crashed(pod, address string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !slices.ContainsFunc(t.unrecovered, func(c crashedPod) bool { return c.pod == pod }) {
		t.unrecovered = append(t.unrecovered, crashedPod{pod: pod, address: address})
	}
}

// flushLeftovers empties the recovery table of each pod whose FreeSWITCH this run crashed and didn't recover
// (sofia recover flush: every row of the pod's profiles). Those calls have ended at the runner and the mock; left
// in the table, the next run's recover there would count them, and recovering them instead leaves channels that
// never end. Scenarios that act on the fleet run alone, so the pod's rows are this run's. The flush only reaches the
// profiles the pod runs now, which are named for its address: a pod that came back with another address can't
// flush the crashed one's rows, and that is said instead.
func (t *timeline) flushLeftovers(ctx context.Context) {
	t.mu.Lock()
	crashed := slices.Clone(t.unrecovered)
	t.unrecovered = nil
	t.mu.Unlock()
	if len(crashed) == 0 {
		return
	}
	now, nowErr := t.r.cfg.Fleet.ExternalAddresses(ctx)
	for _, c := range crashed {
		switch {
		case nowErr != nil:
			t.note("cleanup: couldn't check %s still holds %s, so its calls stay in the recovery table, where a later "+
				"recover will count them: %v", c.pod, c.address, nowErr)
			continue
		case now[c.pod] != c.address:
			t.note("cleanup: %s's calls stay in the recovery table under profiles named for %s, where a later recover "+
				"will count them: %s now holds %q, and a flush there reaches only its own profiles", c.pod, c.address,
				c.pod, now[c.pod])
			continue
		}
		ectx, cancel := context.WithTimeout(ctx, execTimeout)
		_, err := t.r.cfg.Fleet.Exec(ectx, c.pod, fleet.FreeswitchContainer, "fs_cli", "-x", "sofia recover flush")
		cancel()
		if err != nil {
			t.note("cleanup: couldn't flush %s's calls from the recovery table, where a later recover there will count them: %v", c.pod, err)
			continue
		}
		t.note("cleanup: flushed %s's calls from the recovery table", c.pod)
	}
}

// closed reports whether ch is closed, without waiting.
func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// memberOf finds the pod that carries a call: the mock recorded where the INVITE came from, which is the pod's
// address (the fleet runs on the host network).
func (t *timeline) memberOf(ctx context.Context, call string) (fleet.Member, error) {
	if m, ok := t.pods[call]; ok {
		return m, nil
	}
	if t.r.cfg.MockURL == "" {
		return fleet.Member{}, errors.New("finding a call's pod needs the mock backend")
	}
	recs, err := t.r.mockCalls(ctx, t.calls[call].bench)
	if err != nil {
		return fleet.Member{}, fmt.Errorf("reading the backend's record of call %s: %w", call, err)
	}
	if len(recs) == 0 || recs[0].Source == "" {
		return fleet.Member{}, fmt.Errorf("the backend has no record of where call %s came from", call)
	}
	host, _, err := net.SplitHostPort(recs[0].Source)
	if err != nil {
		host = recs[0].Source
	}
	st, err := t.r.cfg.Fleet.Status(ctx)
	if err != nil {
		return fleet.Member{}, err
	}
	m, ok := st.MemberAt(host)
	if !ok {
		return fleet.Member{}, fmt.Errorf("call %s came from %s, which is no member of %s", call, host, t.r.cfg.Fleet.Name())
	}
	t.pods[call] = m
	return m, nil
}

func (t *timeline) waitMember(ctx context.Context, w *scenario.WaitMember) (string, error) {
	m, err := t.memberOf(ctx, w.Of)
	if err != nil {
		return "", err
	}
	state := ""
	err = t.poll(ctx, msOr(w.TimeoutMs, defaultFleetWait), func(s fleet.Status) (bool, error) {
		cur, ok := s.Member(m.Pod)
		switch {
		case !ok:
			state = "Gone"
		// A pod recreated under the same name is a new member: it no longer carries the call.
		case cur.Revision != m.Revision && w.State != "Gone":
			state = "replaced (" + cur.State + ")"
		default:
			state = cur.State
		}
		return state == w.State, nil
	})
	if err != nil {
		return "", fmt.Errorf("pod %s (call %s) is %s, not %s: %w", m.Pod, w.Of, state, w.State, err)
	}
	return fmt.Sprintf("pod %s (call %s) is %s", m.Pod, w.Of, w.State), nil
}

// blackout takes a call's node dark: it starts the blackout pod there, waits for it to run, and returns once
// the pod has taken the node off the network (fleet.BlackoutDelay after it started).
func (t *timeline) blackout(ctx context.Context, b *scenario.Blackout) (string, error) {
	f := t.r.cfg.Fleet
	if t.r.cfg.BlackoutImage == "" {
		return "", errors.New("taking a node dark needs the image to run there (-blackout-image)")
	}
	m, err := t.memberOf(ctx, b.Of)
	if err != nil {
		return "", err
	}
	d := time.Duration(b.ForMs) * time.Millisecond
	pod, err := f.StartBlackout(ctx, m.Node, d, t.r.cfg.BlackoutImage)
	if err != nil {
		return "", fmt.Errorf("starting the blackout on %s: %w", m.Node, err)
	}
	// Until it is seen running, assume the latest it could go dark.
	t.blackouts = append(t.blackouts, blackoutPod{pod: pod, node: m.Node,
		until: time.Now().Add(blackoutStart + fleet.BlackoutDelay + d)})
	bp := &t.blackouts[len(t.blackouts)-1]

	sctx, cancel := context.WithTimeout(ctx, blackoutStart)
	defer cancel()
	var st fleet.BlackoutState
	for {
		st, err = f.Blackout(sctx, pod)
		if err == nil && st.Phase == "Running" && !st.StartedAt.IsZero() {
			break
		}
		if err == nil && st.Finished() {
			return "", fmt.Errorf("blackout pod %s on %s ended before it ran: %s %s", pod, m.Node, st.Phase, st.Reason)
		}
		select {
		case <-sctx.Done():
			return "", fmt.Errorf("blackout pod %s on %s not running within %s (%s %s, last error %v)",
				pod, m.Node, blackoutStart, st.Phase, st.Reason, err)
		case <-time.After(pollEvery):
		}
	}
	// StartedAt is to the second, so allow for one more before counting the node dark.
	dark := st.StartedAt.Add(fleet.BlackoutDelay + time.Second)
	bp.until = dark.Add(d)
	select {
	case <-time.After(time.Until(dark)):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return fmt.Sprintf("call %s is on %s; %s is dark for %s (pod %s)", b.Of, m.Pod, m.Node, d, pod), nil
}

// waitNode waits until the node carrying a call is Ready or NotReady.
func (t *timeline) waitNode(ctx context.Context, w *scenario.WaitNode) (string, error) {
	m, err := t.memberOf(ctx, w.Of)
	if err != nil {
		return "", err
	}
	want := w.State == "Ready"
	ctx, cancel := context.WithTimeout(ctx, msOr(w.TimeoutMs, defaultFleetWait))
	defer cancel()
	var lastErr error
	for {
		ready, err := t.r.cfg.Fleet.NodeReady(ctx, m.Node)
		if err == nil && ready == want {
			return fmt.Sprintf("node %s (call %s) is %s", m.Node, w.Of, w.State), nil
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("node %s (call %s) not %s: %w (last error: %v)", m.Node, w.Of, w.State, ctx.Err(), lastErr)
		case <-time.After(pollEvery):
		}
	}
}

func (t *timeline) rollStuck(ctx context.Context, rs *scenario.RollStuck) (string, error) {
	var first fleet.Status
	err := t.poll(ctx, msOr(rs.TimeoutMs, defaultRollStarted), func(s fleet.Status) (bool, error) {
		first = s
		return s.ObservedGeneration == s.Generation && s.UpdateRevision != s.CurrentRevision && s.Updated >= 1, nil
	})
	if err != nil {
		return "", fmt.Errorf("the roll never replaced a pod: %w (%s)", err, describe(first))
	}
	replaced := ""
	for _, m := range first.Members {
		if m.Revision == first.UpdateRevision {
			replaced = m.Pod
		}
	}
	deadline := time.Now().Add(time.Duration(rs.ForMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		s, err := t.r.cfg.Fleet.Status(ctx)
		if err != nil {
			return "", err
		}
		if s.UpdateRevision != first.UpdateRevision {
			return "", fmt.Errorf("the fleet's template changed while it should be stuck (%s)", describe(s))
		}
		if s.Updated > 1 {
			return "", fmt.Errorf("the roll went past its first pod: %s", describe(s))
		}
		if s.Ready < s.Want-1 {
			return "", fmt.Errorf("more than one pod stopped taking calls: %s", describe(s))
		}
		for _, m := range s.Members {
			if m.Revision == s.UpdateRevision && m.State == "Active" {
				return "", fmt.Errorf("the replacement %s takes calls: the change did not break it", m.Pod)
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollEvery):
		}
	}
	return fmt.Sprintf("roll stuck at %s for %dms; the rest kept taking calls", replaced, rs.ForMs), nil
}

// poll calls check with the fleet's status until it reports done, timeout passes or ctx ends.
func (t *timeline) poll(ctx context.Context, timeout time.Duration, check func(fleet.Status) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		s, err := t.r.cfg.Fleet.Status(ctx)
		if err == nil {
			done, cerr := check(s)
			if cerr != nil {
				return cerr
			}
			if done {
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

func (t *timeline) uncordon(ctx context.Context) ([]string, error) {
	var errs []error
	done := t.cordoned
	for _, n := range t.cordoned {
		if err := t.r.cfg.Fleet.Cordon(ctx, n, false); err != nil {
			errs = append(errs, fmt.Errorf("uncordoning %s: %w", n, err))
		}
	}
	t.cordoned = nil
	return done, errors.Join(errs...)
}

func (t *timeline) restore(ctx context.Context) ([]string, error) {
	var errs []error
	var done []string
	// Newest first, so a key edited twice ends at its first original.
	for _, b := range slices.Backward(t.broken) {
		if err := t.r.cfg.Fleet.SetConfigMapKey(ctx, b.configMap, b.key, b.original); err != nil {
			errs = append(errs, fmt.Errorf("restoring ConfigMap %s key %s: %w", b.configMap, b.key, err))
		}
		done = append(done, b.configMap+"/"+b.key)
	}
	t.broken = nil
	return done, errors.Join(errs...)
}

// cleanup undoes what the actions left behind, however the scenario ended. A node taken dark comes back on its own;
// cleanup waits for that first, so the next scenario finds the fleet whole.
func (t *timeline) cleanup() {
	t.endBlackouts()
	t.mu.Lock()
	unrecovered := len(t.unrecovered)
	t.mu.Unlock()
	if len(t.cordoned) == 0 && len(t.broken) == 0 && unrecovered == 0 {
		return
	}
	// The fleet is put back first, on a budget of its own: a flush that hangs must not leave a node cordoned.
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if nodes, err := t.uncordon(ctx); err != nil {
		t.fail("cleanup: %v", err)
	} else if len(nodes) > 0 {
		t.note("cleanup: uncordoned %s", strings.Join(nodes, ", "))
	}
	if keys, err := t.restore(ctx); err != nil {
		t.fail("cleanup: %v", err)
	} else if len(keys) > 0 {
		t.note("cleanup: restored %s", strings.Join(keys, ", "))
	}
	fctx, fcancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer fcancel()
	t.flushLeftovers(fctx)
}

// endBlackouts waits for each blackout pod to finish, which it can only be seen to do once its node is back on the
// network, notes what its log says, and deletes it. A pod not seen to finish is deleted anyway (its SIGTERM ends
// the blackout early, once the node hears of it) and fails the scenario: its node may still be dark.
func (t *timeline) endBlackouts() {
	f := t.r.cfg.Fleet
	for _, b := range t.blackouts {
		ctx, cancel := context.WithDeadline(context.Background(), b.until.Add(blackoutGrace))
		var st fleet.BlackoutState
		var err error
		for {
			st, err = f.Blackout(ctx, b.pod)
			if (err == nil && st.Finished()) || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(pollEvery):
			}
		}
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), cleanupTimeout)
		if log, lerr := f.PodLog(ctx, b.pod); lerr == nil {
			for line := range strings.Lines(log) {
				if line = strings.TrimSpace(line); line != "" {
					t.note("blackout %s on %s: %s", b.pod, b.node, line)
				}
			}
		}
		switch {
		case err == nil && st.Phase == "Succeeded":
		case err == nil && st.Finished():
			t.fail("blackout %s on %s failed: %s", b.pod, b.node, st.Reason)
		default:
			t.fail("blackout %s on %s not seen to finish by %s (%s, last error %v): the node may still be dark",
				b.pod, b.node, b.until.Add(blackoutGrace).Format(time.TimeOnly), st.Phase, err)
		}
		if err := f.DeletePod(ctx, b.pod); err != nil {
			t.fail("cleanup: deleting blackout pod %s: %v", b.pod, err)
		}
		cancel()
	}
	t.blackouts = nil
}

func describe(s fleet.Status) string {
	ms := make([]string, 0, len(s.Members))
	for _, m := range s.Members {
		rev := "old"
		if m.Revision == s.UpdateRevision {
			rev = "new"
		}
		ms = append(ms, fmt.Sprintf("%s %s %s", m.Pod, m.State, rev))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d wanted, %d ready, %d updated; %s", s.Want, s.Ready, s.Updated, strings.Join(ms, ", "))
	for _, p := range s.Proxies {
		loaded := "loaded"
		if !p.Loaded {
			loaded = "not loaded yet"
		}
		fmt.Fprintf(&b, "; Kamailio %s: %d in its dispatcher list, %s", p.Name, p.Destinations, loaded)
	}
	return b.String()
}

func msOr(ms int, def time.Duration) time.Duration {
	if ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return def
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "nothing"
	}
	return strings.Join(xs, ", ")
}

// crossChecks are expectations that compare calls: otherNodeThan.
func crossChecks(calls []scenario.Call, results []CallResult) []string {
	source := map[string]string{}
	for i, c := range calls {
		if b := results[i].Backend; len(b) > 0 {
			host, _, err := net.SplitHostPort(b[0].Source)
			if err != nil {
				host = b[0].Source
			}
			source[c.ID] = host
		}
	}
	var out []string
	for _, c := range calls {
		b := c.Expect.Backend
		if b == nil || b.OtherNodeThan == "" {
			continue
		}
		mine, theirs := source[c.ID], source[b.OtherNodeThan]
		switch {
		case mine == "" || theirs == "":
			out = append(out, fmt.Sprintf("call %s: can't compare its edge node with call %s's: the backend has no source for one", c.ID, b.OtherNodeThan))
		case mine == theirs:
			out = append(out, fmt.Sprintf("call %s: came from %s, the same edge node as call %s", c.ID, mine, b.OtherNodeThan))
		}
	}
	return out
}
