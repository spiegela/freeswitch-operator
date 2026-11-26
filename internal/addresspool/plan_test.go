package addresspool

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

func claim(pod string) sipv1alpha1.AddressClaim {
	return sipv1alpha1.AddressClaim{Kind: "FreeswitchCluster", Namespace: "telephony", Name: "edge", Pod: pod}
}

func claimant(pod, node string) Claimant { return Claimant{Claim: claim(pod), Node: node} }

func nodes(names ...string) []Node {
	out := make([]Node, 0, len(names))
	for _, n := range names {
		out = append(out, Node{Name: n, ID: "i-" + n})
	}
	return out
}

func addr(id, nodeID string) Address {
	a := Address{ID: id, IP: "203.0.113." + strings.TrimPrefix(id, "eip"), NodeID: nodeID}
	if nodeID != "" {
		a.BindingID = "assoc-" + id
	}
	return a
}

// summary renders a plan as "steps | id:state@node ..." for compact comparison.
func summary(p Plan) string {
	var steps []string
	for _, group := range p.Steps {
		for _, s := range group {
			if s.Unbind {
				steps = append(steps, "unbind "+s.Address.ID)
			} else {
				steps = append(steps, fmt.Sprintf("bind %s %s", s.Address.ID, s.Node))
			}
		}
	}
	st := make([]string, 0, len(p.Status))
	for _, a := range p.Status {
		s := a.ID + ":" + string(a.State)
		if a.Node != "" {
			s += "@" + a.Node
		}
		st = append(st, s)
	}
	return strings.Join(steps, ", ") + " | " + strings.Join(st, " ")
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		in      Input
		want    string
		waiting []string
	}{
		{
			name: "fresh pool: each node with a pod gets the next free address",
			in: Input{
				Observed:  []Address{addr("eip1", ""), addr("eip2", ""), addr("eip3", "")},
				Nodes:     nodes("a", "b", "spare"),
				Claimants: []Claimant{claimant("edge-1", "b"), claimant("edge-0", "a")},
			},
			want: "bind eip1 a, bind eip2 b | eip1:Bound@a eip2:Bound@b eip3:Free",
		},
		{
			name: "steady state: nothing to do",
			in: Input{
				Observed:  []Address{addr("eip1", "i-a"), addr("eip2", "i-b"), addr("eip3", "")},
				Nodes:     nodes("a", "b"),
				Claimants: []Claimant{claimant("edge-0", "a"), claimant("edge-1", "b")},
			},
			want: " | eip1:Bound@a eip2:Bound@b eip3:Free",
		},
		{
			name: "a node without pods keeps its address, idle",
			in: Input{
				Observed:  []Address{addr("eip1", "i-a"), addr("eip2", "i-b")},
				Nodes:     nodes("a", "b"),
				Claimants: []Claimant{claimant("edge-0", "a")},
			},
			want: " | eip1:Bound@a eip2:Idle@b",
		},
		{
			name: "a terminating (draining) pod still holds its node's address",
			in: Input{
				Observed:  []Address{addr("eip1", "i-a")},
				Nodes:     nodes("a"),
				Claimants: []Claimant{claimant("edge-0", "a")},
			},
			want: " | eip1:Bound@a",
		},
		{
			name: "replaced node: the pool's address on a vanished node is unbound and goes to the pod's new node",
			in: Input{
				Observed:  []Address{addr("eip1", "i-a"), addr("eip2", "i-old"), addr("eip3", "")},
				Nodes:     nodes("a", "new"),
				Claimants: []Claimant{claimant("edge-0", "a"), claimant("edge-1", "new")},
				Previous: []sipv1alpha1.PoolAddress{
					{ID: "eip2", State: sipv1alpha1.PoolAddressBound, Node: "old", NodeID: "i-old", LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-1")}},
				},
			},
			want: "unbind eip2, bind eip2 new | eip1:Bound@a eip2:Bound@new eip3:Free",
		},
		{
			name: "a pod back on a new node gets its old free address over an earlier free one",
			in: Input{
				Observed:  []Address{addr("eip1", ""), addr("eip2", "")},
				Nodes:     nodes("new"),
				Claimants: []Claimant{claimant("edge-1", "new")},
				Previous:  []sipv1alpha1.PoolAddress{{ID: "eip2", State: sipv1alpha1.PoolAddressFree, LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-1")}}},
			},
			want: "bind eip2 new | eip1:Free eip2:Bound@new",
		},
		{
			name: "a node never takes another pod's former address while that pod is waiting too",
			in: Input{
				Observed:  []Address{addr("eip1", ""), addr("eip2", "")},
				Nodes:     nodes("x", "y"),
				Claimants: []Claimant{claimant("edge-0", "x"), claimant("edge-1", "y")},
				Previous:  []sipv1alpha1.PoolAddress{{ID: "eip1", State: sipv1alpha1.PoolAddressFree, LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-1")}}},
			},
			want: "bind eip1 y, bind eip2 x | eip1:Bound@y eip2:Bound@x",
		},
		{
			name: "no free address: an idle one is moved off its spare node",
			in: Input{
				Observed:  []Address{addr("eip1", "i-a"), addr("eip2", "i-spare")},
				Nodes:     nodes("a", "spare", "c"),
				Claimants: []Claimant{claimant("edge-0", "a"), claimant("edge-1", "c")},
			},
			want: "unbind eip2, bind eip2 c | eip1:Bound@a eip2:Bound@c",
		},
		{
			name: "exhausted: the node waits",
			in: Input{
				Observed:  []Address{addr("eip1", "i-a")},
				Nodes:     nodes("a", "b"),
				Claimants: []Claimant{claimant("edge-0", "a"), claimant("edge-1", "b")},
			},
			want:    " | eip1:Bound@a",
			waiting: []string{"b"},
		},
		{
			name: "bound outside the cluster without the pool putting it there: a conflict, left alone",
			in: Input{
				Observed:  []Address{addr("eip1", "i-someone-else"), addr("eip2", "")},
				Nodes:     nodes("a"),
				Claimants: []Claimant{claimant("edge-0", "a")},
			},
			want: "bind eip2 a | eip1:Conflict eip2:Bound@a",
		},
		{
			name: "one address per node: a second one on the same node is idle",
			in: Input{
				Observed:  []Address{addr("eip1", "i-a"), addr("eip2", "i-a")},
				Nodes:     nodes("a"),
				Claimants: []Claimant{claimant("edge-0", "a")},
			},
			want: " | eip1:Bound@a eip2:Idle@a",
		},
		{
			name: "the provider can't report on a bound address: its node gets no other",
			in: Input{
				Observed:  []Address{{ID: "eip1", Err: errors.New("throttled")}, addr("eip2", "")},
				Nodes:     nodes("a"),
				Claimants: []Claimant{claimant("edge-0", "a")},
				Previous:  []sipv1alpha1.PoolAddress{{ID: "eip1", Address: "203.0.113.1", State: sipv1alpha1.PoolAddressBound, Node: "a", NodeID: "i-a", ClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}}},
			},
			want: " | eip1:Error@a eip2:Free",
		},
		{
			name: "a node the provider can't name waits",
			in: Input{
				Observed:  []Address{addr("eip1", "")},
				Nodes:     []Node{{Name: "a"}},
				Claimants: []Claimant{claimant("edge-0", "a")},
			},
			want:    " | eip1:Free",
			waiting: []string{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Decide(tc.in)
			if got := summary(p); got != tc.want {
				t.Errorf("plan\n got: %s\nwant: %s", got, tc.want)
			}
			var waiting []string
			for n := range p.Waiting {
				waiting = append(waiting, n)
			}
			slices.Sort(waiting)
			if !slices.Equal(waiting, tc.waiting) {
				t.Errorf("waiting = %v, want %v", waiting, tc.waiting)
			}
		})
	}
}

func TestDecideRecordsClaims(t *testing.T) {
	p := Decide(Input{
		Observed:  []Address{addr("eip1", "")},
		Nodes:     nodes("a"),
		Claimants: []Claimant{claimant("edge-0", "a")},
	})
	got := p.Status[0]
	if len(got.ClaimedBy) != 1 || got.ClaimedBy[0].Pod != "edge-0" || len(got.LastClaimedBy) != 1 {
		t.Fatalf("claims not recorded: %+v", got)
	}
	// Confirmed by the next read, the address is the node's.
	p = Decide(Input{Observed: []Address{addr("eip1", "i-a")}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")}, Previous: p.Status})
	st := &sipv1alpha1.SIPAddressPoolStatus{Addresses: p.Status}
	if b := BindingForNode(st, "a"); b == nil || b.Address != "203.0.113.1" {
		t.Errorf("BindingForNode = %+v", b)
	}

	// Once the pod has gone, the address is idle and remembers who used it.
	p = Decide(Input{Observed: []Address{addr("eip1", "i-a")}, Nodes: nodes("a"), Previous: p.Status})
	got = p.Status[0]
	if got.State != sipv1alpha1.PoolAddressIdle || len(got.ClaimedBy) != 0 || len(got.LastClaimedBy) != 1 {
		t.Fatalf("idle address: %+v", got)
	}
	if b := BindingForNode(&sipv1alpha1.SIPAddressPoolStatus{Addresses: p.Status}, "a"); b != nil {
		t.Errorf("idle address handed out: %+v", b)
	}
}

// A pod that moved to a node holding an address of its own is no longer the last claimant of the address it left:
// when it moves again it gets the address it holds now, not the older, free one first.
func TestDecideForgetsAMovedPodsOlderAddress(t *testing.T) {
	// edge-0 used eip1 on node a, then moved to node b, which kept its idle eip2; eip1 is free again.
	prev := Decide(Input{Observed: []Address{addr("eip1", ""), addr("eip2", "i-b")}, Nodes: nodes("a", "b"),
		Previous: []sipv1alpha1.PoolAddress{
			{ID: "eip1", Address: "203.0.113.1", State: sipv1alpha1.PoolAddressFree, LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}},
			{ID: "eip2", Address: "203.0.113.2", State: sipv1alpha1.PoolAddressIdle, Node: "b", NodeID: "i-b"},
		},
		Claimants: []Claimant{claimant("edge-0", "b")},
	})
	if got := prev.Status[0].LastClaimedBy; len(got) != 0 {
		t.Fatalf("eip1 still remembers %v", got)
	}
	if got := prev.Status[1].LastClaimedBy; len(got) != 1 || got[0].Pod != "edge-0" {
		t.Fatalf("eip2: %v", got)
	}
	// edge-0 moves on to node c: it takes eip2, which it held last, off idle node b.
	p := Decide(Input{Observed: []Address{addr("eip1", ""), addr("eip2", "i-b")}, Nodes: nodes("a", "b", "c"),
		Previous: prev.Status, Claimants: []Claimant{claimant("edge-0", "c")}})
	if got, want := summary(p), "unbind eip2, bind eip2 c | eip1:Free eip2:Bound@c"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// An address the provider can't report on is still its pod's: the pod's older address forgets it all the same.
func TestDecideForgetsAnOlderAddressWhileTheHeldOneErrs(t *testing.T) {
	p := Decide(Input{Observed: []Address{addr("eip1", ""), {ID: "eip2", Err: errors.New("throttled")}}, Nodes: nodes("a", "b"),
		Previous: []sipv1alpha1.PoolAddress{
			{ID: "eip1", Address: "203.0.113.1", State: sipv1alpha1.PoolAddressFree, LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}},
			{ID: "eip2", Address: "203.0.113.2", State: sipv1alpha1.PoolAddressBound, Node: "b", NodeID: "i-b",
				ClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}, LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}},
		},
		Claimants: []Claimant{claimant("edge-0", "b")},
	})
	if got := p.Status[0].LastClaimedBy; len(got) != 0 {
		t.Errorf("eip1 still remembers %v", got)
	}
	if got := p.Status[1].LastClaimedBy; len(got) != 1 || got[0].Pod != "edge-0" {
		t.Errorf("eip2, in Error: %v", got)
	}
}

// An address in Error keeps the claim it had before the provider failed. Once its pod has left that node, the claim
// is stale: it must not make the pod forget the address it used since, which it should get back on its next node.
func TestDecideStaleErrorClaimKeepsTheMovedPodsAddress(t *testing.T) {
	p := Decide(Input{Observed: []Address{{ID: "eip1", Err: errors.New("no such Elastic IP")}, addr("eip2", "")}, Nodes: nodes("a", "b", "c"),
		Previous: []sipv1alpha1.PoolAddress{
			{ID: "eip1", Address: "203.0.113.1", State: sipv1alpha1.PoolAddressError, Node: "a", NodeID: "i-a",
				ClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}, LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}},
			{ID: "eip2", Address: "203.0.113.2", State: sipv1alpha1.PoolAddressFree, LastClaimedBy: []sipv1alpha1.AddressClaim{claim("edge-0")}},
		},
		// edge-0 has left b and isn't scheduled anywhere yet.
	})
	if got := p.Status[1].LastClaimedBy; len(got) != 1 || got[0].Pod != "edge-0" {
		t.Fatalf("eip2 forgot edge-0: %v", got)
	}
	p = Decide(Input{Observed: []Address{{ID: "eip1", Err: errors.New("no such Elastic IP")}, addr("eip2", ""), addr("eip3", "")}, Nodes: nodes("a", "b", "c"),
		Previous:  p.Status,
		Claimants: []Claimant{claimant("edge-0", "c")},
	})
	if got := p.Status[1]; got.Node != "c" {
		t.Errorf("edge-0 on c got another address than its last one: %s", summary(p))
	}
}

// A refused node gets no new address and waits, with why; one it already holds stays.
func TestDecideRefused(t *testing.T) {
	in := Input{
		Observed:  []Address{addr("eip1", "i-a"), addr("eip2", "")},
		Nodes:     nodes("a", "b"),
		Claimants: []Claimant{claimant("edge-0", "a"), claimant("edge-1", "b")},
		Refused:   map[string]string{"a": "runs the VPC CNI", "b": "runs the VPC CNI"},
	}
	p := Decide(in)
	if got := summary(p); got != " | eip1:Bound@a eip2:Free" {
		t.Errorf("plan: %s", got)
	}
	if p.Waiting["b"] != "runs the VPC CNI" || p.Waiting["a"] != "" {
		t.Errorf("waiting: %v", p.Waiting)
	}
}

// A Conflict stays one: the next pass, with it as Previous, must not take it as the pool's own binding.
func TestDecideConflictStaysAlone(t *testing.T) {
	in := Input{Observed: []Address{addr("eip1", "i-someone-else")}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")}}
	for pass := 1; pass <= 3; pass++ {
		p := Decide(in)
		if got := summary(p); got != " | eip1:Conflict" {
			t.Fatalf("pass %d: %s", pass, got)
		}
		in.Previous = p.Status
	}
}

// A just-bound address isn't handed out until a read confirms it, with its bind address if it has one.
func TestDecideConfirmsBinding(t *testing.T) {
	in := Input{Observed: []Address{addr("eip1", "")}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")}}
	p := Decide(in)
	if b := BindingForNode(&sipv1alpha1.SIPAddressPoolStatus{Addresses: p.Status}, "a"); b != nil || p.Status[0].Message == "" {
		t.Fatalf("handed out before confirmed: %+v", p.Status[0])
	}
	in.Previous, in.Observed = p.Status, []Address{addr("eip1", "i-a")}
	p = Decide(in)
	if b := BindingForNode(&sipv1alpha1.SIPAddressPoolStatus{Addresses: p.Status}, "a"); b == nil || b.Message != "" {
		t.Fatalf("not handed out once confirmed: %+v", p.Status[0])
	}
}

// An address the provider has bound but can't use yet (MetalLB hasn't assigned it) stays where it is, but its node's
// pods don't get it, and the node waits.
func TestDecideNotReady(t *testing.T) {
	a := addr("eip1", "i-a")
	a.BindIP, a.NotReady = "203.0.113.1", "MetalLB hasn't assigned it"
	in := Input{
		Observed: []Address{a}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")},
		NodeAddresses: map[string][]string{"a": {"203.0.113.1"}},
	}
	p := Decide(in)
	st := p.Status[0]
	if len(p.Steps) != 0 || st.State != sipv1alpha1.PoolAddressBound || st.BindAddressReady || st.Message != a.NotReady {
		t.Fatalf("status %+v, steps %v", st, p.Steps)
	}
	if b := BindingForNode(&sipv1alpha1.SIPAddressPoolStatus{Addresses: p.Status}, "a"); b != nil || AwaitingAgent(&st) {
		t.Fatalf("handed out, or blamed on the agent: %+v", b)
	}
	if p.Waiting["a"] == "" {
		t.Error("node a should wait")
	}
	if got := NodeBindAddresses(&sipv1alpha1.SIPAddressPoolStatus{Addresses: p.Status}, "a"); len(got) != 1 {
		t.Errorf("the node should keep the address on its interface: %v", got)
	}
}

// A bind address is read from the provider, kept while the provider can't answer, and forgotten when the address
// is given to another node (its bind address there is the provider's to report).
func TestDecideBindAddress(t *testing.T) {
	bound := addr("eip1", "i-a")
	bound.BindIP = "10.0.9.1"
	reported := map[string][]string{"a": {"10.0.9.1"}}
	p := Decide(Input{Observed: []Address{bound}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")}, NodeAddresses: reported})
	if got := p.Status[0]; got.State != sipv1alpha1.PoolAddressBound || got.BindAddress != "10.0.9.1" {
		t.Fatalf("bound: %+v", got)
	}
	failed := Address{ID: "eip1", Err: errors.New("throttled")}
	p = Decide(Input{Observed: []Address{failed}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")}, Previous: p.Status, NodeAddresses: reported})
	if got := p.Status[0]; got.State != sipv1alpha1.PoolAddressError || got.BindAddress != "10.0.9.1" {
		t.Fatalf("error: %+v", got)
	}
	if b := BindingForNode(&sipv1alpha1.SIPAddressPoolStatus{Addresses: p.Status}, "a"); b == nil || b.BindAddress != "10.0.9.1" {
		t.Fatalf("BindingForNode = %+v", b)
	}

	// The pod moves to node b; node a keeps the address idle until b takes it.
	p = Decide(Input{Observed: []Address{bound}, Nodes: nodes("a", "b"), Claimants: []Claimant{claimant("edge-0", "b")}, Previous: p.Status})
	if got := p.Status[0]; got.Node != "b" || got.BindAddress != "" {
		t.Fatalf("moved: %+v", got)
	}
}

func TestNodeBindAddresses(t *testing.T) {
	st := &sipv1alpha1.SIPAddressPoolStatus{Addresses: []sipv1alpha1.PoolAddress{
		{ID: "bound", Node: "a", State: sipv1alpha1.PoolAddressBound, BindAddress: "10.0.9.1"},
		{ID: "idle", Node: "a", State: sipv1alpha1.PoolAddressIdle, BindAddress: "10.0.9.2"},
		{ID: "error", Node: "a", State: sipv1alpha1.PoolAddressError, BindAddress: "10.0.9.3"},
		{ID: "conflict", Node: "a", State: sipv1alpha1.PoolAddressConflict, BindAddress: "10.0.9.4"},
		{ID: "none", Node: "a", State: sipv1alpha1.PoolAddressBound},
		{ID: "other", Node: "b", State: sipv1alpha1.PoolAddressBound, BindAddress: "10.0.9.5"},
	}}
	if got, want := NodeBindAddresses(st, "a"), []string{"10.0.9.1", "10.0.9.2", "10.0.9.3"}; !slices.Equal(got, want) {
		t.Errorf("NodeBindAddresses = %v, want %v", got, want)
	}
}

func TestProviderRead(t *testing.T) {
	st := &sipv1alpha1.SIPAddressPoolStatus{}
	if ProviderRead(st) {
		t.Error("a pool that has never read its provider")
	}
	for status, want := range map[metav1.ConditionStatus]bool{metav1.ConditionTrue: true, metav1.ConditionFalse: false} {
		st.Conditions = []metav1.Condition{{Type: sipv1alpha1.PoolConditionProviderAvailable, Status: status}}
		if got := ProviderRead(st); got != want {
			t.Errorf("ProviderAvailable %s: ProviderRead = %v", status, got)
		}
	}
}

// A bind address is ready once the node's agent reports it, stays so while the node has no report, and is taken
// back when the report leaves it out or the address moves.
func TestDecideBindAddressReady(t *testing.T) {
	bound := addr("eip1", "i-a")
	bound.BindIP = "10.0.9.1"
	in := Input{Observed: []Address{bound}, Nodes: nodes("a", "b"), Claimants: []Claimant{claimant("edge-0", "a")}}
	st := Decide(in).Status[0]
	if st.BindAddressReady {
		t.Fatal("ready with no report")
	}
	if b := BindingForNode(&sipv1alpha1.SIPAddressPoolStatus{Addresses: []sipv1alpha1.PoolAddress{st}}, "a"); b != nil || !AwaitingAgent(&st) {
		t.Fatalf("handed out before the agent reported: %+v", b)
	}

	in.NodeAddresses = map[string][]string{"a": {"10.0.9.1"}}
	st = Decide(in).Status[0]
	if !st.BindAddressReady || AwaitingAgent(&st) {
		t.Fatalf("not ready once reported: %+v", st)
	}

	in.Previous, in.NodeAddresses = []sipv1alpha1.PoolAddress{st}, nil
	if st = Decide(in).Status[0]; !st.BindAddressReady {
		t.Fatal("no report should keep it ready")
	}

	in.Previous, in.NodeAddresses = []sipv1alpha1.PoolAddress{st}, map[string][]string{"a": {}}
	if st = Decide(in).Status[0]; st.BindAddressReady {
		t.Fatal("a report without it should take it back")
	}

	moved := addr("eip1", "i-b")
	moved.BindIP = "10.0.9.1"
	in.Previous = []sipv1alpha1.PoolAddress{{ID: "eip1", Node: "a", BindAddress: "10.0.9.1", BindAddressReady: true, State: sipv1alpha1.PoolAddressBound}}
	in.Observed, in.NodeAddresses = []Address{moved}, nil
	if st = Decide(in).Status[0]; st.BindAddressReady {
		t.Fatal("readiness on another node carried over")
	}
}

func TestDecideFenced(t *testing.T) {
	// edge-1 failed over: its node b is fenced, and the StatefulSet has started edge-1 again on the spare, which holds
	// an idle address. A terminating edge-1 still on b (force deletion pending) claims nothing there.
	base := Input{
		Observed:  []Address{addr("eip1", "i-a"), addr("eip2", "i-b"), addr("eip3", "i-spare")},
		Nodes:     nodes("a", "b", "spare"),
		Claimants: []Claimant{claimant("edge-0", "a"), claimant("edge-1", "b"), claimant("edge-1", "spare")},
		Fenced:    map[string]bool{"b": true},
	}
	t.Run("may fence: the fenced node's address is taken off", func(t *testing.T) {
		in := base
		in.MayFence = true
		p := Decide(in)
		if got := summary(p); got != "unbind eip2 | eip1:Bound@a eip2:Free eip3:Bound@spare" {
			t.Errorf("plan: %s", got)
		}
		if len(p.Waiting) != 0 {
			t.Errorf("waiting: %v", p.Waiting)
		}
	})
	t.Run("advisory without the opt-in: held on the fenced node, never moved", func(t *testing.T) {
		in := base
		// A second needy node with no free address must not take the held one.
		in.Nodes = nodes("a", "b", "spare", "c")
		in.Claimants = append(in.Claimants, claimant("edge-2", "c"))
		p := Decide(in)
		if got := summary(p); got != " | eip1:Bound@a eip2:Fenced@b eip3:Bound@spare" {
			t.Errorf("plan: %s", got)
		}
		if !strings.Contains(p.Status[1].Message, "fenced") {
			t.Errorf("held address message: %q", p.Status[1].Message)
		}
		if p.Waiting["c"] == "" {
			t.Errorf("c should wait: %v", p.Waiting)
		}
	})
	t.Run("a fenced node gets no address", func(t *testing.T) {
		in := Input{
			Observed: []Address{addr("eip1", "")}, Nodes: nodes("b"), MayFence: true,
			Claimants: []Claimant{claimant("edge-1", "b")}, Fenced: map[string]bool{"b": true},
		}
		if got := summary(Decide(in)); got != " | eip1:Free" {
			t.Errorf("plan: %s", got)
		}
	})
}
