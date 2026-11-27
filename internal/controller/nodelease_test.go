package controller

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

func nodeLease(node string, renewed time.Time, seconds int32) coordinationv1.Lease {
	return coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: node, Namespace: NodeLeaseNamespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: ptr.To(node), LeaseDurationSeconds: ptr.To(seconds), RenewTime: &metav1.MicroTime{Time: renewed},
		},
	}
}

// NodeLeases times renewals by its own clock, from when it sees each renewTime change, so a node's clock being off
// doesn't matter; a lease unrenewed for 3/8 of its duration lapses.
func TestNodeLeasesObserve(t *testing.T) {
	l := &NodeLeases{}
	// The nodes' clocks are an hour off: only whether renewTime changes counts.
	skewed := t0.Add(-time.Hour)
	if changed := l.observe([]coordinationv1.Lease{nodeLease("a", skewed, 40), nodeLease("b", skewed, 20)}, t0); len(changed) != 0 {
		t.Fatalf("first look: changed %v", changed)
	}
	if seen, grace, ok := l.Renewal("a"); !ok || !seen.Equal(t0) || grace != 15*time.Second {
		t.Fatalf("a first seen: %v %v %v", seen, grace, ok)
	}
	if _, grace, _ := l.Renewal("b"); grace != 7500*time.Millisecond {
		t.Fatalf("b's grace: %v", grace)
	}

	// b renews; a doesn't. At 8 s b is renewing and a within its grace.
	if changed := l.observe([]coordinationv1.Lease{nodeLease("a", skewed, 40), nodeLease("b", skewed.Add(5*time.Second), 20)}, t0.Add(8*time.Second)); len(changed) != 0 {
		t.Fatalf("at 8 s: changed %v", changed)
	}
	// At 15 s a has lapsed, once.
	leases := []coordinationv1.Lease{nodeLease("a", skewed, 40), nodeLease("b", skewed.Add(10*time.Second), 20)}
	if changed := l.observe(leases, t0.Add(15*time.Second)); len(changed) != 1 || changed[0] != "a" {
		t.Fatalf("at 15 s: changed %v", changed)
	}
	if changed := l.observe(leases, t0.Add(16*time.Second)); len(changed) != 0 {
		t.Fatalf("at 16 s: changed %v", changed)
	}
	// a renews again: it comes back, seen now.
	leases[0] = nodeLease("a", skewed.Add(20*time.Second), 40)
	if changed := l.observe(leases, t0.Add(20*time.Second)); len(changed) != 1 || changed[0] != "a" {
		t.Fatalf("at 20 s: changed %v", changed)
	}
	if seen, _, _ := l.Renewal("a"); !seen.Equal(t0.Add(20 * time.Second)) {
		t.Fatalf("a renewed: seen %v", seen)
	}
	// A lease that's gone is forgotten.
	l.observe(leases[:1], t0.Add(21*time.Second))
	if _, _, ok := l.Renewal("b"); ok {
		t.Fatal("b's lease is gone, yet remembered")
	}
}

// When a node's lease lapses, each fleet with a pod on it is sent on Events, once.
func TestNodeLeasesSendsFleetsOnLapse(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = sipv1alpha1.AddToScheme(scheme)
	pod := func(name, fleet, node string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "telephony", Labels: map[string]string{}}, Spec: corev1.PodSpec{NodeName: node}}
		if fleet != "" {
			p.Labels[sipv1alpha1.ClusterLabel] = fleet
		}
		return p
	}
	lease := nodeLease("b", t0, 40)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1.Pod{}, podNodeNameField, func(o client.Object) []string { return []string{o.(*corev1.Pod).Spec.NodeName} }).
		WithObjects(&lease, pod("edge-0", "edge", "a"), pod("edge-1", "edge", "b"), pod("other-0", "other", "b"), pod("kamailio-0", "", "b")).
		Build()
	now := t0
	l := &NodeLeases{Client: c, Events: make(chan event.GenericEvent, 8), Now: func() time.Time { return now }}
	ctx := context.Background()

	l.tick(ctx)
	now = t0.Add(10 * time.Second)
	l.tick(ctx)
	if len(l.Events) != 0 {
		t.Fatalf("within the grace: %d events", len(l.Events))
	}
	now = t0.Add(15 * time.Second)
	l.tick(ctx)
	now = t0.Add(16 * time.Second)
	l.tick(ctx)
	got := map[string]bool{}
	if n := len(l.Events); n != 2 {
		t.Errorf("%d events, want one per fleet", n)
	}
	for len(l.Events) > 0 {
		ev := <-l.Events
		got[ev.Object.GetNamespace()+"/"+ev.Object.GetName()] = true
	}
	if len(got) != 2 || !got["telephony/edge"] || !got["telephony/other"] {
		t.Errorf("fleets sent: %v", got)
	}
}
