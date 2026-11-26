package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

const (
	conditionMembersHealthy = "MembersHealthy"

	// healthRecheck is how often a fleet with a member that isn't Healthy, or a failover under way, is reconciled
	// with nothing else happening.
	healthRecheck = 10 * time.Second

	// fencedValue is the value of the FencedTaint and the FencedLabel.
	fencedValue = "true"
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
	r.completeFailover(fsc, cfg.replicas, pods, status.LastFailover, now)

	decision := decideFailover(verdicts, cfg.failover.mode, status.LastFailover)
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
		if err := r.persistFailover(ctx, fsc, status.LastFailover); err != nil {
			// The status update at the end of the reconcile tries again.
			log.Error(err, "recording the failover in the status", "pod", t.Name)
		}
		r.event(fsc, corev1.EventTypeWarning, "FailingOver", "Failover",
			"%s failover of %s: fenced node %s and deleted the pod to start it on another node; %s",
			decision.trigger, t.Name, t.Spec.NodeName, truncate(decision.reason, 512))
	}

	suspect, failed, recheck := r.attachHealth(fsc, verdicts, decision, status)
	inProgress := failoverInProgress(status.LastFailover)
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

// persistFailover writes a failover's record to the status at once, with a merge patch that can't conflict. The node
// is fenced and the pod gone by then, and the record is what keeps another automatic failover waiting, so it must not
// be lost to a status update that conflicts at the end of the reconcile (the requeue would find nothing to record).
func (r *FreeswitchClusterReconciler) persistFailover(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, rec *sipv1alpha1.FailoverRecord) error {
	base := fsc.DeepCopy()
	fsc.Status.LastFailover = rec.DeepCopy()
	return r.Status().Patch(ctx, fsc, client.MergeFrom(base))
}

// completeFailover marks the latest failover done once a new pod of the same name is ready, or once the fleet has
// been scaled down past the pod, so that no replacement is coming.
func (r *FreeswitchClusterReconciler) completeFailover(fsc *sipv1alpha1.FreeswitchCluster, replicas int32, pods []corev1.Pod, last *sipv1alpha1.FailoverRecord, now time.Time) {
	if last == nil || last.CompletedAt != nil {
		return
	}
	if ord, ok := podOrdinal(statefulSetName(fsc), last.Pod); ok && ord >= int(replicas) &&
		!slices.ContainsFunc(pods, func(p corev1.Pod) bool { return p.Name == last.Pod }) {
		at := metav1.NewTime(now)
		last.CompletedAt = &at
		r.event(fsc, corev1.EventTypeNormal, "FailoverCompleted", "Failover",
			"%s has no replacement to wait for: the fleet was scaled down to %d", last.Pod, replicas)
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

// podOrdinal is the ordinal of a StatefulSet's pod, from its name.
func podOrdinal(sts, pod string) (int, bool) {
	rest, ok := strings.CutPrefix(pod, sts+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 0
}

// failOver fences the pod's node, then deletes the pod at once (no drain: its calls are lost anyway). The
// StatefulSet starts it again, and the taint keeps it off the fenced node.
func (r *FreeswitchClusterReconciler) failOver(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, p *corev1.Pod, trigger sipv1alpha1.FailoverTrigger, reason string, now time.Time) error {
	if p.Spec.NodeName != "" {
		by := fmt.Sprintf("%s failover of FreeswitchCluster %s/%s pod %s at %s: %s",
			trigger, fsc.Namespace, fsc.Name, p.Name, now.UTC().Format(time.RFC3339), reason)
		if err := fenceNode(ctx, r.apiReader(), r.Client, p.Spec.NodeName, truncate(by, 1024)); err != nil {
			return fmt.Errorf("fencing node %s: %w", p.Spec.NodeName, err)
		}
	}
	uid := p.UID
	if err := r.Delete(ctx, p, client.Preconditions{UID: &uid}, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pod: %w", err)
	}
	return nil
}

func (r *FreeswitchClusterReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// fenceNode taints a node sip.instruments.com/fenced:NoSchedule, labels it sip.instruments.com/fenced=true (for fleets
// whose tolerations would tolerate the taint), and records why. A node already fenced keeps its first record. A node
// that is gone needs no fence. The node is read from reader, the API server rather than a cache where it can: a node
// just lost is being changed by the node lifecycle controller, and a stale copy only conflicts.
func fenceNode(ctx context.Context, reader client.Reader, c client.Client, name, by string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var n corev1.Node
		if err := reader.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			return client.IgnoreNotFound(err)
		}
		tainted, labeled := nodeFenced(&n), n.Labels[sipv1alpha1.FencedLabel] != ""
		if tainted && labeled {
			return nil
		}
		// The node lifecycle controller changes taints too (unreachable, not-ready): don't overwrite its change.
		patch := client.MergeFromWithOptions(n.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if !tainted {
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: sipv1alpha1.FencedTaint, Value: fencedValue, Effect: corev1.TaintEffectNoSchedule})
			if n.Annotations == nil {
				n.Annotations = map[string]string{}
			}
			n.Annotations[sipv1alpha1.FencedByAnnotation] = by
		}
		if !labeled {
			if n.Labels == nil {
				n.Labels = map[string]string{}
			}
			n.Labels[sipv1alpha1.FencedLabel] = fencedValue
		}
		return client.IgnoreNotFound(c.Patch(ctx, &n, patch))
	})
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

// avoidFencedNodes keeps a fleet's pods off fenced nodes by node affinity (the FencedLabel) when its tolerations would
// tolerate the FencedTaint, which would otherwise let the replacement land back on the node just fenced. It is nil
// for every other fleet, whose pod template the taint alone keeps off fenced nodes, so they aren't rolled for it.
func avoidFencedNodes(tolerations []corev1.Toleration) *corev1.NodeAffinity {
	if !slices.ContainsFunc(tolerations, toleratesFenced) {
		return nil
	}
	return &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: sipv1alpha1.FencedLabel, Operator: corev1.NodeSelectorOpDoesNotExist,
		}}}},
	}}
}

// toleratesFenced reports whether a toleration tolerates the FencedTaint (value true, effect NoSchedule).
func toleratesFenced(t corev1.Toleration) bool {
	if t.Effect != "" && t.Effect != corev1.TaintEffectNoSchedule {
		return false
	}
	switch t.Key {
	case "":
		return t.Operator == corev1.TolerationOpExists
	case sipv1alpha1.FencedTaint:
		return t.Operator == corev1.TolerationOpExists || t.Value == fencedValue
	}
	return false
}
