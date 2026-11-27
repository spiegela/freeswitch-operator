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
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

const (
	conditionMembersHealthy = "MembersHealthy"

	// healthRecheck is how often a fleet with a member that isn't Healthy, or a failover under way, is reconciled
	// with nothing else happening: a safety net, since the pod, node and SIP prober events, and each verdict's own
	// recheck, already bring every change.
	healthRecheck = time.Minute

	// fencedValue is the value of the FencedTaint and the FencedLabel.
	fencedValue = "true"
)

// fencedTaint is the taint a failover puts on the node it fences.
var fencedTaint = corev1.Taint{Key: sipv1alpha1.FencedTaint, Value: fencedValue, Effect: corev1.TaintEffectNoSchedule}

// markFirstReady records on each ready pod when the operator first saw it ready, so that only pods that have worked
// count as NotReady or are failed over automatically. The pods passed in are updated too.
func markFirstReady(ctx context.Context, c client.Client, pods []corev1.Pod, now time.Time) error {
	for i := range pods {
		p := &pods[i]
		if !podReady(p) || hasBeenReady(p) {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		metav1.SetMetaDataAnnotation(&p.ObjectMeta, sipv1alpha1.FirstReadyAnnotation, now.UTC().Format(time.RFC3339))
		if err := c.Patch(ctx, p, patch); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// syncHealth decides each member's health, records it in the status (members, lastFailover, the MembersHealthy
// condition) and the metrics, and fails over at most one member. It returns how soon the fleet should be looked at
// again for health's sake, or zero, and a failover that failed: the status is filled in all the same, so the caller
// writes it before returning the error.
func (r *FreeswitchClusterReconciler) syncHealth(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, fleet fleetRef, cfg settings, pods []corev1.Pod, nodes map[string]*corev1.Node, status *sipv1alpha1.FreeswitchClusterStatus) (time.Duration, error) {
	now := r.now()

	verdicts := evaluateHealth(healthInput{pods: pods, nodes: nodes, sip: r.SIPProbes, leases: r.NodeLeases, cfg: cfg.failover, now: now})
	r.completeFailover(fsc, cfg.replicas, pods, status.LastFailover, now)

	decision := decideFailover(verdicts, cfg.failover.mode, status.LastFailover)
	recheck := r.carryOut(ctx, fsc, fleet, cfg, nodes, &decision, status, now)
	recheck = minPositive(recheck, r.forceKeptCallsFailover(ctx, fsc, pods, status.LastFailover, now))

	suspect, failed, verdictRecheck := r.attachHealth(fsc, verdicts, decision, status)
	recheck = minPositive(recheck, verdictRecheck)
	inProgress := failoverInProgress(status.LastFailover)
	meta.SetStatusCondition(&status.Conditions, membersHealthyCondition(fsc.Generation, suspect, failed, status.LastFailover))
	recordHealth(fleet, verdicts, decision.held, inProgress)

	if len(suspect) > 0 || len(failed) > 0 || inProgress || decision.target != nil {
		recheck = minPositive(recheck, healthRecheck)
	}
	return recheck, decision.err
}

// carryOut records a decision's failover, then fences and deletes. It returns how soon to look again, or zero. A
// failover decided from a stale copy of the fleet is dropped (target nil) and decided again in a second. An error is
// kept in the decision (err), for the member's health to report.
//
// A member a person fails over, with spec.recovery, on a node whose kubelet is up, keeps its calls: keepCallsSettle
// after the record, its pod is deleted gracefully, and its drain, seeing the fail-over annotation, stops FreeSWITCH
// without hanging them up (drainScript), for the replacement to recover. Any other failover deletes the pod at once: an automatic one is of
// a member whose calls are lost already, or whose kubelet can't run a drain, or of a pod already shutting down: its
// drain began without the annotation, and so hangs up its calls.
func (r *FreeswitchClusterReconciler) carryOut(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, fleet fleetRef, cfg settings, nodes map[string]*corev1.Node, d *failoverDecision, status *sipv1alpha1.FreeswitchClusterStatus, now time.Time) time.Duration {
	t := d.target
	if t == nil {
		return 0
	}
	log := logf.FromContext(ctx)
	if !d.resume {
		// The record goes in first: once it is in, a crash or an error part way through only resumes this failover,
		// and never starts another. It is written only over the fleet this decision was made from.
		rec := &sipv1alpha1.FailoverRecord{
			Pod: t.Name, PodUID: string(t.UID), Node: t.Spec.NodeName, Trigger: d.trigger,
			Reason: truncate(d.reason, 1024), StartedAt: metav1.NewTime(now),
		}
		switch err := r.persistFailover(ctx, fsc, rec); {
		case apierrors.IsConflict(err):
			// Decided from a copy of the fleet the cache hadn't caught up with, which may not hold the last
			// failover: decide again from a fresh one.
			log.V(1).Info("fleet changed since it was read; deciding its failover again", "pod", t.Name)
			d.target = nil
			return time.Second
		case err != nil:
			recordFailover(fleet, d.trigger, err)
			d.err = fmt.Errorf("recording the failover of %s: %w", t.Name, err)
			return 0
		default:
			status.LastFailover = rec.DeepCopy()
		}
	}
	keepCalls := d.trigger == sipv1alpha1.FailoverTriggerManual && cfg.recovery && r.nodeUp(nodes[t.Spec.NodeName], now) &&
		t.DeletionTimestamp == nil
	if keepCalls && status.LastFailover != nil {
		// The drain learns of the failover from the annotation in a downward-API file, which the kubelet stops
		// refreshing once the pod is being deleted: give it time to write the annotation first. The pod's calls go on
		// meanwhile, on its node, still unfenced.
		if wait := status.LastFailover.StartedAt.Add(keepCallsSettle).Sub(now); wait > 0 {
			return wait
		}
	}
	deleted, err := r.failOver(ctx, fsc, t, d.trigger, d.reason, keepCalls, now)
	if err != nil {
		recordFailover(fleet, d.trigger, err)
		r.event(fsc, corev1.EventTypeWarning, "FailoverFailed", "Failover", "could not fail over %s (retrying): %v", t.Name, err)
		d.err = fmt.Errorf("failing over %s: %w", t.Name, err)
		return 0
	}
	what := "deleted the pod to start it on another node"
	if keepCalls {
		what = "deleted the pod, whose drain stops FreeSWITCH without hanging up its calls, for its replacement on " +
			"another node to recover"
	}
	switch {
	case !deleted && d.resume:
		// Carried out already: the pod is gone, and the cache that still listed it hadn't caught up.
		log.V(1).Info("failover already carried out", "pod", t.Name)
		return 0
	case !deleted:
		// A new failover all the same (recorded, and the node fenced), of a pod something else had deleted.
		what = "the pod was already gone, so it starts on another node"
	}
	log.Info("failed over member", "pod", t.Name, "node", t.Spec.NodeName, "trigger", d.trigger, "reason", d.reason,
		"resumed", d.resume, "deleted", deleted, "keepCalls", keepCalls)
	recordFailover(fleet, d.trigger, nil)
	r.event(fsc, corev1.EventTypeWarning, "FailingOver", "Failover", "%s failover of %s: fenced node %s and %s; %s",
		d.trigger, t.Name, t.Spec.NodeName, what, truncate(d.reason, 512))
	return 0
}

// podNodes fetches the nodes the pods run on, by name. A node that is gone is left out.
func podNodes(ctx context.Context, c client.Reader, pods []corev1.Pod) (map[string]*corev1.Node, error) {
	nodes := map[string]*corev1.Node{}
	for i := range pods {
		name := pods[i].Spec.NodeName
		if name == "" || nodes[name] != nil {
			continue
		}
		var n corev1.Node
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		nodes[name] = &n
	}
	return nodes, nil
}

// persistFailover writes a failover's record to the status before anything is done, with a patch that conflicts if
// the fleet has changed since it was read: the decision to fail over was made from that copy, and one written since
// may hold a failover still under way. The record is what keeps another automatic failover waiting.
func (r *FreeswitchClusterReconciler) persistFailover(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, rec *sipv1alpha1.FailoverRecord) error {
	base := fsc.DeepCopy()
	fsc.Status.LastFailover = rec.DeepCopy()
	if err := r.Status().Patch(ctx, fsc, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		fsc.Status.LastFailover = base.Status.LastFailover
		return err
	}
	return nil
}

// completeFailover marks the latest failover done once a new pod of the same name is ready, or once the fleet has
// been scaled down past the pod, so that no replacement is coming.
func (r *FreeswitchClusterReconciler) completeFailover(fsc *sipv1alpha1.FreeswitchCluster, replicas int32, pods []corev1.Pod, last *sipv1alpha1.FailoverRecord, now time.Time) {
	if last == nil || last.CompletedAt != nil {
		return
	}
	if ordinal(last.Pod) >= int(replicas) && !slices.ContainsFunc(pods, func(p corev1.Pod) bool { return p.Name == last.Pod }) {
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
			if decision.err != nil {
				h.Message = "failover failed, retrying: " + truncate(decision.err.Error(), 512)
			}
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
func membersHealthyCondition(gen int64, suspect, failed []string, last *sipv1alpha1.FailoverRecord) metav1.Condition {
	cond := metav1.Condition{Type: conditionMembersHealthy, ObservedGeneration: gen, Status: metav1.ConditionFalse}
	var parts []string
	switch {
	case len(failed) > 0:
		cond.Reason, parts = "MemberFailed", []string{"Failed: " + strings.Join(failed, ", ")}
	case len(suspect) > 0:
		cond.Reason, parts = "MemberSuspect", []string{"Suspect: " + strings.Join(suspect, ", ")}
	case failoverInProgress(last):
		cond.Reason = "FailingOver"
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "AllHealthy", "no member is Suspect or Failed"
		return cond
	}
	if failoverInProgress(last) {
		parts = append(parts, "waiting for "+last.Pod+" to be ready again after its failover")
	}
	cond.Message = strings.Join(parts, "; ")
	return cond
}

// failOver fences the pod's node, then deletes the pod: at once (no drain: its calls are lost anyway), or with
// keepCalls gracefully, for its drain to stop FreeSWITCH without hanging up its calls (see carryOut). The
// StatefulSet starts it again, and the fenced label keeps it off the fenced node. deleted is false when the pod was
// already gone: deleted, or already replaced by a new pod of the same name (the UID precondition conflicts).
func (r *FreeswitchClusterReconciler) failOver(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, p *corev1.Pod, trigger sipv1alpha1.FailoverTrigger, reason string, keepCalls bool, now time.Time) (deleted bool, err error) {
	if p.Spec.NodeName != "" {
		by := fmt.Sprintf("%s failover of FreeswitchCluster %s/%s pod %s at %s: %s",
			trigger, fsc.Namespace, fsc.Name, p.Name, now.UTC().Format(time.RFC3339), reason)
		if err := fenceNode(ctx, r.apiReader(), r.Client, p.Spec.NodeName, truncate(by, 1024)); err != nil {
			return false, fmt.Errorf("fencing node %s: %w", p.Spec.NodeName, err)
		}
	}
	uid := p.UID
	opts := []client.DeleteOption{client.Preconditions{UID: &uid}}
	if !keepCalls {
		opts = append(opts, client.GracePeriodSeconds(0))
	}
	switch err := r.Delete(ctx, p, opts...); {
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("deleting pod: %w", err)
	}
	return true, nil
}

// keepCallsSettle is how long a failover that keeps its member's calls waits after it is recorded before it fences
// the node and deletes the pod, for the kubelet to write the pod's fail-over annotation to the file its drain reads
// (within a second on kind). A file the kubelet hasn't updated by the delete never is: the drain then misses the
// failover, and drains as for any delete.
const keepCallsSettle = 5 * time.Second

// keepCallsWait is how long the pod of a member failed over with its calls kept (see carryOut) has to stop on its
// own after its graceful delete. Its drain stops FreeSWITCH within seconds; one still there after this didn't (a
// FreeSWITCH that doesn't answer, a drain that missed the annotation), and is deleted at once, calls and all.
const keepCallsWait = 30 * time.Second

// forceKeptCallsFailover deletes at once the pod of the latest failover when it was deleted gracefully, to keep its
// calls, and is still there keepCallsWait later. A pod shutting down since before its failover wasn't deleted to keep
// its calls: its failover is resumed instead (decideFailover). It returns how soon to look again, or zero.
func (r *FreeswitchClusterReconciler) forceKeptCallsFailover(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, pods []corev1.Pod, last *sipv1alpha1.FailoverRecord, now time.Time) time.Duration {
	if last == nil || last.CompletedAt != nil {
		return 0
	}
	for i := range pods {
		p := &pods[i]
		if p.Name != last.Pod || string(p.UID) != last.PodUID || p.DeletionTimestamp == nil ||
			ptr.Deref(p.DeletionGracePeriodSeconds, 0) == 0 || drainStart(p).Before(last.StartedAt.Time) {
			continue
		}
		if wait := drainStart(p).Add(keepCallsWait).Sub(now); wait > 0 {
			return wait
		}
		uid := p.UID
		err := r.Delete(ctx, p, client.Preconditions{UID: &uid}, client.GracePeriodSeconds(0))
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			logf.FromContext(ctx).Error(err, "could not delete a failed-over pod that didn't stop", "pod", p.Name)
			return healthRecheck
		}
		r.event(fsc, corev1.EventTypeWarning, "FailoverForced", "Failover",
			"%s didn't stop within %s of its failover, which was to keep its calls: deleted the pod at once, and its calls with it",
			p.Name, keepCallsWait)
		return 0
	}
	return 0
}

// nodeUp reports whether the node's kubelet is up: the node Ready, and its lease not lapsed.
func (r *FreeswitchClusterReconciler) nodeUp(n *corev1.Node, now time.Time) bool {
	if !nodeReady(n) {
		return false
	}
	_, lapsed := nodeLeaseSignal(n.Name, r.NodeLeases, now)
	return !lapsed
}

// nodeReady reports whether the node's kubelet says it is Ready.
func nodeReady(n *corev1.Node) bool {
	if n == nil {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *FreeswitchClusterReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// fenceNode labels a node sip.instruments.com/fenced=true, which marks it fenced (fleets keep off it by node affinity,
// and pools treat it as failed), taints it sip.instruments.com/fenced:NoSchedule to keep other pods off, and records
// why. A node already fenced (labeled) keeps its first record. A node
// that is gone needs no fence. The node is read from reader, the API server rather than a cache where it can: a node
// just lost is being changed by the node lifecycle controller, and a stale copy only conflicts.
func fenceNode(ctx context.Context, reader client.Reader, c client.Client, name, by string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var n corev1.Node
		if err := reader.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
			return client.IgnoreNotFound(err)
		}
		tainted := slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool { return t.Key == sipv1alpha1.FencedTaint })
		labeled := nodeFenced(&n)
		if tainted && labeled {
			return nil
		}
		// The node lifecycle controller changes taints too (unreachable, not-ready): don't overwrite its change.
		patch := client.MergeFromWithOptions(n.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if !tainted {
			n.Spec.Taints = append(n.Spec.Taints, fencedTaint)
		}
		if !labeled {
			metav1.SetMetaDataLabel(&n.ObjectMeta, sipv1alpha1.FencedLabel, fencedValue)
			metav1.SetMetaDataAnnotation(&n.ObjectMeta, sipv1alpha1.FencedByAnnotation, by)
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

// avoidFencedNodes keeps a fleet's pods off fenced nodes by node affinity against the FencedLabel. The taint alone
// wouldn't: a fleet's tolerations, or ones added at admission, may tolerate it.
func avoidFencedNodes() *corev1.NodeAffinity {
	return &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: sipv1alpha1.FencedLabel, Operator: corev1.NodeSelectorOpDoesNotExist,
		}}}},
	}}
}
