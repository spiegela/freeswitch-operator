package addresspool

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
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

// A bind address is read from the provider, kept while the provider can't answer, and forgotten when the address
// is given to another node (its bind address there is the provider's to report).
func TestDecideBindAddress(t *testing.T) {
	bound := addr("eip1", "i-a")
	bound.BindIP = "10.0.9.1"
	p := Decide(Input{Observed: []Address{bound}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")}})
	if got := p.Status[0]; got.State != sipv1alpha1.PoolAddressBound || got.BindAddress != "10.0.9.1" {
		t.Fatalf("bound: %+v", got)
	}
	failed := Address{ID: "eip1", Err: errors.New("throttled")}
	p = Decide(Input{Observed: []Address{failed}, Nodes: nodes("a"), Claimants: []Claimant{claimant("edge-0", "a")}, Previous: p.Status})
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

func TestNeedsNodeAddress(t *testing.T) {
	spec := &sipv1alpha1.SIPAddressPoolSpec{AWSElasticIP: &sipv1alpha1.AWSElasticIPPool{}}
	if NeedsNodeAddress(spec) {
		t.Error("Primary needs a node address")
	}
	spec.AWSElasticIP.PrivateAddress = sipv1alpha1.AWSPrivateAddressSecondary
	if !NeedsNodeAddress(spec) {
		t.Error("Secondary doesn't")
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
	for reason, want := range map[string]bool{
		"":                                  false,
		"AddressesBound":                    true,
		sipv1alpha1.PoolReasonProviderError: false,
		sipv1alpha1.PoolReasonProviderUnavailable: false,
	} {
		st := &sipv1alpha1.SIPAddressPoolStatus{}
		if reason != "" {
			st.Conditions = []metav1.Condition{{Type: sipv1alpha1.PoolConditionReady, Reason: reason}}
		}
		if got := ProviderRead(st); got != want {
			t.Errorf("reason %q: ProviderRead = %v, want %v", reason, got, want)
		}
	}
}
