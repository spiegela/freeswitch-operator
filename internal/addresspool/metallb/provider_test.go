package metallb

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
	"github.com/spiegela/freeswitch-operator/internal/addresspool"
)

const ns = "freeswitch-operator-system"

func setup(t *testing.T, objs ...client.Object) (*Provider, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = sipv1alpha1.AddToScheme(scheme)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-a"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "fc00::5"}, {Type: corev1.NodeInternalIP, Address: "172.18.0.5"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, node)...).Build()
	pool := &sipv1alpha1.SIPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-media"},
		Spec: sipv1alpha1.SIPAddressPoolSpec{MetalLB: &sipv1alpha1.MetalLBPool{
			Addresses: []string{"172.18.255.200", "172.18.255.201", "172.18.255.202"}, IPAddressPool: "sip",
		}},
	}
	p, err := NewFactory(c, ns)(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Provider), c
}

// assign plays MetalLB: it gives a Service the address it asks for.
func assign(t *testing.T, c client.Client, name, ip string) {
	t.Helper()
	var s corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		t.Fatal(err)
	}
	s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
	if err := c.Status().Update(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
}

func TestBindObserveUnbind(t *testing.T) {
	ctx := context.Background()
	p, c := setup(t)
	if caps := p.Capabilities(); caps.Datapath != sipv1alpha1.AddressDatapathLocal || caps.Fencing != sipv1alpha1.AddressFencingAdvisory {
		t.Errorf("capabilities %+v", caps)
	}
	obs, err := p.Observe(ctx)
	if err != nil || len(obs) != 3 || obs[0].NodeID != "" || obs[0].ID != "172.18.255.200" {
		t.Fatalf("Observe = %+v, %v", obs, err)
	}

	if err := p.Bind(ctx, obs[0], "edge-a"); err != nil {
		t.Fatal(err)
	}
	name := "sip-edge-media-172-18-255-200"
	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Annotations[LoadBalancerIPsAnnotation] != "172.18.255.200" || svc.Annotations[AddressPoolAnnotation] != "sip" ||
		svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal || svc.Spec.Selector != nil ||
		len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 9 {
		t.Errorf("Service %+v", svc)
	}
	var slice discoveryv1.EndpointSlice
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &slice); err != nil {
		t.Fatal(err)
	}
	validLabels(t, &svc, &slice)
	if ep := slice.Endpoints[0]; ep.Addresses[0] != "172.18.0.5" || *ep.NodeName != "edge-a" || !*ep.Conditions.Ready ||
		slice.Labels[discoveryv1.LabelServiceName] != name {
		t.Errorf("EndpointSlice %+v", slice)
	}

	By := func(want string, got addresspool.Address) {
		t.Helper()
		if got.NodeID != "edge-a" || got.BindIP != "172.18.255.200" || got.BindingID != name {
			t.Errorf("%s: %+v", want, got)
		}
	}
	obs, _ = p.Observe(ctx)
	By("bound, not yet assigned", obs[0])
	if obs[0].Err != nil || !strings.Contains(obs[0].NotReady, "hasn't assigned") {
		t.Errorf("unassigned: %+v", obs[0])
	}
	assign(t, c, name, "172.18.255.200")
	obs, _ = p.Observe(ctx)
	By("assigned", obs[0])
	if obs[0].Err != nil || obs[0].NotReady != "" {
		t.Errorf("assigned: %+v", obs[0])
	}

	if err := p.Unbind(ctx, obs[0]); err != nil {
		t.Fatal(err)
	}
	if obs, _ = p.Observe(ctx); obs[0].NodeID != "" {
		t.Errorf("after unbind: %+v", obs[0])
	}
	if err := p.Unbind(ctx, addresspool.Address{BindingID: name}); err != nil {
		t.Errorf("unbinding twice: %v", err)
	}
}

// A move binds the address to another node while the pool's own Service for it may still be there (Unbind's delete
// not yet in the cache, or a Service left without its EndpointSlice): Bind completes it rather than refusing.
func TestBindOverOwnService(t *testing.T) {
	ctx := context.Background()
	p, c := setup(t)
	obs, _ := p.Observe(ctx)
	if err := p.Bind(ctx, obs[0], "edge-a"); err != nil {
		t.Fatal(err)
	}
	name := "sip-edge-media-172-18-255-200"
	if err := c.Delete(ctx, &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}); err != nil {
		t.Fatal(err)
	}
	obs, _ = p.Observe(ctx)
	if obs[0].NodeID != "" {
		t.Fatalf("a Service without its EndpointSlice announces nothing, so the address is unbound: %+v", obs[0])
	}
	if err := p.Bind(ctx, obs[0], "edge-a"); err != nil {
		t.Fatalf("Bind over the pool's own Service: %v", err)
	}
	if obs, _ = p.Observe(ctx); obs[0].NodeID != "edge-a" {
		t.Errorf("not bound again: %+v", obs[0])
	}
	// Again, with the EndpointSlice still there (the cache hasn't seen Unbind's deletes).
	if err := p.Bind(ctx, obs[0], "edge-a"); err != nil {
		t.Errorf("Bind over a complete binding of the pool's own: %v", err)
	}
}

// A pool name too long for a label value is hashed in the labels, and the pool still finds its own Services.
func TestLongPoolName(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-a"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.5"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	pool := &sipv1alpha1.SIPAddressPool{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("edge-media.", 7)},
		Spec:       sipv1alpha1.SIPAddressPoolSpec{MetalLB: &sipv1alpha1.MetalLBPool{Addresses: []string{"172.18.255.200"}}},
	}
	pr, err := NewFactory(c, ns)(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	obs, _ := pr.Observe(ctx)
	if err := pr.Bind(ctx, obs[0], "edge-a"); err != nil {
		t.Fatal(err)
	}
	var svcs corev1.ServiceList
	if err := c.List(ctx, &svcs); err != nil || len(svcs.Items) != 1 {
		t.Fatalf("Services: %v, %v", svcs.Items, err)
	}
	validLabels(t, &svcs.Items[0])
	if obs, _ = pr.Observe(ctx); obs[0].NodeID != "edge-a" {
		t.Errorf("the pool doesn't find its own binding: %+v", obs[0])
	}
}

// Prune takes back the bindings of addresses taken out of the spec, and only those.
func TestPrune(t *testing.T) {
	ctx := context.Background()
	p, c := setup(t)
	obs, _ := p.Observe(ctx)
	for _, a := range obs[:2] {
		if err := p.Bind(ctx, a, "edge-a"); err != nil {
			t.Fatal(err)
		}
	}
	p.addresses = p.addresses[1:] // 172.18.255.200 taken out of the spec
	if err := p.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var svcs corev1.ServiceList
	var eps discoveryv1.EndpointSliceList
	if err := c.List(ctx, &svcs); err != nil {
		t.Fatal(err)
	}
	if err := c.List(ctx, &eps); err != nil {
		t.Fatal(err)
	}
	if len(svcs.Items) != 1 || svcs.Items[0].Name != "sip-edge-media-172-18-255-201" || len(eps.Items) != 1 {
		t.Errorf("after pruning: Services %d, EndpointSlices %d", len(svcs.Items), len(eps.Items))
	}
}

// validLabels checks objs' labels as the API server would: the fake client doesn't.
func validLabels(t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		for k, v := range o.GetLabels() {
			if errs := append(validation.IsQualifiedName(k), validation.IsValidLabelValue(v)...); len(errs) > 0 {
				t.Errorf("%T label %s=%s: %v", o, k, v, errs)
			}
		}
	}
}

// An address another Service asks for or holds is a conflict, and Bind refuses it.
func TestOtherServicesConflict(t *testing.T) {
	ctx := context.Background()
	asks := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "apps", Annotations: map[string]string{LoadBalancerIPsAnnotation: "172.18.255.201"}},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
	holds := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "dns", Namespace: "apps"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "172.18.255.202"}}}},
	}
	p, _ := setup(t, asks, holds)
	obs, err := p.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs[1].NodeID != "Service apps/web" || obs[2].NodeID != "Service apps/dns" || obs[0].NodeID != "" {
		t.Errorf("Observe = %+v", obs)
	}
	if err := p.Bind(ctx, obs[1], "edge-a"); err == nil || !strings.Contains(err.Error(), "apps/web") {
		t.Errorf("Bind over another Service: %v", err)
	}
	// A conflict has no binding of the pool's: Unbind touches nothing.
	if err := p.Unbind(ctx, obs[1]); err != nil {
		t.Errorf("unbinding what isn't the pool's: %v", err)
	}
}

func TestServiceName(t *testing.T) {
	if got := serviceName("edge.media", "10.0.0.1"); got != "sip-edge-media-10-0-0-1" {
		t.Errorf("short: %s", got)
	}
	long := serviceName(strings.Repeat("p", 60), "10.0.0.1")
	if len(long) > 63 || !strings.HasSuffix(long, "-10-0-0-1") || !strings.HasPrefix(long, "sip-") {
		t.Errorf("long: %s", long)
	}
}

func TestFactoryNeedsNamespace(t *testing.T) {
	pool := &sipv1alpha1.SIPAddressPool{Spec: sipv1alpha1.SIPAddressPoolSpec{MetalLB: &sipv1alpha1.MetalLBPool{Addresses: []string{"10.0.0.1"}}}}
	if _, err := NewFactory(nil, "")(context.Background(), pool); err == nil {
		t.Error("no namespace accepted")
	}
}

// The pool's own Service on a node that has gone announces nothing: the address is unbound, not a conflict the pool
// can't clear, and the next Bind moves the Service to the new node.
func TestOwnServiceOnAGoneNode(t *testing.T) {
	ctx := context.Background()
	p, c := setup(t)
	obs, _ := p.Observe(ctx)
	if err := p.Bind(ctx, obs[0], "edge-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "edge-a"}}); err != nil {
		t.Fatal(err)
	}
	if obs, _ = p.Observe(ctx); obs[0].NodeID != "" {
		t.Fatalf("bound to a gone node: %+v", obs[0])
	}
	b := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-b"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.6"}}},
	}
	if err := c.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := p.Bind(ctx, obs[0], "edge-b"); err != nil {
		t.Fatalf("Bind over the gone node's Service: %v", err)
	}
	if obs, _ = p.Observe(ctx); obs[0].NodeID != "edge-b" {
		t.Errorf("not moved: %+v", obs[0])
	}
}
