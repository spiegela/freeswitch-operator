package controller

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

type fakeProbes map[types.UID]sipProbeResult

// fakeLeases is when each node's lease was last seen renewed, with the default grace (15 s).
type fakeLeases map[string]time.Time

func (f fakeLeases) Renewal(node string) (time.Time, time.Duration, bool) {
	seen, ok := f[node]
	return seen, nodeLeaseGrace(defaultNodeLeaseDuration), ok
}

func (f fakeProbes) Result(uid types.UID) (sipProbeResult, bool) {
	r, ok := f[uid]
	return r, ok
}

// hpod is a scheduled, ready pod that has been ready before.
func hpod(name, node string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Annotations: map[string]string{
			sipv1alpha1.FirstReadyAnnotation: t0.Add(-time.Hour).Format(time.RFC3339),
		}},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(t0.Add(-time.Hour)),
		}}},
	}
}

func unready(p corev1.Pod, since time.Time) corev1.Pod {
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(since)}}
	return p
}

func hnode(name string, ready corev1.ConditionStatus, since time.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: ready, LastTransitionTime: metav1.NewTime(since), Reason: "NodeStatusUnknown",
		}}},
	}
}

func healthyNodes(names ...string) map[string]*corev1.Node {
	out := map[string]*corev1.Node{}
	for _, n := range names {
		out[n] = hnode(n, corev1.ConditionTrue, t0.Add(-time.Hour))
	}
	return out
}

// lostB is a healthy node a and a node b whose kubelet was lost at since.
func lostB(since time.Time) map[string]*corev1.Node {
	nodes := healthyNodes("a")
	nodes["b"] = hnode("b", corev1.ConditionUnknown, since)
	return nodes
}

func states(vs []verdict) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		s := v.pod.Name + "=" + string(v.health.State)
		for _, sig := range v.health.Signals {
			s += "+" + string(sig.Type)
			if !sig.Counted {
				s += "(uncounted)"
			}
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func TestEvaluateHealth(t *testing.T) {
	cfg := resolveFailover(nil) // 2 signals, 5 s, notReady 20 s
	sipDown := func(since time.Time) sipProbeResult {
		return sipProbeResult{Failing: true, Since: since, Message: "no answer"}
	}
	cases := []struct {
		name   string
		pods   []corev1.Pod
		nodes  map[string]*corev1.Node
		sip    fakeProbes
		leases fakeLeases
		cfg    *failoverSettings
		want   string
	}{
		{
			name:  "all healthy",
			pods:  []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes: healthyNodes("a", "b"),
			want:  "edge-0=Healthy edge-1=Healthy",
		},
		{
			name:  "node lost and SIP down long enough: Failed",
			pods:  []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes: lostB(t0.Add(-time.Minute)),
			sip:   fakeProbes{"edge-1-uid": sipDown(t0.Add(-40 * time.Second))},
			want:  "edge-0=Healthy edge-1=Failed+NodeLost+SIPUnreachable",
		},
		{
			name:  "two signals, but the second only just began: Suspect until failAfter",
			pods:  []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes: lostB(t0.Add(-time.Minute)),
			sip:   fakeProbes{"edge-1-uid": sipDown(t0.Add(-2 * time.Second))},
			want:  "edge-0=Healthy edge-1=Suspect+NodeLost+SIPUnreachable",
		},
		{
			name:  "kubelet lost but FreeSWITCH answers SIP: Suspect",
			pods:  []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes: lostB(t0.Add(-time.Hour)),
			want:  "edge-0=Healthy edge-1=Suspect+NodeLost",
		},
		{
			name:  "...and Failed when one signal is enough",
			pods:  []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes: lostB(t0.Add(-time.Hour)),
			cfg:   &failoverSettings{signalsRequired: 1, failAfter: 15 * time.Second, notReady: 20 * time.Second},
			want:  "edge-0=Healthy edge-1=Failed+NodeLost",
		},
		{
			name:  "FreeSWITCH hung on a sound node: NotReady and SIP down make Failed",
			pods:  []corev1.Pod{hpod("edge-0", "a"), unready(hpod("edge-1", "b"), t0.Add(-time.Minute))},
			nodes: healthyNodes("a", "b"),
			sip:   fakeProbes{"edge-1-uid": sipDown(t0.Add(-time.Minute))},
			want:  "edge-0=Healthy edge-1=Failed+NotReady+SIPUnreachable",
		},
		{
			name:  "unready within notReadySeconds isn't a signal yet",
			pods:  []corev1.Pod{hpod("edge-0", "a"), unready(hpod("edge-1", "b"), t0.Add(-5*time.Second))},
			nodes: healthyNodes("a", "b"),
			want:  "edge-0=Healthy edge-1=Healthy",
		},
		{
			name: "a pod that has never been ready doesn't count as NotReady",
			pods: func() []corev1.Pod {
				p := unready(hpod("edge-1", "b"), t0.Add(-time.Hour))
				p.Annotations = nil
				return []corev1.Pod{hpod("edge-0", "a"), p}
			}(),
			nodes: healthyNodes("a", "b"),
			want:  "edge-0=Healthy edge-1=Healthy",
		},
		{
			name:  "readiness doesn't count again while the node is lost",
			pods:  []corev1.Pod{hpod("edge-0", "a"), unready(hpod("edge-1", "b"), t0.Add(-time.Minute))},
			nodes: lostB(t0.Add(-time.Minute)),
			want:  "edge-0=Healthy edge-1=Suspect+NodeLost",
		},
		{
			name: "a draining pod on a lost node can still fail",
			pods: func() []corev1.Pod {
				p := unready(hpod("edge-1", "b"), t0.Add(-time.Minute))
				p.DeletionTimestamp = &metav1.Time{Time: t0.Add(-time.Minute)}
				return []corev1.Pod{hpod("edge-0", "a"), p}
			}(),
			nodes: lostB(t0.Add(-time.Minute)),
			sip:   fakeProbes{"edge-1-uid": sipDown(t0.Add(-time.Minute))},
			want:  "edge-0=Healthy edge-1=Failed+NodeLost+SIPUnreachable",
		},
		{
			name:  "SIP down for every member is the observer's problem",
			pods:  []corev1.Pod{hpod("edge-0", "a"), unready(hpod("edge-1", "b"), t0.Add(-time.Minute))},
			nodes: healthyNodes("a", "b"),
			sip:   fakeProbes{"edge-0-uid": sipDown(t0.Add(-time.Minute)), "edge-1-uid": sipDown(t0.Add(-time.Minute))},
			want:  "edge-0=Suspect+SIPUnreachable(uncounted) edge-1=Suspect+NotReady+SIPUnreachable(uncounted)",
		},
		{
			name: "a member that has never been ready doesn't hide another's failure",
			pods: func() []corev1.Pod {
				p := unready(hpod("edge-0", "a"), t0.Add(-time.Minute))
				p.Annotations = nil
				return []corev1.Pod{p, hpod("edge-1", "b")}
			}(),
			nodes: lostB(t0.Add(-time.Minute)),
			sip:   fakeProbes{"edge-0-uid": sipDown(t0.Add(-time.Minute)), "edge-1-uid": sipDown(t0.Add(-time.Minute))},
			want:  "edge-0=Suspect+SIPUnreachable edge-1=Failed+NodeLost+SIPUnreachable",
		},
		{
			name:  "...but a fleet of one has no one to compare with",
			pods:  []corev1.Pod{unready(hpod("edge-0", "a"), t0.Add(-time.Minute))},
			nodes: healthyNodes("a"),
			sip:   fakeProbes{"edge-0-uid": sipDown(t0.Add(-time.Minute))},
			want:  "edge-0=Failed+NotReady+SIPUnreachable",
		},
		{
			name:  "a node that is gone is lost",
			pods:  []corev1.Pod{hpod("edge-0", "a"), unready(hpod("edge-1", "b"), t0.Add(-time.Minute))},
			nodes: healthyNodes("a"),
			sip:   fakeProbes{"edge-1-uid": sipDown(t0.Add(-time.Minute))},
			want:  "edge-0=Healthy edge-1=Failed+NodeLost+SIPUnreachable",
		},
		{
			name:   "kubelet's lease lapsed and SIP down: Failed while the node is still Ready",
			pods:   []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes:  healthyNodes("a", "b"),
			leases: fakeLeases{"a": t0, "b": t0.Add(-30 * time.Second)},
			sip:    fakeProbes{"edge-1-uid": sipDown(t0.Add(-25 * time.Second))},
			want:   "edge-0=Healthy edge-1=Failed+NodeLost+SIPUnreachable",
		},
		{
			name:   "a lease renewed within its grace isn't a signal",
			pods:   []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes:  healthyNodes("a", "b"),
			leases: fakeLeases{"a": t0, "b": t0.Add(-14 * time.Second)},
			sip:    fakeProbes{"edge-1-uid": sipDown(t0.Add(-25 * time.Second))},
			want:   "edge-0=Healthy edge-1=Suspect+SIPUnreachable",
		},
		{
			name:   "a lapsed lease counts from its grace's end: Suspect until failAfter after that",
			pods:   []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes:  healthyNodes("a", "b"),
			leases: fakeLeases{"a": t0, "b": t0.Add(-17 * time.Second)},
			sip:    fakeProbes{"edge-1-uid": sipDown(t0.Add(-25 * time.Second))},
			want:   "edge-0=Healthy edge-1=Suspect+NodeLost+SIPUnreachable",
		},
		{
			name:   "every lease lapsed is the observer's problem",
			pods:   []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes:  healthyNodes("a", "b"),
			leases: fakeLeases{"a": t0.Add(-time.Minute), "b": t0.Add(-time.Minute)},
			sip:    fakeProbes{"edge-1-uid": sipDown(t0.Add(-time.Minute))},
			want:   "edge-0=Suspect+NodeLost(uncounted) edge-1=Suspect+NodeLost(uncounted)+SIPUnreachable",
		},
		{
			name:  "SIP probes still within failureThreshold aren't a signal",
			pods:  []corev1.Pod{hpod("edge-0", "a"), hpod("edge-1", "b")},
			nodes: healthyNodes("a", "b"),
			sip:   fakeProbes{"edge-1-uid": {Failing: false, Since: t0.Add(-5 * time.Second)}},
			want:  "edge-0=Healthy edge-1=Healthy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			if tc.cfg != nil {
				c = *tc.cfg
			}
			in := healthInput{pods: tc.pods, nodes: tc.nodes, sip: tc.sip, leases: tc.leases, cfg: c, now: t0}
			if got := states(evaluateHealth(in)); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestEvaluateHealthRecheck(t *testing.T) {
	cfg := resolveFailover(nil)
	// Unready for 5 s: the signal counts at 20 s, so look again in 15 s.
	vs := evaluateHealth(healthInput{
		pods: []corev1.Pod{unready(hpod("edge-0", "a"), t0.Add(-5*time.Second))}, nodes: healthyNodes("a"), cfg: cfg, now: t0,
	})
	if vs[0].recheck != 15*time.Second {
		t.Errorf("recheck %s", vs[0].recheck)
	}
	// Two signals, the later one 2 s old: Failed 3 s from now.
	vs = evaluateHealth(healthInput{
		pods:  []corev1.Pod{hpod("edge-0", "a")},
		nodes: map[string]*corev1.Node{"a": hnode("a", corev1.ConditionUnknown, t0.Add(-time.Minute))},
		sip:   fakeProbes{"edge-0-uid": {Failing: true, Since: t0.Add(-2 * time.Second)}},
		cfg:   cfg, now: t0,
	})
	if vs[0].health.State != sipv1alpha1.HealthSuspect || vs[0].recheck != 3*time.Second {
		t.Errorf("state %s recheck %s", vs[0].health.State, vs[0].recheck)
	}
}

func failedVerdict(p corev1.Pod, at time.Time) verdict {
	pp := p
	return verdict{pod: &pp, failedAt: at,
		signals: []signal{{typ: sipv1alpha1.SignalNodeLost, since: at, counts: at}},
		health: sipv1alpha1.MemberHealth{State: sipv1alpha1.HealthFailed, Signals: []sipv1alpha1.HealthSignal{
			{Type: sipv1alpha1.SignalNodeLost, Counted: true, Message: "node gone"},
		}}}
}

func TestDecideFailover(t *testing.T) {
	healthy := verdict{pod: ptrPod(hpod("edge-0", "a")), health: sipv1alpha1.MemberHealth{State: sipv1alpha1.HealthHealthy}}
	auto, detect := sipv1alpha1.FailoverAutomatic, sipv1alpha1.FailoverDetect

	d := decideFailover([]verdict{healthy, failedVerdict(hpod("edge-1", "b"), t0)}, detect, nil)
	if d.target != nil || !strings.Contains(d.held["edge-1"], "Detect") {
		t.Errorf("detect: target %v held %v", d.target, d.held)
	}

	d = decideFailover([]verdict{healthy, failedVerdict(hpod("edge-1", "b"), t0)}, auto, nil)
	if d.target == nil || d.target.Name != "edge-1" || d.trigger != sipv1alpha1.FailoverTriggerAutomatic || !strings.Contains(d.reason, "NodeLost") {
		t.Errorf("automatic: %+v", d)
	}

	// Two Failed: the earlier first, the other held.
	d = decideFailover([]verdict{failedVerdict(hpod("edge-1", "b"), t0), failedVerdict(hpod("edge-2", "c"), t0.Add(-time.Second))}, auto, nil)
	if d.target == nil || d.target.Name != "edge-2" || !strings.Contains(d.held["edge-1"], "one member at a time") {
		t.Errorf("two failed: target %v held %v", d.target, d.held)
	}

	// A failover under way holds the next, however long it has been waiting; a finished one doesn't.
	last := &sipv1alpha1.FailoverRecord{Pod: "edge-2", StartedAt: metav1.NewTime(t0.Add(-time.Minute))}
	d = decideFailover([]verdict{failedVerdict(hpod("edge-1", "b"), t0)}, auto, last)
	if d.target != nil || !strings.Contains(d.held["edge-1"], "edge-2") {
		t.Errorf("in progress: target %v held %v", d.target, d.held)
	}
	done := metav1.NewTime(t0.Add(-30 * time.Second))
	last.CompletedAt = &done
	if d = decideFailover([]verdict{failedVerdict(hpod("edge-1", "b"), t0)}, auto, last); d.target == nil {
		t.Errorf("after completion: held %v", d.held)
	}
	last = &sipv1alpha1.FailoverRecord{Pod: "edge-2", StartedAt: metav1.NewTime(t0.Add(-24 * time.Hour))}
	if d = decideFailover([]verdict{failedVerdict(hpod("edge-1", "b"), t0)}, auto, last); d.target != nil || !strings.Contains(d.held["edge-1"], "spare") {
		t.Errorf("a day stuck: target %v held %v", d.target, d.held)
	}

	// Shutting down: held, unless its node is lost.
	draining := failedVerdict(hpod("edge-1", "b"), t0)
	draining.pod.DeletionTimestamp = &metav1.Time{Time: t0}
	draining.health.Signals = []sipv1alpha1.HealthSignal{{Type: sipv1alpha1.SignalSIPUnreachable, Counted: true}}
	if d = decideFailover([]verdict{draining}, auto, nil); d.target != nil || !strings.Contains(d.held["edge-1"], "shutting down") {
		t.Errorf("draining: target %v held %v", d.target, d.held)
	}
	draining.health.Signals = append(draining.health.Signals, sipv1alpha1.HealthSignal{Type: sipv1alpha1.SignalNodeLost, Counted: true})
	if d = decideFailover([]verdict{draining}, auto, nil); d.target == nil {
		t.Errorf("terminating on a lost node: held %v", d.held)
	}

	// Never ready: held.
	never := hpod("edge-1", "b")
	never.Annotations = nil
	d = decideFailover([]verdict{failedVerdict(never, t0)}, auto, nil)
	if d.target != nil || !strings.Contains(d.held["edge-1"], "never been ready") {
		t.Errorf("never ready: target %v held %v", d.target, d.held)
	}

	// The annotation fails over a healthy pod, in Detect mode, with another failover under way.
	asked := hpod("edge-0", "a")
	asked.Annotations[sipv1alpha1.FailOverAnnotation] = "true"
	last = &sipv1alpha1.FailoverRecord{Pod: "edge-2", StartedAt: metav1.NewTime(t0)}
	d = decideFailover([]verdict{{pod: &asked, health: sipv1alpha1.MemberHealth{State: sipv1alpha1.HealthHealthy}}}, detect, last)
	if d.target == nil || d.target.Name != "edge-0" || d.trigger != sipv1alpha1.FailoverTriggerManual {
		t.Errorf("manual: %+v", d)
	}
	// ...but only set to true.
	for _, v := range []string{"false", "0", "no", ""} {
		asked.Annotations[sipv1alpha1.FailOverAnnotation] = v
		if d = decideFailover([]verdict{{pod: &asked, health: sipv1alpha1.MemberHealth{State: sipv1alpha1.HealthHealthy}}}, detect, nil); d.target != nil {
			t.Errorf("fail-over=%q: failed over", v)
		}
	}
}

// A failover recorded but not carried out is finished first, whatever the pod's health now; once its pod is shutting
// down, there is nothing left to finish.
func TestDecideFailoverResume(t *testing.T) {
	healthy := verdict{pod: ptrPod(hpod("edge-0", "a")), health: sipv1alpha1.MemberHealth{State: sipv1alpha1.HealthHealthy}}
	pending := hpod("edge-1", "b")
	last := &sipv1alpha1.FailoverRecord{Pod: "edge-1", PodUID: string(pending.UID), Trigger: sipv1alpha1.FailoverTriggerManual,
		Reason: "asked", StartedAt: metav1.NewTime(t0)}
	d := decideFailover([]verdict{healthy, {pod: &pending}}, sipv1alpha1.FailoverDetect, last)
	if d.target == nil || d.target.Name != "edge-1" || !d.resume || d.trigger != sipv1alpha1.FailoverTriggerManual || d.reason != "asked" {
		t.Errorf("resume: %+v", d)
	}
	pending.DeletionTimestamp = &metav1.Time{Time: t0}
	if d = decideFailover([]verdict{healthy, {pod: &pending}}, sipv1alpha1.FailoverDetect, last); d.target != nil {
		t.Errorf("resume a pod already shutting down: %+v", d)
	}

	// Failed over by hand and shutting down gracefully, still annotated: not asked for again until it is gone.
	pending.Annotations = map[string]string{sipv1alpha1.FailOverAnnotation: "true"}
	if d = decideFailover([]verdict{healthy, {pod: &pending}}, sipv1alpha1.FailoverDetect, last); d.target != nil {
		t.Errorf("asked again while its failover is carried out: %+v", d)
	}
	// A request on a pod whose last failover is done still counts.
	done := metav1.NewTime(t0)
	if d = decideFailover([]verdict{healthy, {pod: &pending}}, sipv1alpha1.FailoverDetect,
		&sipv1alpha1.FailoverRecord{Pod: "edge-1", PodUID: string(pending.UID), StartedAt: metav1.NewTime(t0), CompletedAt: &done}); d.target == nil || d.resume {
		t.Errorf("a request on a pod whose last failover is done: %+v", d)
	}
}

func TestAvoidFencedNodes(t *testing.T) {
	terms := avoidFencedNodes().RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	want := []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
		Key: sipv1alpha1.FencedLabel, Operator: corev1.NodeSelectorOpDoesNotExist,
	}}}}
	if !equality.Semantic.DeepEqual(terms, want) {
		t.Errorf("terms %+v", terms)
	}
}

func TestOrdinal(t *testing.T) {
	for pod, want := range map[string]int{"edge-0": 0, "edge-12": 12, "edge-x": -1, "edge-": -1, "edge": -1} {
		if n := ordinal(pod); n != want {
			t.Errorf("%s: %d, want %d", pod, n, want)
		}
	}
}

func TestCompleteFailoverScaledDown(t *testing.T) {
	r := &FreeswitchClusterReconciler{}
	fsc := &sipv1alpha1.FreeswitchCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}
	last := &sipv1alpha1.FailoverRecord{Pod: "edge-2", PodUID: "old", StartedAt: metav1.NewTime(t0.Add(-time.Hour))}
	r.completeFailover(fsc, 3, []corev1.Pod{hpod("edge-0", "a")}, last, t0)
	if last.CompletedAt != nil {
		t.Fatalf("edge-2 is still wanted, yet completed")
	}
	r.completeFailover(fsc, 2, []corev1.Pod{hpod("edge-0", "a")}, last, t0)
	if last.CompletedAt == nil || !last.CompletedAt.Equal(&metav1.Time{Time: t0}) {
		t.Errorf("scaled down to 2: completedAt %v", last.CompletedAt)
	}
}

func ptrPod(p corev1.Pod) *corev1.Pod { return &p }

func TestResolveFailover(t *testing.T) {
	d := resolveFailover(nil)
	if d.mode != sipv1alpha1.FailoverDetect || d.signalsRequired != 2 || d.failAfter != 5*time.Second ||
		d.notReady != 20*time.Second || d.probePeriod != 2*time.Second || d.probeTimeout != 2*time.Second || d.probeThreshold != 3 {
		t.Errorf("defaults: %+v", d)
	}
	zero := int32(0)
	got := resolveFailover(&sipv1alpha1.FailoverSpec{Mode: sipv1alpha1.FailoverAutomatic, SignalsRequired: 1, FailAfterSeconds: &zero,
		SIPProbe: sipv1alpha1.SIPProbeSpec{PeriodSeconds: 3, FailureThreshold: 2}})
	if got.mode != sipv1alpha1.FailoverAutomatic || got.signalsRequired != 1 || got.failAfter != 0 || got.notReady != 20*time.Second ||
		got.probePeriod != 3*time.Second || got.probeTimeout != 2*time.Second || got.probeThreshold != 2 {
		t.Errorf("set: %+v", got)
	}
}

// sipResponder answers each UDP request on a local socket with resp(request), or not at all when it returns "".
func sipResponder(t *testing.T, resp func(req string) string) (string, int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if r := resp(string(buf[:n])); r != "" {
				_, _ = pc.WriteTo([]byte(r), from)
			}
		}
	}()
	a := pc.LocalAddr().(*net.UDPAddr)
	return a.IP.String(), int32(a.Port)
}

func headerOf(req, name string) string {
	for l := range strings.SplitSeq(req, "\r\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func TestSIPOptions(t *testing.T) {
	ctx := context.Background()
	t.Run("any answer counts", func(t *testing.T) {
		host, port := sipResponder(t, func(req string) string {
			if !strings.HasPrefix(req, "OPTIONS sip:") {
				return ""
			}
			return "SIP/2.0 403 Forbidden\r\nVia: " + headerOf(req, "Via") + "\r\nCall-ID: " + headerOf(req, "Call-ID") +
				"\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"
		})
		if err := SIPOptions(ctx, host, port, "udp", time.Second); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a lost first request is retransmitted", func(t *testing.T) {
		n := 0
		host, port := sipResponder(t, func(req string) string {
			n++
			if n == 1 {
				return ""
			}
			return "SIP/2.0 200 OK\r\ni: " + headerOf(req, "Call-ID") + "\r\n\r\n"
		})
		if err := SIPOptions(ctx, host, port, "udp", 2*time.Second); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("an answer to another request doesn't count", func(t *testing.T) {
		host, port := sipResponder(t, func(string) string { return "SIP/2.0 200 OK\r\nCall-ID: someone-else\r\n\r\n" })
		if err := SIPOptions(ctx, host, port, "udp", 700*time.Millisecond); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("silence times out", func(t *testing.T) {
		host, port := sipResponder(t, func(string) string { return "" })
		start := time.Now()
		err := SIPOptions(ctx, host, port, "udp", 600*time.Millisecond)
		if err == nil || time.Since(start) > 2*time.Second {
			t.Fatalf("err %v after %s", err, time.Since(start))
		}
	})
	t.Run("tcp", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		go func() {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			buf := make([]byte, 4096)
			n, _ := c.Read(buf)
			_, _ = c.Write([]byte("SIP/2.0 200 OK\r\nCall-ID: " + headerOf(string(buf[:n]), "Call-ID") + "\r\nContent-Length: 0\r\n\r\n"))
		}()
		a := l.Addr().(*net.TCPAddr)
		if err := SIPOptions(ctx, "127.0.0.1", int32(a.Port), "tcp", time.Second); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSIPProber(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = sipv1alpha1.AddToScheme(scheme)
	fsc := &sipv1alpha1.FreeswitchCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "telephony"},
		Spec: sipv1alpha1.FreeswitchClusterSpec{Failover: &sipv1alpha1.FailoverSpec{SIPProbe: sipv1alpha1.SIPProbeSpec{PeriodSeconds: 1, FailureThreshold: 2}}}}
	pod := hpod("edge-0", "a")
	pod.Namespace, pod.Labels, pod.Status.PodIP = "telephony", selectorLabels(fsc), "10.0.0.1"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fsc, &pod).Build()

	now := t0
	answer := false
	p := &SIPProber{
		Client: c, Events: make(chan event.GenericEvent, 4), Now: func() time.Time { return now },
		Probe: func(_ context.Context, host string, port int32, transport sipv1alpha1.DispatcherTransport, _ time.Duration) error {
			if host != "10.0.0.1" || port != 5060 || transport != "udp" {
				t.Errorf("probed %s:%d/%s", host, port, transport)
			}
			if answer {
				return nil
			}
			return errors.New("no answer")
		},
	}
	sem := make(chan struct{}, 4)
	step := func() {
		var wg sync.WaitGroup
		p.tick(context.Background(), sem, &wg)
		wg.Wait()
		now = now.Add(time.Second)
	}
	if _, ok := p.Result(pod.UID); ok {
		t.Fatal("a result before any probe")
	}
	step()
	if r, ok := p.Result(pod.UID); !ok || r.Failing || !r.Since.Equal(t0) {
		t.Fatalf("after one failure: %+v %v", r, ok)
	}
	step()
	r, _ := p.Result(pod.UID)
	if !r.Failing || !r.Since.Equal(t0) || r.Message != "SIP OPTIONS probes unanswered: no answer" {
		t.Fatalf("after two failures: %+v", r)
	}
	select {
	case ev := <-p.Events:
		if ev.Object.GetName() != "edge" || ev.Object.GetNamespace() != "telephony" {
			t.Errorf("event for %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
		}
	default:
		t.Fatal("no event when the signal started failing")
	}
	answer = true
	step()
	if r, _ := p.Result(pod.UID); r.Failing {
		t.Fatalf("after an answer: %+v", r)
	}
	if len(p.Events) != 1 {
		t.Fatalf("want an event when the signal recovered, have %d", len(p.Events))
	}
	// A pod that has gone is forgotten.
	if err := c.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	step()
	if _, ok := p.Result(pod.UID); ok {
		t.Fatal("a result for a pod that has gone")
	}
}

func failoverTestClient(t *testing.T, funcs interceptor.Funcs) (client.Client, *sipv1alpha1.FreeswitchCluster, corev1.Pod, *corev1.Node) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = sipv1alpha1.AddToScheme(scheme)
	fsc := &sipv1alpha1.FreeswitchCluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "telephony"}}
	pod := hpod("edge-0", "a")
	pod.Namespace = "telephony"
	pod.Annotations[sipv1alpha1.FailOverAnnotation] = "true"
	node := hnode("a", corev1.ConditionTrue, t0.Add(-time.Hour))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fsc, &pod, node).WithStatusSubresource(fsc).
		WithInterceptorFuncs(funcs).Build()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(fsc), fsc); err != nil {
		t.Fatal(err)
	}
	return c, fsc, pod, node
}

// A failover that fails is reported on the member and returned, with the rest of the status filled in, so the fleet's
// status goes on being written while the failover is retried.
func TestFailoverErrorKeepsStatus(t *testing.T) {
	refuseNodes := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*corev1.Node); ok {
			return errors.New("node patch refused")
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
	c, fsc, pod, node := failoverTestClient(t, refuseNodes)
	r := &FreeswitchClusterReconciler{Client: c, Now: func() time.Time { return t0 }}
	status := fsc.Status.DeepCopy()
	status.Members = []sipv1alpha1.FreeswitchMember{{Member: sipv1alpha1.Member{Pod: "edge-0"}}}
	_, err := r.syncHealth(context.Background(), fsc, fleetRef{kind: kindFreeswitch, namespace: "telephony", name: "edge"},
		resolve(fsc), []corev1.Pod{pod}, map[string]*corev1.Node{"a": node}, status)
	if err == nil || !strings.Contains(err.Error(), "node patch refused") {
		t.Fatalf("err %v", err)
	}
	if status.LastFailover == nil || status.LastFailover.Pod != "edge-0" || status.LastFailover.CompletedAt != nil {
		t.Errorf("record %+v", status.LastFailover)
	}
	if h := status.Members[0].Health; h == nil || !strings.Contains(h.Message, "failover failed, retrying") {
		t.Errorf("health %+v", h)
	}
}

// Resuming a failover whose pod is already gone (a cache that hadn't caught up still listed it) does nothing more:
// it isn't counted or announced again.
func TestResumeAlreadyCarriedOut(t *testing.T) {
	c, fsc, pod, _ := failoverTestClient(t, interceptor.Funcs{})
	r := &FreeswitchClusterReconciler{Client: c, Now: func() time.Time { return t0 }}
	fleet := fleetRef{kind: kindFreeswitch, namespace: "telephony", name: "edge-resume"}
	ok := func() float64 {
		return testutil.ToFloat64(failovers.WithLabelValues(fleet.labels(string(sipv1alpha1.FailoverTriggerManual), reloadOK)...))
	}
	if deleted, err := r.failOver(context.Background(), fsc, &pod, sipv1alpha1.FailoverTriggerManual, "asked", false, t0); err != nil || !deleted {
		t.Fatalf("first: deleted %v, err %v", deleted, err)
	}
	before := ok()
	d := failoverDecision{target: &pod, trigger: sipv1alpha1.FailoverTriggerManual, reason: "asked", resume: true}
	r.carryOut(context.Background(), fsc, fleet, settings{}, nil, &d, fsc.Status.DeepCopy(), t0)
	if d.err != nil || ok() != before {
		t.Errorf("err %v; failovers ok %v, was %v", d.err, ok(), before)
	}
}

// A pod already replaced by a new one of the same name fails the delete's UID precondition with a Conflict: that pod
// is gone too, and resuming its failover does nothing more.
func TestResumeAlreadyReplaced(t *testing.T) {
	replaced := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		return apierrors.NewConflict(corev1.Resource("pods"), obj.GetName(), errors.New("Precondition failed: UID in precondition"))
	}}
	c, fsc, pod, _ := failoverTestClient(t, replaced)
	r := &FreeswitchClusterReconciler{Client: c, Now: func() time.Time { return t0 }}
	fleet := fleetRef{kind: kindFreeswitch, namespace: "telephony", name: "edge-replaced"}
	if deleted, err := r.failOver(context.Background(), fsc, &pod, sipv1alpha1.FailoverTriggerManual, "asked", false, t0); err != nil || deleted {
		t.Fatalf("deleted %v, err %v", deleted, err)
	}
	d := failoverDecision{target: &pod, trigger: sipv1alpha1.FailoverTriggerManual, reason: "asked", resume: true}
	r.carryOut(context.Background(), fsc, fleet, settings{}, nil, &d, fsc.Status.DeepCopy(), t0)
	for _, result := range []string{reloadOK, reloadError} {
		if n := testutil.ToFloat64(failovers.WithLabelValues(fleet.labels(string(sipv1alpha1.FailoverTriggerManual), result)...)); d.err != nil || n != 0 {
			t.Errorf("err %v; failovers %s %v", d.err, result, n)
		}
	}
}

// The label marks a fence, so the record of why goes with it: a node left tainted after a person removed only the
// label gets the new failover's record.
func TestFenceRecordGoesWithLabel(t *testing.T) {
	c, _, _, node := failoverTestClient(t, interceptor.Funcs{})
	ctx := context.Background()
	node.Spec.Taints = []corev1.Taint{fencedTaint}
	node.Annotations = map[string]string{sipv1alpha1.FencedByAnnotation: "an old failover"}
	if err := c.Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := fenceNode(ctx, c, c, "a", "this failover"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatal(err)
	}
	if !nodeFenced(node) || len(node.Spec.Taints) != 1 || node.Annotations[sipv1alpha1.FencedByAnnotation] != "this failover" {
		t.Errorf("labels %v taints %v annotations %v", node.Labels, node.Spec.Taints, node.Annotations)
	}
}

// podDeletes records each pod delete's grace period: nil for the pod's own, graceful.
func podDeletes(graces *[]*int64) interceptor.Funcs {
	return interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*corev1.Pod); ok {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			*graces = append(*graces, o.GracePeriodSeconds)
		}
		return c.Delete(ctx, obj, opts...)
	}}
}

// A member a person fails over keeps its calls only with spec.recovery, and with a node whose kubelet can run its
// drain: its pod is deleted gracefully then, and at once otherwise.
func TestManualFailoverKeepsCallsWithRecovery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recovery  bool
		node      corev1.ConditionStatus
		lapsed    bool
		keepCalls bool
	}{
		{"recovery, node ready", true, corev1.ConditionTrue, false, true},
		{"no recovery", false, corev1.ConditionTrue, false, false},
		{"node not ready", true, corev1.ConditionUnknown, false, false},
		{"node ready, but its lease lapsed", true, corev1.ConditionTrue, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var graces []*int64
			c, fsc, pod, _ := failoverTestClient(t, podDeletes(&graces))
			if tc.recovery {
				fsc.Spec.Recovery = &sipv1alpha1.RecoverySpec{FenceImage: "example/exporter:1"}
			}
			r := &FreeswitchClusterReconciler{Client: c, Now: func() time.Time { return t0 }, NodeLeases: fakeLeases{"a": t0}}
			if tc.lapsed {
				r.NodeLeases = fakeLeases{"a": t0.Add(-time.Minute)}
			}
			status := fsc.Status.DeepCopy()
			nodes := map[string]*corev1.Node{"a": hnode("a", tc.node, t0.Add(-time.Hour))}
			fleet := fleetRef{kind: kindFreeswitch, namespace: "telephony", name: "edge"}
			cfg := resolve(fsc) // before the record's patch reloads fsc, which the fake server holds without recovery
			recheck, err := r.syncHealth(context.Background(), fsc, fleet, cfg, []corev1.Pod{pod}, nodes, status)
			if err != nil {
				t.Fatal(err)
			}
			if tc.keepCalls {
				// Recorded, then the kubelet is given time to write the annotation for the drain.
				if len(graces) != 0 || status.LastFailover == nil || recheck != keepCallsSettle {
					t.Fatalf("before the settle: deletes %v, record %v, recheck %v", graces, status.LastFailover, recheck)
				}
				r.Now = func() time.Time { return t0.Add(keepCallsSettle) }
				if _, err := r.syncHealth(context.Background(), fsc, fleet, cfg, []corev1.Pod{pod}, nodes, status); err != nil {
					t.Fatal(err)
				}
			}
			if len(graces) != 1 {
				t.Fatalf("deletes: %v", graces)
			}
			if graceful := graces[0] == nil; graceful != tc.keepCalls || (!graceful && *graces[0] != 0) {
				t.Errorf("grace period %v, want graceful %v", graces[0], tc.keepCalls)
			}
		})
	}
}

// A pod deleted gracefully to keep its calls that is still there keepCallsWait after the delete is deleted at once.
func TestForceKeptCallsFailover(t *testing.T) {
	var graces []*int64
	c, fsc, pod, _ := failoverTestClient(t, podDeletes(&graces))
	r := &FreeswitchClusterReconciler{Client: c, Now: func() time.Time { return t0 }}
	ctx := context.Background()
	last := &sipv1alpha1.FailoverRecord{Pod: "edge-0", PodUID: string(pod.UID), StartedAt: metav1.NewTime(t0.Add(-5 * time.Second))}
	// Deleted at t0 with a grace period of 330s: the deletion timestamp is when that ends.
	pod.DeletionTimestamp = &metav1.Time{Time: t0.Add(330 * time.Second)}
	pod.DeletionGracePeriodSeconds = ptr.To[int64](330)

	if wait := r.forceKeptCallsFailover(ctx, fsc, []corev1.Pod{pod}, last, t0.Add(10*time.Second)); wait != 20*time.Second || len(graces) != 0 {
		t.Fatalf("within the wait: wait %v, deletes %v", wait, graces)
	}
	done := *last
	done.CompletedAt = &metav1.Time{Time: t0}
	deletedAtOnce := pod
	deletedAtOnce.DeletionGracePeriodSeconds = ptr.To[int64](0)
	for name, tc := range map[string]struct {
		pod  corev1.Pod
		last *sipv1alpha1.FailoverRecord
	}{"completed": {pod, &done}, "deleted at once": {deletedAtOnce, last}, "no failover": {pod, nil}} {
		if wait := r.forceKeptCallsFailover(ctx, fsc, []corev1.Pod{tc.pod}, tc.last, t0.Add(time.Minute)); wait != 0 || len(graces) != 0 {
			t.Fatalf("%s: wait %v, deletes %v", name, wait, graces)
		}
	}
	if wait := r.forceKeptCallsFailover(ctx, fsc, []corev1.Pod{pod}, last, t0.Add(30*time.Second)); wait != 0 || len(graces) != 1 || graces[0] == nil || *graces[0] != 0 {
		t.Fatalf("after the wait: wait %v, deletes %v", wait, graces)
	}
}

// Of a lapsed lease and a node not Ready, the signal reported is the one that counts first.
func TestNodeLostSignalEarliest(t *testing.T) {
	p := hpod("edge-0", "a")
	notReadyLong := hnode("a", corev1.ConditionUnknown, t0.Add(-time.Hour))
	notReadyNow := hnode("a", corev1.ConditionUnknown, t0)
	lapsed := fakeLeases{"a": t0.Add(-time.Minute)}
	for name, tc := range map[string]struct {
		node   *corev1.Node
		leases NodeLeaseResults
		lost   bool
		lease  bool
	}{
		"ready, lease renewing":           {node: healthyNodes("a")["a"], leases: fakeLeases{"a": t0}},
		"ready, no lease seen":            {node: healthyNodes("a")["a"], leases: fakeLeases{}},
		"ready, no lease watch":           {node: healthyNodes("a")["a"]},
		"ready, lease lapsed":             {node: healthyNodes("a")["a"], leases: lapsed, lost: true, lease: true},
		"not ready long before the lapse": {node: notReadyLong, leases: lapsed, lost: true},
		"lapsed before not ready":         {node: notReadyNow, leases: lapsed, lost: true, lease: true},
		"node gone":                       {leases: lapsed, lost: true},
	} {
		s, lost := nodeLostSignal(&p, tc.node, tc.leases, t0)
		if lost != tc.lost || (lost && strings.Contains(s.message, "lease") != tc.lease) {
			t.Errorf("%s: lost %v, signal %+v", name, lost, s)
		}
		if tc.lease && (!s.since.Equal(t0.Add(-time.Minute)) || !s.counts.Equal(t0.Add(-45*time.Second))) {
			t.Errorf("%s: since %v counts %v", name, s.since, s.counts)
		}
	}
}
