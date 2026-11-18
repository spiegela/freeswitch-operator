// Package addresspool holds what SIPAddressPools share whatever provides their addresses: the Provider interface
// each provider implements, and the planner that decides which address goes to which node.
package addresspool

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

// Capabilities are fixed for a provider: how its addresses reach a node, and what unbinding guarantees.
type Capabilities struct {
	Datapath sipv1alpha1.AddressDatapath
	Fencing  sipv1alpha1.AddressFencing
}

// Address is one of a pool's addresses as its provider reports it.
type Address struct {
	// ID is the provider's name for the address (an Elastic IP's allocation ID).
	ID string
	// IP is the address peers see.
	IP string
	// NodeID is the provider's name for what the address is bound to now (an instance ID), or empty when unbound.
	NodeID string
	// BindingID is the provider's handle on the current binding (an association ID), for Unbind.
	BindingID string
	// Err is set when the provider couldn't report on this address; the rest of the fields may be empty.
	Err error
}

// Provider binds a pool's addresses to nodes. It holds no policy: which address goes where is the planner's.
type Provider interface {
	// Capabilities are the provider's datapath and fencing.
	Capabilities() Capabilities
	// NodeID is the provider's name for a node, the one Observe reports and Bind takes.
	NodeID(n *corev1.Node) (string, error)
	// Observe reports every address in the pool's spec, in spec order, with where each is bound now. It is the
	// truth the planner works from; the pool's status is only a record of it.
	Observe(ctx context.Context) ([]Address, error)
	// Bind binds an unbound address to a node. It refuses an address that is bound anywhere, and a node that
	// already holds an address the pool doesn't own.
	Bind(ctx context.Context, a Address, nodeID string) error
	// Unbind takes an address off whatever holds it.
	Unbind(ctx context.Context, a Address) error
}

// Factory builds the provider for a pool, or returns an error the pool reports (a provider not enabled, say).
type Factory func(ctx context.Context, pool *sipv1alpha1.SIPAddressPool) (Provider, error)

// ProviderName is the name of the provider a pool's spec selects, as the manager's --address-providers flag
// spells it, or empty when none is set.
func ProviderName(spec *sipv1alpha1.SIPAddressPoolSpec) string {
	switch {
	case spec.AWSElasticIP != nil:
		return "aws"
	default:
		return ""
	}
}

// Registry is the providers the manager enabled, by name.
type Registry map[string]Factory

// Factory returns a Factory that picks the provider each pool's spec selects, refusing those not enabled.
func (r Registry) Factory() Factory {
	return func(ctx context.Context, pool *sipv1alpha1.SIPAddressPool) (Provider, error) {
		name := ProviderName(&pool.Spec)
		if name == "" {
			return nil, fmt.Errorf("no provider set")
		}
		f, ok := r[name]
		if !ok {
			enabled := make([]string, 0, len(r))
			for k := range r {
				enabled = append(enabled, k)
			}
			slices.Sort(enabled)
			return nil, fmt.Errorf("provider %q is not enabled (--address-providers=%s)", name, strings.Join(enabled, ","))
		}
		return f(ctx, pool)
	}
}
