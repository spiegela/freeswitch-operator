// Package metallb is the SIPAddressPool provider for addresses MetalLB announces (L2 or BGP). MetalLB announces
// only addresses that belong to a LoadBalancer Service, so a binding is a Service of the pool's own, in the
// operator's namespace, that asks MetalLB for the address (metallb.io/loadBalancerIPs) with
// externalTrafficPolicy Local and a selector-less EndpointSlice naming the node: MetalLB then announces the address
// from that node, and gives it to no other Service.
//
// The Service's one port is a placeholder (discard, 9/UDP): kube-proxy rewrites traffic to the Service's ports, and
// SIP and RTP must reach the fleet's pods untouched. The address agent puts the address itself on the node's
// interface, so the node delivers every port locally and the pods bind it. So the datapath is Local; a node cut off
// from the cluster keeps the address on its interface, so fencing is advisory.
package metallb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
	"github.com/spiegela/freeswitch-operator/internal/addresspool"
)

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;delete

const (
	// LoadBalancerIPsAnnotation asks MetalLB for an address.
	LoadBalancerIPsAnnotation = "metallb.io/loadBalancerIPs"
	// AddressPoolAnnotation names the MetalLB IPAddressPool to take it from.
	AddressPoolAnnotation = "metallb.io/address-pool"

	// PoolLabel, AddressLabel and NodeLabel mark a binding's Service and EndpointSlice: the SIPAddressPool (its name,
	// or a hash of a name too long for a label), the address, and the node it is bound to.
	PoolLabel    = "sip.instruments.com/address-pool"
	AddressLabel = "sip.instruments.com/address"
	NodeLabel    = "sip.instruments.com/node"

	managedBy = "address-pool.sip.instruments.com" // a label value: no slash
	portName  = "announce"
	port      = 9 // discard: nothing listens, and the Service carries no traffic of its own
)

// Provider binds one pool's addresses.
type Provider struct {
	c         client.Client
	namespace string
	pool      string
	key       string // the pool's PoolLabel value
	addresses []string
	ipPool    string
}

// NewFactory returns the factory for MetalLB pools, which keeps its Services in namespace (the operator's own).
func NewFactory(c client.Client, namespace string) addresspool.Factory {
	return func(_ context.Context, pool *sipv1alpha1.SIPAddressPool) (addresspool.Provider, error) {
		spec := pool.Spec.MetalLB
		if spec == nil {
			return nil, errors.New("not a MetalLB pool")
		}
		if namespace == "" {
			return nil, errors.New("the operator doesn't know its namespace (POD_NAMESPACE) to keep the pool's Services in")
		}
		return &Provider{
			c: c, namespace: namespace, pool: pool.Name, key: poolKey(pool.Name),
			addresses: spec.Addresses, ipPool: spec.IPAddressPool,
		}, nil
	}
}

// Capabilities: the address arrives at the node itself; a node cut off from the cluster keeps it on its interface.
func (p *Provider) Capabilities() addresspool.Capabilities {
	return addresspool.Capabilities{Datapath: sipv1alpha1.AddressDatapathLocal, Fencing: sipv1alpha1.AddressFencingAdvisory}
}

// NodeID is the node's name: a binding names its node in an EndpointSlice.
func (p *Provider) NodeID(n *corev1.Node) (string, error) {
	return n.Name, nil
}

// Observe reads every LoadBalancer Service and the pool's EndpointSlices. An address is bound where the pool's
// Service for it and its EndpointSlice say, and ready once MetalLB has assigned it; one another Service requests or
// holds is a conflict. A Service whose EndpointSlice is missing announces nothing, so its address counts as unbound,
// and the next Bind completes it.
func (p *Provider) Observe(ctx context.Context) ([]addresspool.Address, error) {
	svcs, err := p.services(ctx)
	if err != nil {
		return nil, err
	}
	var eps discoveryv1.EndpointSliceList
	if err := p.c.List(ctx, &eps, client.InNamespace(p.namespace), client.MatchingLabels{PoolLabel: p.key}); err != nil {
		return nil, err
	}
	sliced := map[string]bool{}
	for _, s := range eps.Items {
		sliced[s.Name] = true
	}
	ours := p.ours(svcs)
	others := holders(svcs, p.owns)

	out := make([]addresspool.Address, 0, len(p.addresses))
	for _, ip := range p.addresses {
		a := addresspool.Address{ID: ip, IP: ip}
		switch s, bound := ours[ip]; {
		case bound && sliced[s.Name]:
			a.NodeID, a.BindingID, a.BindIP = s.Labels[NodeLabel], s.Name, ip
			a.NotReady = notAssigned(s, ip, others[ip])
		case len(others[ip]) > 0:
			a.NodeID = "Service " + strings.Join(others[ip], ", ")
		}
		out = append(out, a)
	}
	return out, nil
}

// notAssigned says why MetalLB hasn't assigned ip to the pool's Service s, or "" when it has.
func notAssigned(s *corev1.Service, ip string, others []string) string {
	if slices.Contains(ingressOf(s), ip) {
		return ""
	}
	if len(others) > 0 {
		return fmt.Sprintf("MetalLB hasn't assigned it to Service %s/%s: %s request it too", s.Namespace, s.Name, strings.Join(others, ", "))
	}
	return fmt.Sprintf("MetalLB hasn't assigned it to Service %s/%s: is it in an IPAddressPool?", s.Namespace, s.Name)
}

// Bind makes, or completes, the address's Service and the EndpointSlice that names the node. It refuses an address
// another Service requests or holds.
func (p *Provider) Bind(ctx context.Context, a addresspool.Address, nodeID string) error {
	var node corev1.Node
	if err := p.c.Get(ctx, types.NamespacedName{Name: nodeID}, &node); err != nil {
		return err
	}
	nodeIP := internalIPv4(&node)
	if nodeIP == "" {
		return fmt.Errorf("node %s has no IPv4 InternalIP", nodeID)
	}
	svcs, err := p.services(ctx)
	if err != nil {
		return err
	}
	// The pool's own Services don't count: one for this address is the one this binding replaces or completes
	// (and one Unbind has just deleted may still be in the cache).
	if o := holders(svcs, p.owns)[a.IP]; len(o) > 0 {
		return fmt.Errorf("%s is already requested or held by Service %s", a.IP, strings.Join(o, ", "))
	}

	name := serviceName(p.pool, a.IP)
	labels := map[string]string{PoolLabel: p.key, AddressLabel: a.IP, NodeLabel: nodeID}
	svc, created, err := p.ensureService(ctx, name, labels, a.IP)
	if err != nil {
		return err
	}
	if err := p.replaceSlice(ctx, svc, labels, nodeID, nodeIP); err != nil {
		if created {
			if derr := p.c.Delete(ctx, svc); derr != nil && !apierrors.IsNotFound(derr) {
				err = fmt.Errorf("%w; deleting the Service again also failed: %w", err, derr)
			}
		}
		return err
	}
	return nil
}

// ensureService creates the binding's Service, or, when one is left from an earlier binding of the address,
// relabels it for this one. It says whether it created it.
func (p *Provider) ensureService(ctx context.Context, name string, labels map[string]string, ip string) (*corev1.Service, bool, error) {
	annotations := map[string]string{LoadBalancerIPsAnnotation: ip}
	if p.ipPool != "" {
		annotations[AddressPoolAnnotation] = p.ipPool
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.namespace, Labels: labels, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy:         corev1.ServiceExternalTrafficPolicyLocal,
			AllocateLoadBalancerNodePorts: ptr.To(false),
			Ports:                         []corev1.ServicePort{{Name: portName, Protocol: corev1.ProtocolUDP, Port: port}},
		},
	}
	err := p.c.Create(ctx, svc)
	if err == nil {
		return svc, true, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, false, fmt.Errorf("creating Service %s/%s: %w", p.namespace, name, err)
	}
	var cur corev1.Service
	if err := p.c.Get(ctx, types.NamespacedName{Namespace: p.namespace, Name: name}, &cur); err != nil {
		return nil, false, err
	}
	if !p.owns(&cur) || cur.Labels[AddressLabel] != ip {
		return nil, false, fmt.Errorf("the Service %s/%s exists, and is not this pool's for %s", p.namespace, name, ip)
	}
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Labels[NodeLabel] = labels[NodeLabel]
	if err := p.c.Patch(ctx, &cur, patch); err != nil {
		return nil, false, fmt.Errorf("relabelling Service %s/%s: %w", p.namespace, name, err)
	}
	return &cur, false, nil
}

// replaceSlice (re)makes the EndpointSlice that names the binding's node.
func (p *Provider) replaceSlice(ctx context.Context, svc *corev1.Service, labels map[string]string, nodeID, nodeIP string) error {
	sliceLabels := map[string]string{discoveryv1.LabelServiceName: svc.Name, discoveryv1.LabelManagedBy: managedBy}
	maps.Copy(sliceLabels, labels)
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: svc.Name, Namespace: p.namespace, Labels: sliceLabels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: svc.Name, UID: svc.UID}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{nodeIP},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			NodeName:   ptr.To(nodeID),
		}},
		Ports: []discoveryv1.EndpointPort{{Name: ptr.To(portName), Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(int32(port))}},
	}
	if err := p.c.Delete(ctx, slice); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the old EndpointSlice %s/%s: %w", p.namespace, svc.Name, err)
	}
	if err := p.c.Create(ctx, slice); err != nil {
		return fmt.Errorf("creating EndpointSlice %s/%s: %w", p.namespace, svc.Name, err)
	}
	return nil
}

// Unbind deletes the address's Service and EndpointSlice. MetalLB stops announcing it at once, and the address
// agent takes it off the node.
func (p *Provider) Unbind(ctx context.Context, a addresspool.Address) error {
	if a.BindingID == "" {
		return nil // nothing of the pool's
	}
	return p.delete(ctx, a.BindingID)
}

// Prune deletes the pool's Services, and their EndpointSlices, for addresses no longer in its spec, so MetalLB stops
// announcing them and another pool may take them.
func (p *Provider) Prune(ctx context.Context) error {
	svcs, err := p.services(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for ip, s := range p.ours(svcs) {
		if !slices.Contains(p.addresses, ip) {
			errs = append(errs, p.delete(ctx, s.Name))
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) delete(ctx context.Context, name string) error {
	meta := metav1.ObjectMeta{Name: name, Namespace: p.namespace}
	if err := p.c.Delete(ctx, &discoveryv1.EndpointSlice{ObjectMeta: meta}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := p.c.Delete(ctx, &corev1.Service{ObjectMeta: meta}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (p *Provider) services(ctx context.Context) ([]corev1.Service, error) {
	var svcs corev1.ServiceList
	if err := p.c.List(ctx, &svcs); err != nil {
		return nil, err
	}
	return svcs.Items, nil
}

func (p *Provider) owns(s *corev1.Service) bool {
	return s.Namespace == p.namespace && s.Labels[PoolLabel] == p.key && s.Labels[AddressLabel] != ""
}

// ours is the pool's LoadBalancer Services by address (the first, should there be two).
func (p *Provider) ours(svcs []corev1.Service) map[string]*corev1.Service {
	out := map[string]*corev1.Service{}
	for i := range svcs {
		s := &svcs[i]
		if s.Spec.Type != corev1.ServiceTypeLoadBalancer || !p.owns(s) {
			continue
		}
		if _, dup := out[s.Labels[AddressLabel]]; !dup {
			out[s.Labels[AddressLabel]] = s
		}
	}
	return out
}

// holders is, by address, the LoadBalancer Services (namespace/name, sorted) that request or hold it, but for those
// skip says to leave out.
func holders(svcs []corev1.Service, skip func(*corev1.Service) bool) map[string][]string {
	out := map[string][]string{}
	for i := range svcs {
		s := &svcs[i]
		if s.Spec.Type != corev1.ServiceTypeLoadBalancer || skip(s) {
			continue
		}
		for _, ip := range addressesOf(s) {
			if name := s.Namespace + "/" + s.Name; !slices.Contains(out[ip], name) {
				out[ip] = append(out[ip], name)
			}
		}
	}
	for ip := range out {
		slices.Sort(out[ip])
	}
	return out
}

// addressesOf is every address a LoadBalancer Service requests or holds.
func addressesOf(s *corev1.Service) []string {
	out := ingressOf(s)
	for ip := range strings.SplitSeq(s.Annotations[LoadBalancerIPsAnnotation], ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			out = append(out, ip)
		}
	}
	if s.Spec.LoadBalancerIP != "" { //nolint:staticcheck // deprecated, but MetalLB still honours it
		out = append(out, s.Spec.LoadBalancerIP) //nolint:staticcheck
	}
	return out
}

func ingressOf(s *corev1.Service) []string {
	var out []string
	for _, in := range s.Status.LoadBalancer.Ingress {
		if in.IP != "" {
			out = append(out, in.IP)
		}
	}
	return out
}

func internalIPv4(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if ip, err := netip.ParseAddr(a.Address); a.Type == corev1.NodeInternalIP && err == nil && ip.Is4() {
			return a.Address
		}
	}
	return ""
}

// serviceName is a binding's Service name: sip-<pool>-<address>, or with a hash of the pool's name in place of it
// when that would be too long for a Service name.
func serviceName(pool, ip string) string {
	addr := strings.NewReplacer(".", "-", ":", "-").Replace(ip)
	name := "sip-" + strings.ReplaceAll(pool, ".", "-") + "-" + addr
	if len(name) <= 63 {
		return name
	}
	return "sip-" + hash(pool) + "-" + addr
}

// poolKey is the pool's PoolLabel value: its name, or a hash of a name too long for a label value (63 characters).
func poolKey(pool string) string {
	if len(pool) <= 63 {
		return pool
	}
	return "sha256-" + hash(pool)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:5])
}
