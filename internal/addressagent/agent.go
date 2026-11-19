// Package addressagent is the per-node half of a SIPAddressPool whose addresses reach a node at an address the node
// doesn't already have: an AWS Elastic IP associated with a secondary private address, and later a Local datapath
// (MetalLB). The cloud sends the traffic to the node, but the kernel drops it, and a pod can't bind the address,
// until the address is on an interface. The agent runs on each node, puts the bind addresses the pools give its
// node on the interface that holds the node's own address, and reports what its node has on its own pod, which the
// operator waits for before it gives the fleet's pods their addresses.
package addressagent

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
	"git.instruments.com/ci/freeswitch-operator/internal/addresspool"
)

// labelSuffix marks the addresses the agent adds (the IPv4 address label <interface>:sip), so it only ever removes
// its own.
const labelSuffix = ":sip"

// Addr is an IPv4 address on an interface.
type Addr struct {
	IP    netip.Addr
	Label string
}

// Links is the node's network configuration, as far as the agent touches it.
type Links interface {
	// Interface returns the interface that holds hostIP, and every IPv4 address on any interface.
	Interface(hostIP netip.Addr) (iface string, all []Addr, err error)
	// Add puts ip/32 on the interface with a label.
	Add(iface string, ip netip.Addr, label string) error
	// Del takes ip off the interface.
	Del(iface string, ip netip.Addr) error
}

// Agent keeps one node's bind addresses on its interface.
type Agent struct {
	Client client.Client
	Links  Links
	// Node is the node's name; HostIP its own address.
	Node   string
	HostIP netip.Addr
	// Pod is the agent's own pod, which it annotates.
	Pod types.NamespacedName
}

// Sync is one pass: add what the pools give this node, remove what the agent added that no pool gives it, and
// report the result on the agent's pod.
//
// It removes an address only when every pool that needs node addresses has read its provider, so a provider
// outage never takes an address off a node with calls on it. Deleting such a pool does take its addresses off the
// nodes' interfaces: delete one only when no fleet uses it.
func (a *Agent) Sync(ctx context.Context) error {
	log := logf.FromContext(ctx)
	var pools sipv1alpha1.SIPAddressPoolList
	if err := a.Client.List(ctx, &pools); err != nil {
		return err
	}
	want := map[netip.Addr]bool{}
	settled := true
	for _, p := range pools.Items {
		if !addresspool.NeedsNodeAddress(&p.Spec) {
			continue
		}
		settled = settled && addresspool.ProviderRead(&p.Status)
		for _, s := range addresspool.NodeBindAddresses(&p.Status, a.Node) {
			ip, err := netip.ParseAddr(s)
			if err != nil || !ip.Is4() {
				log.Info("ignoring a bind address that isn't IPv4", "pool", p.Name, "address", s)
				continue
			}
			want[ip] = true
		}
	}

	iface, all, err := a.Links.Interface(a.HostIP)
	if err != nil {
		return err
	}
	label := iface + labelSuffix
	if len(label) > 15 {
		return fmt.Errorf("interface name %q is too long for the address label %q (15 characters at most)", iface, label)
	}
	have := map[netip.Addr]Addr{}
	for _, ad := range all {
		have[ad.IP] = ad
	}
	for _, ip := range slices.SortedFunc(maps.Keys(want), netip.Addr.Compare) {
		if _, ok := have[ip]; ok {
			continue // already there, ours or not (the node's own address, for a pool switched from Primary)
		}
		log.Info("adding bind address", "address", ip, "interface", iface)
		if err := a.Links.Add(iface, ip, label); err != nil {
			return fmt.Errorf("adding %s to %s: %w", ip, iface, err)
		}
		have[ip] = Addr{IP: ip, Label: label}
	}
	if settled {
		for ip, ad := range have {
			if want[ip] || ad.Label != label {
				continue
			}
			log.Info("removing bind address no pool gives this node", "address", ip, "interface", iface)
			if err := a.Links.Del(iface, ip); err != nil {
				return fmt.Errorf("removing %s from %s: %w", ip, iface, err)
			}
			delete(have, ip)
		}
	}

	var present []string
	for ip := range want {
		if _, ok := have[ip]; ok {
			present = append(present, ip.String())
		}
	}
	slices.Sort(present)
	return a.report(ctx, strings.Join(present, ","))
}

func (a *Agent) report(ctx context.Context, value string) error {
	var pod corev1.Pod
	if err := a.Client.Get(ctx, a.Pod, &pod); err != nil {
		return err
	}
	if cur, ok := pod.Annotations[sipv1alpha1.AgentBindAddressesAnnotation]; ok && cur == value {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[sipv1alpha1.AgentBindAddressesAnnotation] = value
	return a.Client.Patch(ctx, &pod, patch)
}
