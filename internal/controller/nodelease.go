package controller

import (
	"context"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

// NodeLeaseNamespace is where each kubelet keeps its node's Lease, named for the node.
const NodeLeaseNamespace = corev1.NamespaceNodeLease

// nodeLeaseTick is how often NodeLeases reads the nodes' leases.
const nodeLeaseTick = time.Second

// defaultNodeLeaseDuration is the kubelet's default nodeLeaseDurationSeconds, for a lease that doesn't say.
const defaultNodeLeaseDuration = 40 * time.Second

// nodeLeaseGrace is how long a node's lease may go unrenewed before the node counts as lost: one and a half of the
// kubelet's renew intervals, which the kubelet makes a quarter of the lease's duration (15 s of the default 40 s).
// One renewal missed, and half an interval more for a slow one.
func nodeLeaseGrace(duration time.Duration) time.Duration { return duration * 3 / 8 }

// NodeLeaseResults reads what NodeLeases knows of each node's lease.
type NodeLeaseResults interface {
	// Renewal returns when the node's lease was last seen renewed, by the operator's clock, and how long it may then
	// go unrenewed before the node counts as lost. ok is false for a node whose lease it hasn't seen.
	Renewal(node string) (seen time.Time, grace time.Duration, ok bool)
}

// NodeLeases follows each node's Lease, which its kubelet renews every quarter of the lease's duration (10 s by
// default), and notes when it sees each renewal by its own clock: a node's clock may be off, so the lease's own
// renewTime is only compared with itself. It is the operator's quicker view of a lost node: the node lifecycle
// controller waits its node-monitor-grace-period (40 s to 50 s, and not to be changed on a managed control plane)
// after the last renewal before it marks the node NotReady. When a node's lease lapses, or is renewed again after
// lapsing, it sends each fleet with a pod on the node on Events, so its health is decided again at once. It runs on
// the leader only. After a restart it counts each lease as renewed when it first sees it, which only delays a
// verdict.
type NodeLeases struct {
	Client client.Reader
	// Events receives a fleet (a FreeswitchCluster with only its name and namespace) with a pod on a node whose lease
	// lapsed or came back.
	Events chan event.GenericEvent
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	nodes map[string]*leaseState
}

type leaseState struct {
	renewTime metav1.MicroTime // the lease's renewTime when last seen to change
	seen      time.Time        // when that was, by the operator's clock
	grace     time.Duration
	lapsed    bool
}

func (l *NodeLeases) now() time.Time { return nowFrom(l.Now) }

// Renewal implements NodeLeaseResults.
func (l *NodeLeases) Renewal(node string) (time.Time, time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.nodes[node]
	if !ok {
		return time.Time{}, 0, false
	}
	return s.seen, s.grace, true
}

// Start implements manager.Runnable.
func (l *NodeLeases) Start(ctx context.Context) error {
	t := time.NewTicker(nodeLeaseTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			l.tick(ctx)
		}
	}
}

// tick reads the nodes' leases, notes renewals, and sends the fleets on nodes whose lease lapsed or came back.
func (l *NodeLeases) tick(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("node-leases")
	var leases coordinationv1.LeaseList
	if err := l.Client.List(ctx, &leases, client.InNamespace(NodeLeaseNamespace), client.UnsafeDisableDeepCopy); err != nil {
		log.Error(err, "listing node leases")
		return
	}
	changed := l.observe(leases.Items, l.now())
	for _, node := range changed {
		l.sendFleetsOn(ctx, node)
	}
}

// observe notes the leases as seen at now, forgets nodes whose lease has gone, and returns the nodes whose lease
// lapsed or came back since the last look.
func (l *NodeLeases) observe(leases []coordinationv1.Lease, now time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nodes == nil {
		l.nodes = map[string]*leaseState{}
	}
	live := make(map[string]bool, len(leases))
	var changed []string
	for i := range leases {
		ls := &leases[i]
		live[ls.Name] = true
		renew := ptr.Deref(ls.Spec.RenewTime, metav1.MicroTime{})
		grace := nodeLeaseGrace(defaultNodeLeaseDuration)
		if d := ptr.Deref(ls.Spec.LeaseDurationSeconds, 0); d > 0 {
			grace = nodeLeaseGrace(time.Duration(d) * time.Second)
		}
		s := l.nodes[ls.Name]
		if s == nil {
			l.nodes[ls.Name] = &leaseState{renewTime: renew, seen: now, grace: grace}
			continue
		}
		s.grace = grace
		if !renew.Equal(&s.renewTime) {
			s.renewTime, s.seen = renew, now
		}
		if lapsed := now.Sub(s.seen) >= s.grace; lapsed != s.lapsed {
			s.lapsed = lapsed
			changed = append(changed, ls.Name)
		}
	}
	for node := range l.nodes {
		if !live[node] {
			delete(l.nodes, node)
		}
	}
	return changed
}

// sendFleetsOn sends on Events each fleet with a pod on node.
func (l *NodeLeases) sendFleetsOn(ctx context.Context, node string) {
	if l.Events == nil {
		return
	}
	var pods corev1.PodList
	if err := l.Client.List(ctx, &pods, client.MatchingFields{podNodeNameField: node}, client.UnsafeDisableDeepCopy); err != nil {
		logf.FromContext(ctx).WithName("node-leases").Error(err, "listing pods on a node whose lease changed", "node", node)
		return
	}
	seen := map[fleetRef]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		name := p.Labels[sipv1alpha1.ClusterLabel]
		ref := fleetRef{kind: kindFreeswitch, namespace: p.Namespace, name: name}
		if name == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		ev := event.GenericEvent{Object: &sipv1alpha1.FreeswitchCluster{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: name}}}
		select {
		case l.Events <- ev:
		case <-ctx.Done():
			return
		}
	}
}
