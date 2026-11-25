package controller

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

// failoverTimeout is how long a failover counts as under way when its replacement doesn't become ready: another
// automatic failover waits that long at most.
const failoverTimeout = 10 * time.Minute

// failoverSettings is spec.failover with every default applied.
type failoverSettings struct {
	mode            sipv1alpha1.FailoverMode
	signalsRequired int
	failAfter       time.Duration
	notReady        time.Duration
	probePeriod     time.Duration
	probeTimeout    time.Duration
	probeThreshold  int
}

func resolveFailover(f *sipv1alpha1.FailoverSpec) failoverSettings {
	out := failoverSettings{
		mode: sipv1alpha1.FailoverDetect, signalsRequired: 2, failAfter: 15 * time.Second, notReady: 20 * time.Second,
		probePeriod: 5 * time.Second, probeTimeout: 2 * time.Second, probeThreshold: 3,
	}
	if f == nil {
		return out
	}
	if f.Mode != "" {
		out.mode = f.Mode
	}
	if f.SignalsRequired != 0 {
		out.signalsRequired = int(f.SignalsRequired)
	}
	if f.FailAfterSeconds != nil {
		out.failAfter = time.Duration(*f.FailAfterSeconds) * time.Second
	}
	if f.NotReadySeconds != nil {
		out.notReady = time.Duration(*f.NotReadySeconds) * time.Second
	}
	if p := f.SIPProbe; p.PeriodSeconds != 0 {
		out.probePeriod = time.Duration(p.PeriodSeconds) * time.Second
	}
	if p := f.SIPProbe; p.TimeoutSeconds != 0 {
		out.probeTimeout = time.Duration(p.TimeoutSeconds) * time.Second
	}
	if p := f.SIPProbe; p.FailureThreshold != 0 {
		out.probeThreshold = int(p.FailureThreshold)
	}
	return out
}

// sipProbeResult is what the SIP prober knows of one pod.
type sipProbeResult struct {
	// Failing is true once failureThreshold probes in a row have gone unanswered.
	Failing bool
	// Since is the first unanswered probe of the current run of failures.
	Since time.Time
	// Message is the last probe's error.
	Message string
}

// SIPProbeResults reads the SIP prober's results by pod UID. ok is false for a pod it has no result for yet.
type SIPProbeResults interface {
	Result(uid types.UID) (r sipProbeResult, ok bool)
}

// healthInput is everything a fleet's health verdict is decided from.
type healthInput struct {
	pods  []corev1.Pod
	nodes map[string]*corev1.Node // by name; a pod's node missing from it is gone
	sip   SIPProbeResults         // nil: no SIP signal
	cfg   failoverSettings
	now   time.Time
}

// signal is one failing view of a member, as evaluated.
type signal struct {
	typ     sipv1alpha1.HealthSignalType
	since   time.Time // when the problem began
	counts  time.Time // when it began to count: since, plus any grace
	message string
	counted bool // false when discounted (failing for every member)
}

// verdict is the health of one member.
type verdict struct {
	pod     *corev1.Pod
	health  sipv1alpha1.MemberHealth
	signals []signal
	// eligible: the pod has been ready, so failing it over can help.
	eligible bool
	// failedAt is when the member became (or becomes) Failed; zero unless it is Failed.
	failedAt time.Time
	// recheck is how long until the verdict could change by time alone, or zero.
	recheck time.Duration
}

// evaluateHealth decides each scheduled pod's health. Signals:
//   - NodeLost: the pod's node is gone, or its Ready condition isn't True (the node lifecycle controller sets it
//     Unknown once the kubelet stops renewing its lease), since the condition last changed.
//   - NotReady: a pod that has been ready (FirstReadyAnnotation), isn't terminating, and runs on a node that isn't
//     lost, has been unready for notReadySeconds. It doesn't count while the node is lost: the node's loss is what
//     makes it unready, so it would be the same signal twice.
//   - SIPUnreachable: the prober's last failureThreshold probes went unanswered.
//
// A signal failing for every member of a fleet of two or more doesn't count: more likely the observer (the
// operator's network, the control plane) than every member at once, and failing them all over wouldn't help.
// A member is Failed once signalsRequired counted signals have failed together for failAfterSeconds.
func evaluateHealth(in healthInput) []verdict {
	var out []verdict
	for i := range in.pods {
		p := &in.pods[i]
		if p.Spec.NodeName == "" {
			continue
		}
		v := verdict{pod: p, eligible: p.Annotations[sipv1alpha1.FirstReadyAnnotation] != ""}
		nodeLost := false
		if s, lost := nodeLostSignal(p, in.nodes[p.Spec.NodeName]); lost {
			nodeLost = true
			v.signals = append(v.signals, s)
		}
		if s, ok := notReadySignal(p, in.cfg.notReady); ok && !nodeLost {
			if s.counts.After(in.now) {
				v.recheck = minPositive(v.recheck, s.counts.Sub(in.now))
			} else {
				v.signals = append(v.signals, s)
			}
		}
		if in.sip != nil {
			if r, ok := in.sip.Result(p.UID); ok && r.Failing {
				v.signals = append(v.signals, signal{typ: sipv1alpha1.SignalSIPUnreachable, since: r.Since, counts: r.Since, message: r.Message})
			}
		}
		out = append(out, v)
	}

	// Discount a signal that fails for every member.
	discounted := map[sipv1alpha1.HealthSignalType]bool{}
	if len(out) >= 2 {
		for _, typ := range []sipv1alpha1.HealthSignalType{sipv1alpha1.SignalNodeLost, sipv1alpha1.SignalNotReady, sipv1alpha1.SignalSIPUnreachable} {
			all := true
			for i := range out {
				if !slices.ContainsFunc(out[i].signals, func(s signal) bool { return s.typ == typ }) {
					all = false
					break
				}
			}
			discounted[typ] = all
		}
	}

	for i := range out {
		v := &out[i]
		var counted []time.Time
		for j := range v.signals {
			s := &v.signals[j]
			s.counted = !discounted[s.typ]
			if s.counted {
				counted = append(counted, s.counts)
			}
			hs := sipv1alpha1.HealthSignal{Type: s.typ, Since: metav1.NewTime(s.since), Counted: s.counted, Message: s.message}
			if !s.counted {
				hs.Message = strings.TrimSpace(hs.Message + " (failing for every member, so not counted)")
			}
			v.health.Signals = append(v.health.Signals, hs)
		}
		slices.SortFunc(v.health.Signals, func(a, b sipv1alpha1.HealthSignal) int { return cmp.Compare(a.Type, b.Type) })
		switch {
		case len(v.signals) == 0:
			v.health.State = sipv1alpha1.HealthHealthy
		case len(counted) < in.cfg.signalsRequired:
			v.health.State = sipv1alpha1.HealthSuspect
			v.health.Message = fmt.Sprintf("%d of the %d signals required to fail over are failing", len(counted), in.cfg.signalsRequired)
		default:
			slices.SortFunc(counted, func(a, b time.Time) int { return a.Compare(b) })
			failedAt := counted[in.cfg.signalsRequired-1].Add(in.cfg.failAfter)
			if failedAt.After(in.now) {
				v.health.State = sipv1alpha1.HealthSuspect
				v.health.Message = fmt.Sprintf("Failed at %s unless a signal recovers", failedAt.UTC().Format(time.RFC3339))
				v.recheck = minPositive(v.recheck, failedAt.Sub(in.now))
			} else {
				v.health.State = sipv1alpha1.HealthFailed
				v.failedAt = failedAt
			}
		}
	}
	return out
}

// nodeLostSignal reports whether the control plane has lost the pod's node.
func nodeLostSignal(p *corev1.Pod, n *corev1.Node) (signal, bool) {
	if n == nil {
		since := p.CreationTimestamp.Time
		if c := podCondition(p, corev1.PodReady); c != nil && c.Status != corev1.ConditionTrue {
			since = c.LastTransitionTime.Time
		}
		return signal{typ: sipv1alpha1.SignalNodeLost, since: since, counts: since, message: "node " + p.Spec.NodeName + " is gone"}, true
	}
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		if c.Status == corev1.ConditionTrue {
			return signal{}, false
		}
		msg := fmt.Sprintf("node %s is Ready=%s", n.Name, c.Status)
		if c.Reason != "" {
			msg += " (" + c.Reason + ")"
		}
		if c.Message != "" {
			msg += ": " + c.Message
		}
		return signal{typ: sipv1alpha1.SignalNodeLost, since: c.LastTransitionTime.Time, counts: c.LastTransitionTime.Time, message: msg}, true
	}
	// No Ready condition yet: a node still joining, not a lost one.
	return signal{}, false
}

// notReadySignal reports whether a pod that has been ready is unready, and not terminating (a draining pod is
// unready by design). counts is when it has been unready for grace.
func notReadySignal(p *corev1.Pod, grace time.Duration) (signal, bool) {
	if p.DeletionTimestamp != nil || p.Annotations[sipv1alpha1.FirstReadyAnnotation] == "" {
		return signal{}, false
	}
	c := podCondition(p, corev1.PodReady)
	if c == nil || c.Status == corev1.ConditionTrue {
		return signal{}, false
	}
	msg := "pod is not ready"
	if c.Message != "" {
		msg += ": " + c.Message
	}
	if restarts := freeswitchRestarts(p); restarts > 0 {
		msg += fmt.Sprintf(" (FreeSWITCH has restarted %d times)", restarts)
	}
	since := c.LastTransitionTime.Time
	return signal{typ: sipv1alpha1.SignalNotReady, since: since, counts: since.Add(grace), message: msg}, true
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

func freeswitchRestarts(p *corev1.Pod) int32 {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == containerName {
			return cs.RestartCount
		}
	}
	return 0
}

func minPositive(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

// failoverDecision is what the failover loop should do now.
type failoverDecision struct {
	// target is the pod to fail over, or nil.
	target  *corev1.Pod
	trigger sipv1alpha1.FailoverTrigger
	reason  string
	// held explains, by pod, why a Failed member isn't failed over now.
	held map[string]string
}

// failoverInProgress reports whether the fleet's last failover is still waiting for its replacement.
func failoverInProgress(last *sipv1alpha1.FailoverRecord, now time.Time) bool {
	return last != nil && last.CompletedAt == nil && now.Sub(last.StartedAt.Time) < failoverTimeout
}

// decideFailover picks at most one member to fail over:
//   - A pod a person annotated sip.instruments.com/fail-over, whatever the mode or its health (the first by name).
//   - Otherwise, in mode Automatic and with no failover under way, the Failed member that has been ready and failed
//     first. A Failed member that has never been ready isn't failed over: a configuration or image that never
//     worked fails the same way on any node.
func decideFailover(verdicts []verdict, mode sipv1alpha1.FailoverMode, last *sipv1alpha1.FailoverRecord, now time.Time) failoverDecision {
	d := failoverDecision{held: map[string]string{}}
	for i := range verdicts {
		p := verdicts[i].pod
		if p.Annotations[sipv1alpha1.FailOverAnnotation] != "" {
			d.target, d.trigger = p, sipv1alpha1.FailoverTriggerManual
			d.reason = "requested with the " + sipv1alpha1.FailOverAnnotation + " annotation"
			return d
		}
	}
	var candidates []*verdict
	for i := range verdicts {
		v := &verdicts[i]
		if v.health.State != sipv1alpha1.HealthFailed {
			continue
		}
		switch {
		case mode != sipv1alpha1.FailoverAutomatic:
			d.held[v.pod.Name] = "failover mode is " + string(mode) + ": annotate the pod " + sipv1alpha1.FailOverAnnotation + "=true to fail it over"
		case !v.eligible:
			d.held[v.pod.Name] = "the pod has never been ready, so it would fail the same way on another node"
		case failoverInProgress(last, now):
			d.held[v.pod.Name] = "waiting for the failover of " + last.Pod + " to finish"
		default:
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return d
	}
	slices.SortFunc(candidates, func(a, b *verdict) int {
		return cmp.Or(a.failedAt.Compare(b.failedAt), cmp.Compare(a.pod.Name, b.pod.Name))
	})
	c := candidates[0]
	d.target, d.trigger, d.reason = c.pod, sipv1alpha1.FailoverTriggerAutomatic, signalSummary(c.health.Signals)
	for _, other := range candidates[1:] {
		d.held[other.pod.Name] = "failing over " + c.pod.Name + " first; one member at a time"
	}
	return d
}

// signalSummary lists counted signals, for a failover's reason.
func signalSummary(signals []sipv1alpha1.HealthSignal) string {
	var parts []string
	for _, s := range signals {
		if s.Counted {
			parts = append(parts, string(s.Type)+": "+s.Message)
		}
	}
	return strings.Join(parts, "; ")
}

// nodeFenced reports whether a failover has fenced the node.
func nodeFenced(n *corev1.Node) bool {
	return slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == sipv1alpha1.FencedTaint })
}
