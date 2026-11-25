package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

const (
	conditionMembersHealthy = "MembersHealthy"

	// healthRecheck is how often a fleet with a member that isn't Healthy, or a failover under way, is reconciled
	// with nothing else happening.
	healthRecheck = 10 * time.Second
)

// markFirstReady records on each ready pod when the operator first saw it ready, so that only pods that have worked
// count as NotReady or are failed over automatically. The pods passed in are updated too.
func markFirstReady(ctx context.Context, c client.Client, pods []corev1.Pod, now time.Time) error {
	for i := range pods {
		p := &pods[i]
		if !podReady(p) || p.Annotations[sipv1alpha1.FirstReadyAnnotation] != "" {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[sipv1alpha1.FirstReadyAnnotation] = now.UTC().Format(time.RFC3339)
		if err := c.Patch(ctx, p, patch); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// syncHealth decides each member's health, records it in the status (members, lastFailover, the MembersHealthy
// condition) and the metrics, and fails over at most one member. It returns how soon the fleet should be looked at
// again for health's sake, or zero.
func (r *FreeswitchClusterReconciler) syncHealth(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, fleet fleetRef, cfg settings, pods []corev1.Pod, status *sipv1alpha1.FreeswitchClusterStatus) (time.Duration, error) {
	log := logf.FromContext(ctx)
	now := r.now()

	nodes, err := r.podNodes(ctx, pods)
	if err != nil {
		return 0, err
	}
	in := healthInput{pods: pods, nodes: nodes, cfg: cfg.failover, now: now}
	if r.SIPProbes != nil {
		in.sip = r.SIPProbes
	}
	verdicts := evaluateHealth(in)
	r.completeFailover(fsc, pods, status.LastFailover, now)

	decision := decideFailover(verdicts, cfg.failover.mode, status.LastFailover, now)
	if t := decision.target; t != nil {
		log.Info("failing over member", "pod", t.Name, "node", t.Spec.NodeName, "trigger", decision.trigger, "reason", decision.reason)
		err := r.failOver(ctx, fsc, t, decision.trigger, decision.reason, now)
		recordFailover(fleet, decision.trigger, err)
		if err != nil {
			r.event(fsc, corev1.EventTypeWarning, "FailoverFailed", "Failover", "could not fail over %s: %v", t.Name, err)
			return 0, fmt.Errorf("failing over %s: %w", t.Name, err)
		}
		status.LastFailover = &sipv1alpha1.FailoverRecord{
			Pod: t.Name, PodUID: string(t.UID), Node: t.Spec.NodeName, Trigger: decision.trigger,
			Reason: truncate(decision.reason, 1024), StartedAt: metav1.NewTime(now),
		}
		r.event(fsc, corev1.EventTypeWarning, "FailingOver", "Failover",
			"%s failover of %s: fenced node %s and deleted the pod to start it on another node; %s",
			decision.trigger, t.Name, t.Spec.NodeName, truncate(decision.reason, 512))
	}

	suspect, failed, recheck := r.attachHealth(fsc, verdicts, decision, status)
	inProgress := failoverInProgress(status.LastFailover, now)
	meta.SetStatusCondition(&status.Conditions, membersHealthyCondition(fsc.Generation, suspect, failed, inProgress, status.LastFailover))
	recordHealth(fleet, verdicts, decision.held, inProgress)

	if len(suspect) > 0 || len(failed) > 0 || inProgress || decision.target != nil {
		recheck = minPositive(recheck, healthRecheck)
	}
	return recheck, nil
}

// podNodes fetches the nodes the pods run on, by name. A node that is gone is left out.
func (r *FreeswitchClusterReconciler) podNodes(ctx context.Context, pods []corev1.Pod) (map[string]*corev1.Node, error) {
	nodes := map[string]*corev1.Node{}
	for i := range pods {
		name := pods[i].Spec.NodeName
		if name == "" || nodes[name] != nil {
			continue
		}
		var n corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		nodes[name] = &n
	}
	return nodes, nil
}

// completeFailover marks the latest failover done once a new pod of the same name is ready.
func (r *FreeswitchClusterReconciler) completeFailover(fsc *sipv1alpha1.FreeswitchCluster, pods []corev1.Pod, last *sipv1alpha1.FailoverRecord, now time.Time) {
	if last == nil || last.CompletedAt != nil {
		return
	}
	for i := range pods {
		p := &pods[i]
		if p.Name == last.Pod && string(p.UID) != last.PodUID && memberState(p) == sipv1alpha1.MemberActive {
			at := metav1.NewTime(now)
			last.CompletedAt, last.ReplacementNode = &at, p.Spec.NodeName
			r.event(fsc, corev1.EventTypeNormal, "FailoverCompleted", "Failover",
				"%s is ready again on node %s, %s after its failover", p.Name, p.Spec.NodeName, now.Sub(last.StartedAt.Time).Round(time.Second))
		}
	}
}

// attachHealth attaches each verdict to its member, says what is being done about it, and records each change of
// state as an event. It returns the Suspect and Failed members, and the soonest recheck the verdicts ask for.
func (r *FreeswitchClusterReconciler) attachHealth(fsc *sipv1alpha1.FreeswitchCluster, verdicts []verdict, decision failoverDecision, status *sipv1alpha1.FreeswitchClusterStatus) (suspect, failed []string, recheck time.Duration) {
	previous := map[string]*sipv1alpha1.MemberHealth{}
	for _, m := range fsc.Status.Members {
		previous[m.Pod] = m.Health
	}
	byPod := map[string]*verdict{}
	for i := range verdicts {
		byPod[verdicts[i].pod.Name] = &verdicts[i]
	}
	for i := range status.Members {
		m := &status.Members[i]
		v := byPod[m.Pod]
		if v == nil {
			continue
		}
		h := v.health
		if decision.target != nil && decision.target.Name == m.Pod {
			h.Message = "failing over: " + string(decision.trigger)
		} else if why, ok := decision.held[m.Pod]; ok {
			h.Message = "Failed, not failed over: " + why
		}
		m.Health = &h
		switch h.State {
		case sipv1alpha1.HealthSuspect:
			suspect = append(suspect, m.Pod)
		case sipv1alpha1.HealthFailed:
			failed = append(failed, m.Pod)
		}
		recheck = minPositive(recheck, v.recheck)
		if prev := previous[m.Pod]; prev == nil || prev.State != h.State {
			r.healthEvent(fsc, m.Pod, prev, h)
		}
	}
	return suspect, failed, recheck
}

// healthEvent records a member's change of health state.
func (r *FreeswitchClusterReconciler) healthEvent(fsc *sipv1alpha1.FreeswitchCluster, pod string, prev *sipv1alpha1.MemberHealth, h sipv1alpha1.MemberHealth) {
	switch h.State {
	case sipv1alpha1.HealthSuspect:
		r.event(fsc, corev1.EventTypeWarning, "MemberSuspect", "HealthCheck", "%s is Suspect: %s", pod, signalList(h.Signals))
	case sipv1alpha1.HealthFailed:
		r.event(fsc, corev1.EventTypeWarning, "MemberFailed", "HealthCheck", "%s has Failed: %s. %s", pod, signalList(h.Signals), h.Message)
	case sipv1alpha1.HealthHealthy:
		if prev != nil {
			r.event(fsc, corev1.EventTypeNormal, "MemberHealthy", "HealthCheck", "%s is Healthy again", pod)
		}
	}
}

// membersHealthyCondition sums up the members' health, and a failover still waiting for its replacement.
func membersHealthyCondition(gen int64, suspect, failed []string, inProgress bool, last *sipv1alpha1.FailoverRecord) metav1.Condition {
	cond := metav1.Condition{Type: conditionMembersHealthy, ObservedGeneration: gen}
	switch {
	case len(failed) > 0:
		cond.Status, cond.Reason = metav1.ConditionFalse, "MemberFailed"
		cond.Message = "Failed: " + strings.Join(failed, ", ")
	case len(suspect) > 0:
		cond.Status, cond.Reason = metav1.ConditionFalse, "MemberSuspect"
		cond.Message = "Suspect: " + strings.Join(suspect, ", ")
	case inProgress:
		cond.Status, cond.Reason = metav1.ConditionFalse, "FailingOver"
		cond.Message = "waiting for " + last.Pod + " to be ready again after its failover"
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "AllHealthy", "no member is Suspect or Failed"
	}
	if inProgress && cond.Reason != "FailingOver" {
		cond.Message += "; waiting for " + last.Pod + " to be ready again after its failover"
	}
	return cond
}

// failOver fences the pod's node, then deletes the pod at once (no drain: its calls are lost anyway). The
// StatefulSet starts it again, and the taint keeps it off the fenced node.
func (r *FreeswitchClusterReconciler) failOver(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, p *corev1.Pod, trigger sipv1alpha1.FailoverTrigger, reason string, now time.Time) error {
	if p.Spec.NodeName != "" {
		by := fmt.Sprintf("%s failover of FreeswitchCluster %s/%s pod %s at %s: %s",
			trigger, fsc.Namespace, fsc.Name, p.Name, now.UTC().Format(time.RFC3339), reason)
		if err := fenceNode(ctx, r.Client, p.Spec.NodeName, truncate(by, 1024)); err != nil {
			return fmt.Errorf("fencing node %s: %w", p.Spec.NodeName, err)
		}
	}
	uid := p.UID
	if err := r.Delete(ctx, p, client.Preconditions{UID: &uid}, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pod: %w", err)
	}
	return nil
}

// fenceNode taints a node sip.instruments.com/fenced:NoSchedule and records why. A node already fenced keeps its
// first record. A node that is gone needs no fence.
func fenceNode(ctx context.Context, c client.Client, name, by string) error {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return client.IgnoreNotFound(err)
	}
	if nodeFenced(&n) {
		return nil
	}
	// The node lifecycle controller changes taints too (unreachable, not-ready): don't overwrite its change.
	patch := client.MergeFromWithOptions(n.DeepCopy(), client.MergeFromWithOptimisticLock{})
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: sipv1alpha1.FencedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule})
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[sipv1alpha1.FencedByAnnotation] = by
	return client.IgnoreNotFound(c.Patch(ctx, &n, patch))
}

func (r *FreeswitchClusterReconciler) event(fsc *sipv1alpha1.FreeswitchCluster, typ, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(fsc, nil, typ, reason, action, note, args...)
}

func signalList(signals []sipv1alpha1.HealthSignal) string {
	parts := make([]string, 0, len(signals))
	for _, s := range signals {
		p := string(s.Type)
		if s.Message != "" {
			p += " (" + s.Message + ")"
		}
		parts = append(parts, p)
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
