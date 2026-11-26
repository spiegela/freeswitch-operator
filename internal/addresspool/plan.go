package addresspool

import (
	"cmp"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

// Node is a cluster node as the planner sees it.
type Node struct {
	// Name of the Node object.
	Name string
	// ID is the provider's name for the node, or empty when the provider can't name it (no providerID yet).
	ID string
}

// Claimant is a pod that uses the pool, on the node it runs on. Terminating pods still claim: a draining pod has
// live calls on its address.
type Claimant struct {
	Claim sipv1alpha1.AddressClaim
	Node  string
}

// Input is everything the planner decides from.
type Input struct {
	// Observed is the provider's report, in spec order.
	Observed []Address
	// Nodes are the cluster's nodes.
	Nodes []Node
	// Claimants are the pods using the pool.
	Claimants []Claimant
	// Previous is the pool's status before this pass: who claimed each address last, and where the pool bound it.
	Previous []sipv1alpha1.PoolAddress
	// NodeAddresses are the bind addresses each node's address agent reports on the node's interface. A node
	// missing from it has no report (no agent, or a new one before its first pass): its addresses keep the
	// readiness they had.
	NodeAddresses map[string][]string
	// Refused are nodes the pool must not bind a new address to, with why (an AWS Secondary pool on a node whose VPC
	// CNI manages the same interface's secondary addresses). They wait; an address already bound there stays.
	Refused map[string]string
	// Fenced are nodes a failover has fenced (the sip.instruments.com/fenced label). Their pods' claims don't count,
	// they are given no address, and the addresses on them are taken off when MayFence, and held there otherwise.
	Fenced map[string]bool
	// MayFence says the pool takes its addresses off fenced nodes: its provider's fencing is Authoritative, or the
	// pool allows Advisory fencing.
	MayFence bool
}

// Step is one provider call. A move is two steps on one address: Unbind, then Bind.
type Step struct {
	Unbind  bool
	Address Address
	// NodeID and Node are the target of a Bind.
	NodeID string
	Node   string
}

// Plan is what to do, and the status the pool has once it is done.
type Plan struct {
	// Steps grouped by address, each group in order. A failed step abandons the rest of its group.
	Steps [][]Step
	// Status is the pool's addresses once every step has succeeded, in spec order.
	Status []sipv1alpha1.PoolAddress
	// Waiting are nodes running a pod that uses the pool which get no address, with why.
	Waiting map[string]string
}

// Decide plans one pass. Its rules, whatever the provider:
//   - An address bound to a cluster node stays there while the node lives; it is Bound when a pod on the node
//     uses the pool and Idle otherwise (a spare). One address per node: extra ones on a node are Idle.
//   - An address bound outside the cluster is unbound when the pool put it there (its node has gone) and left
//     alone, as a Conflict, when it didn't.
//   - A node with a pod using the pool and no address gets one: first a Free or Idle address one of its pods used
//     last, then any Free address, then any Idle one (taken off its idle node), each in spec order.
//   - A fenced node is treated as failed: its pods' claims don't count and it gets no address. An address on it is
//     unbound when the pool may fence, and otherwise left there, Fenced, never moved to another node, since the
//     node may still answer for it.
func Decide(in Input) Plan {
	pl := newPlanner(in)
	for i, a := range in.Observed {
		pl.observe(i, a)
	}
	pl.assign()

	plan := Plan{Waiting: pl.waiting, Status: pl.status}
	for i := range pl.status {
		st := &pl.status[i]
		if st.State == sipv1alpha1.PoolAddressBound {
			st.ClaimedBy = pl.claims[st.Node]
			st.LastClaimedBy = pl.claims[st.Node]
		}
	}
	pl.forgetOlderClaims()
	for i := range pl.status {
		st := &pl.status[i]
		pl.bindAddressReady(st)
		if msg := pl.notReady[i]; msg != "" {
			// Bound, but not to be handed out: the provider says it can't carry traffic yet, or this pass bound
			// it and the next read confirms it.
			st.Message, st.BindAddressReady = msg, false
			if st.State == sipv1alpha1.PoolAddressBound && len(pl.steps[i]) == 0 {
				pl.waiting[st.Node] = "its address " + st.Address + " can't be used yet: " + msg
			}
		}
		if len(pl.steps[i]) > 0 {
			plan.Steps = append(plan.Steps, pl.steps[i])
		}
	}
	return plan
}

// forgetOlderClaims keeps each pod the last claimant of one address only, the one bound to its node: a pod that
// moved to a node with an address of its own would otherwise still be the last to have used its old one, and, when
// it next needs an address, could be given that instead of the one it holds now (a free address comes before an
// idle one). An address in Error is still held: its node keeps it while the provider can't report on it. But its
// ClaimedBy is carried over from before the provider failed, so it holds a pod only while the pod is still on that
// node: one that has moved on holds what its new node has.
func (pl *planner) forgetOlderClaims() {
	held := func(st sipv1alpha1.PoolAddress) bool {
		return st.State == sipv1alpha1.PoolAddressBound || st.State == sipv1alpha1.PoolAddressError
	}
	holds := map[sipv1alpha1.AddressClaim]bool{}
	for _, st := range pl.status {
		for _, c := range st.ClaimedBy {
			switch st.State {
			case sipv1alpha1.PoolAddressBound:
				holds[c] = true
			case sipv1alpha1.PoolAddressError:
				if slices.Contains(pl.claims[st.Node], c) {
					holds[c] = true
				}
			}
		}
	}
	for i := range pl.status {
		st := &pl.status[i]
		if held(*st) || len(st.LastClaimedBy) == 0 {
			continue
		}
		last := slices.DeleteFunc(slices.Clone(st.LastClaimedBy), func(c sipv1alpha1.AddressClaim) bool { return holds[c] })
		if len(last) == 0 {
			last = nil
		}
		st.LastClaimedBy = last
	}
}

// bindAddressReady records whether the node's address agent has an address's bind address on its interface.
func (pl *planner) bindAddressReady(st *sipv1alpha1.PoolAddress) {
	if st.BindAddress == "" || st.Node == "" {
		st.BindAddressReady = false
		return
	}
	if have, ok := pl.in.NodeAddresses[st.Node]; ok {
		st.BindAddressReady = slices.Contains(have, st.BindAddress)
		return
	}
	p := pl.prev[st.ID]
	st.BindAddressReady = p.BindAddressReady && p.Node == st.Node && p.BindAddress == st.BindAddress
}

// planner is one pass of Decide in progress.
type planner struct {
	in           Input
	nodeByID     map[string]string
	nodeIDByName map[string]string
	claims       map[string][]sipv1alpha1.AddressClaim // by node, sorted
	prev         map[string]sipv1alpha1.PoolAddress    // by address ID
	status       []sipv1alpha1.PoolAddress
	steps        [][]Step
	notReady     []string        // by address: why a bound address isn't to be handed out yet
	served       map[string]bool // nodes that hold a Bound address
	waiting      map[string]string
}

func newPlanner(in Input) *planner {
	pl := &planner{
		in: in, nodeByID: map[string]string{}, nodeIDByName: map[string]string{},
		claims: map[string][]sipv1alpha1.AddressClaim{}, prev: map[string]sipv1alpha1.PoolAddress{},
		status: make([]sipv1alpha1.PoolAddress, len(in.Observed)), steps: make([][]Step, len(in.Observed)),
		notReady: make([]string, len(in.Observed)), served: map[string]bool{}, waiting: map[string]string{},
	}
	for _, n := range in.Nodes {
		if n.ID != "" {
			pl.nodeByID[n.ID] = n.Name
		}
		pl.nodeIDByName[n.Name] = n.ID
	}
	for _, c := range in.Claimants {
		if in.Fenced[c.Node] {
			continue
		}
		pl.claims[c.Node] = append(pl.claims[c.Node], c.Claim)
	}
	for n := range pl.claims {
		slices.SortFunc(pl.claims[n], compareClaims)
	}
	for _, p := range in.Previous {
		pl.prev[p.ID] = p
	}
	return pl
}

// observe records where the provider has address i, unbinding it from a node the pool put it on that has gone.
func (pl *planner) observe(i int, a Address) {
	p := pl.prev[a.ID]
	st := sipv1alpha1.PoolAddress{ID: a.ID, Address: a.IP, LastClaimedBy: p.LastClaimedBy}
	switch {
	case a.Err != nil:
		st.Address = cmp.Or(a.IP, p.Address)
		st.State, st.Node, st.NodeID, st.Message = sipv1alpha1.PoolAddressError, p.Node, p.NodeID, a.Err.Error()
		st.ClaimedBy, st.BindAddress = p.ClaimedBy, cmp.Or(a.BindIP, p.BindAddress)
		// Not knowing where an address is now is no reason to give its node another one.
		if p.Node != "" && (p.State == sipv1alpha1.PoolAddressBound || p.State == sipv1alpha1.PoolAddressError) {
			pl.served[p.Node] = true
		}
	case a.NodeID == "":
		st.State = sipv1alpha1.PoolAddressFree
	default:
		node, inCluster := pl.nodeByID[a.NodeID]
		switch {
		case inCluster && pl.in.Fenced[node] && pl.in.MayFence:
			pl.steps[i] = append(pl.steps[i], Step{Unbind: true, Address: a})
			st.State = sipv1alpha1.PoolAddressFree
		case inCluster && pl.in.Fenced[node]:
			st.State, st.Node, st.NodeID, st.BindAddress = sipv1alpha1.PoolAddressFenced, node, a.NodeID, a.BindIP
			st.Message = "on fenced node " + node + ", which may still answer for it (the provider's fencing is " +
				"Advisory): left there until the node is unfenced or leaves the cluster, or the pool sets allowAdvisoryFencing"
		case inCluster && len(pl.claims[node]) > 0 && !pl.served[node]:
			pl.served[node] = true
			st.State, st.Node, st.NodeID, st.BindAddress = sipv1alpha1.PoolAddressBound, node, a.NodeID, a.BindIP
			pl.notReady[i] = a.NotReady
		case inCluster:
			st.State, st.Node, st.NodeID, st.BindAddress = sipv1alpha1.PoolAddressIdle, node, a.NodeID, a.BindIP
			pl.notReady[i] = a.NotReady
		case p.NodeID == a.NodeID && p.State != sipv1alpha1.PoolAddressConflict:
			// The pool bound it to a node that has left the cluster. (A Conflict was never the pool's: it stays one.)
			pl.steps[i] = append(pl.steps[i], Step{Unbind: true, Address: a})
			st.State = sipv1alpha1.PoolAddressFree
		default:
			st.State, st.NodeID = sipv1alpha1.PoolAddressConflict, a.NodeID
			st.Message = fmt.Sprintf("bound to %s, which is not a node in this cluster; the pool leaves it alone", a.NodeID)
		}
	}
	pl.status[i] = st
}

// assign gives an address to each node with a pod using the pool and none yet. Two passes, so a node takes
// another's former address only once every node has had its own back.
func (pl *planner) assign() {
	// In the order of each node's first pod, so the outcome doesn't depend on map order.
	var needy []string
	for node := range pl.claims {
		if !pl.served[node] {
			needy = append(needy, node)
		}
	}
	slices.SortFunc(needy, func(a, b string) int { return compareClaims(pl.claims[a][0], pl.claims[b][0]) })

	for _, sticky := range []bool{true, false} {
		for _, node := range needy {
			if pl.served[node] {
				continue
			}
			if why, refused := pl.in.Refused[node]; refused {
				pl.waiting[node] = why
				continue
			}
			if pl.nodeIDByName[node] == "" {
				pl.waiting[node] = "the provider can't name this node yet"
				continue
			}
			if i := pl.pick(node, sticky); i >= 0 {
				pl.give(node, i)
				delete(pl.waiting, node)
			} else if !sticky {
				pl.waiting[node] = "no free or idle address in the pool"
			}
		}
	}
}

// pick returns the address to give a node, or -1: Free before Idle, in spec order, and when sticky only one a pod
// on the node used last.
func (pl *planner) pick(node string, sticky bool) int {
	for _, state := range []sipv1alpha1.PoolAddressState{sipv1alpha1.PoolAddressFree, sipv1alpha1.PoolAddressIdle} {
		for i := range pl.status {
			if pl.status[i].State == state && (!sticky || pl.usedBefore(i, node)) {
				return i
			}
		}
	}
	return -1
}

func (pl *planner) usedBefore(i int, node string) bool {
	for _, c := range pl.claims[node] {
		if slices.Contains(pl.status[i].LastClaimedBy, c) {
			return true
		}
	}
	return false
}

// give binds address i to a node, unbinding it from its idle node first.
func (pl *planner) give(node string, i int) {
	a := pl.in.Observed[i]
	if pl.status[i].State == sipv1alpha1.PoolAddressIdle {
		pl.steps[i] = append(pl.steps[i], Step{Unbind: true, Address: a})
	}
	id := pl.nodeIDByName[node]
	pl.steps[i] = append(pl.steps[i], Step{Address: a, NodeID: id, Node: node})
	// The bind address, if the provider uses one, is known once the provider has bound it: the next pass reads it,
	// and until then the node's pods aren't given the address.
	pl.status[i].State, pl.status[i].Node, pl.status[i].NodeID, pl.status[i].BindAddress = sipv1alpha1.PoolAddressBound, node, id, ""
	pl.notReady[i] = "binding to " + node + "; the provider's next read confirms it"
	pl.served[node] = true
}

func compareClaims(a, b sipv1alpha1.AddressClaim) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Pod, b.Pod))
}

// BindingForNode returns the pool's address bound to a node, ready for its pods, or nil. An address in Error that
// was last seen on the node still counts: a provider that can't answer for a while is no reason to make the node's
// pods unready. One with a bind address counts once the node has it (BindAddressReady). A Bound one with a message
// doesn't count yet: the provider can't use it, or hasn't confirmed binding it.
func BindingForNode(status *sipv1alpha1.SIPAddressPoolStatus, node string) *sipv1alpha1.PoolAddress {
	for i, a := range status.Addresses {
		if a.Node != node || len(a.ClaimedBy) == 0 || (a.BindAddress != "" && !a.BindAddressReady) ||
			(a.State == sipv1alpha1.PoolAddressBound && a.Message != "") {
			continue
		}
		if a.State == sipv1alpha1.PoolAddressBound || a.State == sipv1alpha1.PoolAddressError {
			return &status.Addresses[i]
		}
	}
	return nil
}

// AwaitingAgent says whether an address is bound for pods but its node's address agent doesn't report its bind
// address yet (and nothing else holds it back: see BindingForNode).
func AwaitingAgent(a *sipv1alpha1.PoolAddress) bool {
	return a.State == sipv1alpha1.PoolAddressBound && a.BindAddress != "" && !a.BindAddressReady && a.Message == ""
}

// NodeBindAddresses returns the bind addresses a pool gives a node's interface: those of its addresses on the node
// that are bound, idle, fenced or unreadable, claimed or not, so a node keeps an address while a provider can't answer
// and has it before a pod claims it.
func NodeBindAddresses(status *sipv1alpha1.SIPAddressPoolStatus, node string) []string {
	var out []string
	for _, a := range status.Addresses {
		if a.Node != node || a.BindAddress == "" {
			continue
		}
		switch a.State {
		case sipv1alpha1.PoolAddressBound, sipv1alpha1.PoolAddressIdle, sipv1alpha1.PoolAddressFenced, sipv1alpha1.PoolAddressError:
			out = append(out, a.BindAddress)
		}
	}
	return out
}

// ProviderRead says whether a pool's status comes from a successful read of its provider, so that an address
// missing from it is really gone.
func ProviderRead(status *sipv1alpha1.SIPAddressPoolStatus) bool {
	return meta.IsStatusConditionTrue(status.Conditions, sipv1alpha1.PoolConditionProviderAvailable)
}
