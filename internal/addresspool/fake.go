package addresspool

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

// Fake is an in-memory Provider for tests. Nodes are named by their Node name. It keeps the providers' rules: Bind
// refuses a bound address, so a move must unbind first.
type Fake struct {
	mu      sync.Mutex
	caps    Capabilities
	order   []string
	ips     map[string]string
	boundTo map[string]string
	// Calls records "bind <id> <node>" and "unbind <id>", in order.
	Calls []string
	// FailBind and FailUnbind make the next call for an address ID fail once.
	FailBind, FailUnbind map[string]error
	// ObserveErr makes Observe fail.
	ObserveErr error
	// BindIPs, when set, are the bind addresses Observe reports for bound addresses, by address ID, as a provider
	// that binds to a secondary private address does.
	BindIPs map[string]string
	// NotReady, when set, is what Observe reports as NotReady for bound addresses, by address ID, as MetalLB's
	// provider does before MetalLB assigns the address.
	NotReady map[string]string
}

// NewFake returns a Fake holding the given addresses, ID to IP, in order.
func NewFake(caps Capabilities, ids []string, ips []string) *Fake {
	f := &Fake{caps: caps, ips: map[string]string{}, boundTo: map[string]string{}, FailBind: map[string]error{}, FailUnbind: map[string]error{}}
	for i, id := range ids {
		f.order = append(f.order, id)
		f.ips[id] = ips[i]
	}
	return f
}

// NATFake is a Fake with AWS-like capabilities.
func NATFake(ids []string, ips []string) *Fake {
	return NewFake(Capabilities{Datapath: sipv1alpha1.AddressDatapathNAT, Fencing: sipv1alpha1.AddressFencingAuthoritative}, ids, ips)
}

func (f *Fake) Capabilities() Capabilities { return f.caps }

func (f *Fake) NodeID(n *corev1.Node) (string, error) { return n.Name, nil }

func (f *Fake) Observe(context.Context) ([]Address, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ObserveErr != nil {
		return nil, f.ObserveErr
	}
	out := make([]Address, 0, len(f.order))
	for _, id := range f.order {
		a := Address{ID: id, IP: f.ips[id], NodeID: f.boundTo[id]}
		if a.NodeID != "" {
			a.BindingID = "binding-" + id
			a.BindIP = f.BindIPs[id]
			a.NotReady = f.NotReady[id]
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *Fake) Bind(_ context.Context, a Address, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("bind %s %s", a.ID, nodeID))
	if err := f.FailBind[a.ID]; err != nil {
		delete(f.FailBind, a.ID)
		return err
	}
	if cur := f.boundTo[a.ID]; cur != "" {
		return fmt.Errorf("%s is already bound to %s", a.ID, cur)
	}
	f.boundTo[a.ID] = nodeID
	return nil
}

func (f *Fake) Unbind(_ context.Context, a Address) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "unbind "+a.ID)
	if err := f.FailUnbind[a.ID]; err != nil {
		delete(f.FailUnbind, a.ID)
		return err
	}
	delete(f.boundTo, a.ID)
	return nil
}

// SetBound binds an address directly, as something outside the operator would.
func (f *Fake) SetBound(id, nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if nodeID == "" {
		delete(f.boundTo, id)
	} else {
		f.boundTo[id] = nodeID
	}
}

// BoundTo returns what an address is bound to.
func (f *Fake) BoundTo(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boundTo[id]
}

// ResetCalls clears the call record.
func (f *Fake) ResetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = nil
}

// CallLog returns a copy of the call record.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}
