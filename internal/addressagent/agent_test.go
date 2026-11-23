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

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

type fakeLinks struct {
	addrs []Addr
	calls []string
}

func (f *fakeLinks) Interface(netip.Addr) (Link, []Addr, error) {
	return Link{Name: "ens5", Index: 2}, slices.Clone(f.addrs), nil
}

func (f *fakeLinks) Add(_ Link, ip netip.Addr, label string) error {
	f.calls = append(f.calls, "add "+ip.String()+" "+label)
	f.addrs = append(f.addrs, Addr{IP: ip, Label: label})
	return nil
}

func (f *fakeLinks) Del(_ Link, ip netip.Addr) error {
	f.calls = append(f.calls, "del "+ip.String())
	f.addrs = slices.DeleteFunc(f.addrs, func(a Addr) bool { return a.IP == ip })
	return nil
}

func secondaryPool(available bool, addrs ...sipv1alpha1.PoolAddress) *sipv1alpha1.SIPAddressPool {
	read := metav1.ConditionTrue
	if !available {
		read = metav1.ConditionFalse
	}
	return &sipv1alpha1.SIPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "edge"},
		Spec: sipv1alpha1.SIPAddressPoolSpec{AWSElasticIP: &sipv1alpha1.AWSElasticIPPool{
			Region: "us-east-2", AllocationIDs: []string{"eipalloc-1"}, PrivateAddress: sipv1alpha1.AWSPrivateAddressSecondary,
		}},
		Status: sipv1alpha1.SIPAddressPoolStatus{
			Addresses:  addrs,
			Conditions: []metav1.Condition{{Type: sipv1alpha1.PoolConditionProviderAvailable, Status: read}},
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
	pool := secondaryPool(true, bound, elsewhere)
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

// While a pool can't read its provider, the agent keeps every address that pool names (its status may be stale),
// but still removes one no pool names: another pool's address that moved away.
func TestSyncKeepsAddressesWhileAPoolIsUnsettled(t *testing.T) {
	ctx := context.Background()
	stale := sipv1alpha1.PoolAddress{ID: "eipalloc-1", State: sipv1alpha1.PoolAddressError, Node: "node-b", BindAddress: "10.0.9.7"}
	a, links, _ := setup(t, secondaryPool(false, stale))
	links.addrs = append(links.addrs,
		Addr{IP: netip.MustParseAddr("10.0.9.7"), Label: "ens5:sip"}, Addr{IP: netip.MustParseAddr("10.0.9.9"), Label: "ens5:sip"})
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(links.calls, []string{"del 10.0.9.9"}) {
		t.Errorf("calls = %v", links.calls)
	}
}

// An address already on the node (its own, for a pool switched from Primary) is reported, not added.
func TestSyncReportsAnAddressAlreadyThere(t *testing.T) {
	ctx := context.Background()
	own := sipv1alpha1.PoolAddress{ID: "eipalloc-1", State: sipv1alpha1.PoolAddressBound, Node: "node-a", BindAddress: "10.0.1.10"}
	a, links, c := setup(t, secondaryPool(true, own))
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(links.calls) != 0 || reported(t, c) != "10.0.1.10" {
		t.Errorf("calls = %v, reported %q", links.calls, reported(t, c))
	}
}

// Pools that bind the node's own address need nothing from the agent, and one that can't read its provider doesn't
// hold back removals for the others.
func TestSyncIgnoresPoolsWithoutBindAddresses(t *testing.T) {
	ctx := context.Background()
	p := secondaryPool(false, sipv1alpha1.PoolAddress{ID: "eipalloc-1", State: sipv1alpha1.PoolAddressBound, Node: "node-a"})
	p.Spec.AWSElasticIP.PrivateAddress = sipv1alpha1.AWSPrivateAddressPrimary
	a, links, _ := setup(t, p)
	links.addrs = append(links.addrs, Addr{IP: netip.MustParseAddr("10.0.9.7"), Label: "ens5:sip"})
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(links.calls, []string{"del 10.0.9.7"}) {
		t.Errorf("calls = %v", links.calls)
	}
}

func TestHostIPv4(t *testing.T) {
	for _, tc := range []struct{ ips, ip, want string }{
		{"fc00::5,172.18.0.5", "fc00::5", "172.18.0.5"},
		{"", "10.0.1.10", "10.0.1.10"},
		{"10.0.1.10", "", "10.0.1.10"},
		{"fc00::5", "fc00::5", ""},
	} {
		got, err := HostIPv4(tc.ips, tc.ip)
		if (tc.want == "" && err == nil) || (tc.want != "" && (err != nil || got.String() != tc.want)) {
			t.Errorf("HostIPv4(%q, %q) = %v, %v; want %s", tc.ips, tc.ip, got, err, tc.want)
		}
	}
}
