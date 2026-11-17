package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/fleet"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

// Fleet waits poll this often, and give up after these defaults.
const (
	pollEvery          = 500 * time.Millisecond
	defaultFleetWait   = 300 * time.Second
	defaultRollStarted = 120 * time.Second
	cleanupTimeout     = 60 * time.Second
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

	// What to undo: nodes cordoned, and each edited ConfigMap key's original value.
	cordoned []string
	broken   []brokenKey
	// pods remembers which pod carried each call, once looked up.
	pods map[string]fleet.Member

	mu       sync.Mutex
	log      []string
	failures []string
}

type brokenKey struct{ configMap, key, original string }

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
		if !strings.Contains(orig, b.Replace) {
			return "", fmt.Errorf("ConfigMap %s key %s has no %q to replace", b.ConfigMap, b.Key, b.Replace)
		}
		if err := f.SetConfigMapKey(ctx, b.ConfigMap, b.Key, strings.Replace(orig, b.Replace, b.With, 1)); err != nil {
			return "", err
		}
		t.broken = append(t.broken, brokenKey{configMap: b.ConfigMap, key: b.Key, original: orig})
		return fmt.Sprintf("ConfigMap %s key %s: %q -> %q", b.ConfigMap, b.Key, b.Replace, b.With), nil

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
	}
	return "", errors.New("no action")
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

// cleanup undoes what the actions left behind, however the scenario ended.
func (t *timeline) cleanup() {
	if len(t.cordoned) == 0 && len(t.broken) == 0 {
		return
	}
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
	return fmt.Sprintf("%d wanted, %d ready, %d updated; %s", s.Want, s.Ready, s.Updated, strings.Join(ms, ", "))
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
