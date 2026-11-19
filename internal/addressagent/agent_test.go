package addressagent

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

type fakeLinks struct {
	addrs []Addr
	calls []string
}

func (f *fakeLinks) Interface(netip.Addr) (string, []Addr, error) {
	return "ens5", slices.Clone(f.addrs), nil
}

func (f *fakeLinks) Add(iface string, ip netip.Addr, label string) error {
	f.calls = append(f.calls, "add "+ip.String()+" "+label)
	f.addrs = append(f.addrs, Addr{IP: ip, Label: label})
	return nil
}

func (f *fakeLinks) Del(iface string, ip netip.Addr) error {
	f.calls = append(f.calls, "del "+ip.String())
	f.addrs = slices.DeleteFunc(f.addrs, func(a Addr) bool { return a.IP == ip })
	return nil
}

func secondaryPool(reason string, addrs ...sipv1alpha1.PoolAddress) *sipv1alpha1.SIPAddressPool {
	return &sipv1alpha1.SIPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "edge"},
		Spec: sipv1alpha1.SIPAddressPoolSpec{AWSElasticIP: &sipv1alpha1.AWSElasticIPPool{
			Region: "us-east-2", AllocationIDs: []string{"eipalloc-1"}, PrivateAddress: sipv1alpha1.AWSPrivateAddressSecondary,
		}},
		Status: sipv1alpha1.SIPAddressPoolStatus{
			Addresses:  addrs,
			Conditions: []metav1.Condition{{Type: sipv1alpha1.PoolConditionReady, Status: metav1.ConditionTrue, Reason: reason}},
		},
	}
}

func setup(t *testing.T, objs ...client.Object) (*Agent, *fakeLinks, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = sipv1alpha1.AddToScheme(scheme)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-x", Namespace: "ops"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, pod)...).WithStatusSubresource(&sipv1alpha1.SIPAddressPool{}).Build()
	links := &fakeLinks{addrs: []Addr{{IP: netip.MustParseAddr("10.0.1.10"), Label: "ens5"}}}
	return &Agent{Client: c, Links: links, Node: "node-a", HostIP: netip.MustParseAddr("10.0.1.10"), Pod: types.NamespacedName{Namespace: "ops", Name: "agent-x"}}, links, c
}

func reported(t *testing.T, c client.Client) string {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ops", Name: "agent-x"}, &pod); err != nil {
		t.Fatal(err)
	}
	return pod.Annotations[sipv1alpha1.AgentBindAddressesAnnotation]
}

func TestSyncAddsReportsAndRemoves(t *testing.T) {
	ctx := context.Background()
	bound := sipv1alpha1.PoolAddress{ID: "eipalloc-1", State: sipv1alpha1.PoolAddressBound, Node: "node-a", BindAddress: "10.0.9.7"}
	elsewhere := sipv1alpha1.PoolAddress{ID: "eipalloc-2", State: sipv1alpha1.PoolAddressBound, Node: "node-b", BindAddress: "10.0.9.8"}
	pool := secondaryPool("AddressesBound", bound, elsewhere)
	a, links, c := setup(t, pool)

	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(links.calls, []string{"add 10.0.9.7 ens5:sip"}) {
		t.Errorf("calls = %v", links.calls)
	}
	if got := reported(t, c); got != "10.0.9.7" {
		t.Errorf("reported %q", got)
	}

	// The address moves off this node: the agent takes back what it added, never the node's own address.
	pool.Status.Addresses[0].Node = "node-c"
	if err := c.Status().Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	links.calls = nil
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(links.calls, []string{"del 10.0.9.7"}) {
		t.Errorf("calls = %v", links.calls)
	}
	if got := reported(t, c); got != "" {
		t.Errorf("reported %q", got)
	}
}

// While a pool can't read its provider, the agent adds but never removes.
func TestSyncKeepsAddressesWhileAPoolIsUnsettled(t *testing.T) {
	ctx := context.Background()
	a, links, _ := setup(t, secondaryPool(sipv1alpha1.PoolReasonProviderError))
	links.addrs = append(links.addrs, Addr{IP: netip.MustParseAddr("10.0.9.7"), Label: "ens5:sip"})
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(links.calls) != 0 {
		t.Errorf("calls = %v", links.calls)
	}
}

// An address already on the node (its own, for a pool switched from Primary) is reported, not added.
func TestSyncReportsAnAddressAlreadyThere(t *testing.T) {
	ctx := context.Background()
	own := sipv1alpha1.PoolAddress{ID: "eipalloc-1", State: sipv1alpha1.PoolAddressBound, Node: "node-a", BindAddress: "10.0.1.10"}
	a, links, c := setup(t, secondaryPool("AddressesBound", own))
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(links.calls) != 0 || reported(t, c) != "10.0.1.10" {
		t.Errorf("calls = %v, reported %q", links.calls, reported(t, c))
	}
}

// Pools that bind the node's own address need nothing from the agent.
func TestSyncIgnoresPrimaryPools(t *testing.T) {
	ctx := context.Background()
	p := secondaryPool("AddressesBound", sipv1alpha1.PoolAddress{ID: "eipalloc-1", State: sipv1alpha1.PoolAddressBound, Node: "node-a", BindAddress: "10.0.9.7"})
	p.Spec.AWSElasticIP.PrivateAddress = sipv1alpha1.AWSPrivateAddressPrimary
	a, links, _ := setup(t, p)
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(links.calls) != 0 {
		t.Errorf("calls = %v", links.calls)
	}
}
