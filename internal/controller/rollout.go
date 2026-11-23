package controller

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podRevision(p *corev1.Pod) string {
	return p.Labels[appsv1.ControllerRevisionHashLabelKey]
}

func memberState(p *corev1.Pod) sipv1alpha1.MemberState {
	switch {
	case p.DeletionTimestamp != nil:
		return sipv1alpha1.MemberDraining
	case podReady(p):
		return sipv1alpha1.MemberActive
	default:
		return sipv1alpha1.MemberStarting
	}
}

// ordinal returns a StatefulSet pod's ordinal, or -1.
func ordinal(p *corev1.Pod) int {
	i := strings.LastIndex(p.Name, "-")
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(p.Name[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// rolloutDecision is what the rolling-update loop should do next.
type rolloutDecision struct {
	// Replace is the pod to delete now, or nil.
	Replace *corev1.Pod
	// Outdated is the number of pods not on the update revision.
	Outdated int
	// Waiting explains why nothing is replaced while pods are outdated.
	Waiting string
}

// nextPodToReplace applies the fleet's one rule for planned disruption: replace one outdated pod at a time, and
// only when every other pod is ready and none is terminating. An outdated pod that is not ready is replaced
// first, since it takes no calls anyway. Among ready outdated pods the highest ordinal goes first, as a
// StatefulSet would.
func nextPodToReplace(pods []corev1.Pod, updateRevision string, desired int32) rolloutDecision {
	if updateRevision == "" {
		return rolloutDecision{}
	}
	var outdated []*corev1.Pod
	for i := range pods {
		if podRevision(&pods[i]) != updateRevision {
			outdated = append(outdated, &pods[i])
		}
	}
	d := rolloutDecision{Outdated: len(outdated)}
	if len(outdated) == 0 {
		return d
	}
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			d.Waiting = "pod " + pods[i].Name + " is draining"
			return d
		}
	}
	if int32(len(pods)) < desired {
		d.Waiting = "waiting for the fleet to reach its replica count"
		return d
	}
	// Prefer an outdated pod that is not ready. Otherwise every other pod must be ready.
	slices.SortFunc(outdated, func(a, b *corev1.Pod) int { return cmp.Compare(ordinal(b), ordinal(a)) })
	for _, p := range outdated {
		if !podReady(p) {
			d.Replace = p
			return d
		}
	}
	for i := range pods {
		if !podReady(&pods[i]) {
			d.Waiting = "pod " + pods[i].Name + " is not ready"
			return d
		}
	}
	d.Replace = outdated[0]
	return d
}

// fleetMembers describes each pod, and counts those that take calls and those on updateRevision.
func fleetMembers(pods []corev1.Pod, updateRevision string) (members []sipv1alpha1.Member, ready, updated int32) {
	members = make([]sipv1alpha1.Member, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		m := sipv1alpha1.Member{
			Pod:             p.Name,
			Node:            p.Spec.NodeName,
			Address:         p.Status.PodIP,
			ExternalAddress: p.Annotations[sipv1alpha1.ExternalAddressAnnotation],
			State:           memberState(p),
			Revision:        podRevision(p),
		}
		if m.State == sipv1alpha1.MemberActive {
			ready++
		}
		if updateRevision != "" && m.Revision == updateRevision {
			updated++
		}
		members = append(members, m)
	}
	return members, ready, updated
}

// setFleetConditions sets the conditions both kinds of fleet report: Available, Progressing, Degraded and Ready.
func setFleetConditions(conds *[]metav1.Condition, gen int64, ready, replicas int32, decision rolloutDecision) {
	if ready > 0 {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionAvailable, Status: metav1.ConditionTrue, Reason: "MembersReady", Message: fmt.Sprintf("%d of %d members take calls", ready, replicas), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionAvailable, Status: metav1.ConditionFalse, Reason: "NoMembersReady", Message: "no member takes calls", ObservedGeneration: gen})
	}
	if decision.Outdated > 0 {
		msg := fmt.Sprintf("%d members on an older revision", decision.Outdated)
		if decision.Waiting != "" {
			msg += "; " + decision.Waiting
		}
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionProgressing, Status: metav1.ConditionTrue, Reason: "RollingUpdate", Message: msg, ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionProgressing, Status: metav1.ConditionFalse, Reason: "UpToDate", Message: "every member runs the latest revision", ObservedGeneration: gen})
	}
	if ready < replicas {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionTrue, Reason: "MembersNotReady", Message: fmt.Sprintf("%d of %d members ready", ready, replicas), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionFalse, Reason: "AllMembersReady", Message: "every member is ready", ObservedGeneration: gen})
	}
	// Ready is the summary GitOps tools (Flux health checks, kstatus) wait on: the whole fleet ready and current.
	if ready >= replicas && decision.Outdated == 0 {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionReady, Status: metav1.ConditionTrue, Reason: "FleetReady", Message: "every member is ready and current", ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionReady, Status: metav1.ConditionFalse, Reason: "FleetNotReady", Message: fmt.Sprintf("%d of %d members ready, %d outdated", ready, replicas, decision.Outdated), ObservedGeneration: gen})
	}
}

// nodeExternalAddress returns the node's first ExternalIP address.
func nodeExternalAddress(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeExternalIP {
			return a.Address
		}
	}
	return ""
}
