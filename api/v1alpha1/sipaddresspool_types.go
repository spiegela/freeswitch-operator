/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SIPAddressPoolSpec names the addresses in a pool and the provider that binds them to nodes. Exactly one provider
// block is set. The operator never allocates or releases an address: peers allow-list them, so the pool is a fixed
// list, and the operator only binds and unbinds the addresses named here.
// +kubebuilder:validation:XValidation:rule="[has(self.awsElasticIP), has(self.metalLB)].filter(x, x).size() == 1",message="exactly one provider must be set"
type SIPAddressPoolSpec struct {
	// awsElasticIP draws the pool from AWS Elastic IPs, associated with each node's instance (1:1 NAT).
	// +optional
	AWSElasticIP *AWSElasticIPPool `json:"awsElasticIP,omitempty"`

	// metalLB draws the pool from addresses MetalLB announces, from each node they are bound to (Local).
	// +optional
	MetalLB *MetalLBPool `json:"metalLB,omitempty"`

	// allowAdvisoryFencing lets the pool take its addresses off a fenced node (one carrying the
	// sip.instruments.com/fenced label, after a failover) when its provider's fencing is Advisory, as MetalLB's is:
	// a node cut off from the cluster but not from the network may go on answering for the address until it is
	// stopped. Authoritative pools always take their addresses off a fenced node. Without it, an Advisory pool leaves
	// the address on the fenced node, Fenced and unused, until the label is removed or the node leaves the cluster.
	// +optional
	AllowAdvisoryFencing bool `json:"allowAdvisoryFencing,omitempty"`
}

// MetalLBPool is a pool of addresses MetalLB announces (L2 or BGP). For each binding the operator makes a
// LoadBalancer Service that asks MetalLB for the address and has it announced from the node it is bound to; the
// operator's address agent puts the address on that node's interface, and the fleets' pods bind it.
type MetalLBPool struct {
	// addresses in the pool, IPv4. Each must be in a MetalLB IPAddressPool, and no other Service may request it.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:Format=ipv4
	// +listType=set
	Addresses []string `json:"addresses"`

	// ipAddressPool is the MetalLB IPAddressPool the addresses are in (metallb.io/address-pool on each Service).
	// Empty lets MetalLB find the pool that holds each address.
	// +optional
	IPAddressPool string `json:"ipAddressPool,omitempty"`
}

// AWSElasticIPPool is a pool of existing Elastic IPs in one region.
type AWSElasticIPPool struct {
	// region the Elastic IPs and the edge nodes are in.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// allocationIDs of the Elastic IPs in the pool. The operator associates them with edge nodes' instances and
	// disassociates them, and never touches any other Elastic IP. Removing an ID leaves its address wherever it is.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:Pattern=`^eipalloc-[0-9a-f]+$`
	// +listType=set
	AllocationIDs []string `json:"allocationIDs"`

	// privateAddress is the private address on each node that an Elastic IP translates to. Primary (the default)
	// uses the node's own address, the primary private address of its primary network interface, and refuses a
	// node that already holds an Elastic IP outside the pool. Secondary assigns the node's primary interface a
	// secondary private address for the Elastic IP, which leaves an Elastic IP already on the node's own address
	// (a cluster endpoint, say) alone; the operator's address agent adds it to the node's interface, and the
	// fleets' pods bind it (sip.instruments.com/bind-address). Secondary needs ec2:AssignPrivateIpAddresses,
	// ec2:UnassignPrivateIpAddresses, ec2:CreateTags and ec2:DeleteTags too. Don't use it on nodes whose CNI hands
	// out secondary addresses of the primary interface to pods (the VPC CNI does): it could hand out this one, fill the
	// interface's address limit, or release it. The pool refuses new bindings to nodes running the VPC CNI's aws-node.
	// +kubebuilder:validation:Enum=Primary;Secondary
	// +kubebuilder:default=Primary
	// +optional
	PrivateAddress AWSPrivateAddress `json:"privateAddress,omitempty"`
}

// AWSPrivateAddress is which private address on a node an Elastic IP is associated with.
type AWSPrivateAddress string

const (
	// AWSPrivateAddressPrimary associates the Elastic IP with the node's own address.
	AWSPrivateAddressPrimary AWSPrivateAddress = "Primary"
	// AWSPrivateAddressSecondary associates it with a secondary private address assigned for it.
	AWSPrivateAddressSecondary AWSPrivateAddress = "Secondary"
)

// AddressDatapath says how traffic for a pool's address reaches the node it is bound to.
// +kubebuilder:validation:Enum=NAT;Local
type AddressDatapath string

const (
	// AddressDatapathNAT means the node sees only its private address and the provider translates 1:1, so a pod
	// binds its private address and advertises the pool's.
	AddressDatapathNAT AddressDatapath = "NAT"
	// AddressDatapathLocal means the address arrives at the node itself, so a pod binds and advertises it.
	AddressDatapathLocal AddressDatapath = "Local"
)

// AddressFencing says what unbinding an address guarantees.
// +kubebuilder:validation:Enum=Authoritative;Advisory
type AddressFencing string

const (
	// AddressFencingAuthoritative means unbinding takes the address off a node whether or not the node is alive.
	AddressFencingAuthoritative AddressFencing = "Authoritative"
	// AddressFencingAdvisory means a node cut off from the cluster may go on holding the address.
	AddressFencingAdvisory AddressFencing = "Advisory"
)

// The pool's conditions. ProviderAvailable is True while the pool reads its provider, so its status is current; it
// is False with reason ProviderUnavailable (no provider) or ProviderError (the provider can't answer).
const (
	PoolConditionReady             = "Ready"
	PoolConditionProviderAvailable = "ProviderAvailable"
	PoolReasonProviderUnavailable  = "ProviderUnavailable"
	PoolReasonProviderError        = "ProviderError"
)

// PoolAddressState is where one of a pool's addresses stands.
// +kubebuilder:validation:Enum=Free;Bound;Idle;Fenced;Conflict;Error
type PoolAddressState string

const (
	// PoolAddressFree means the address is bound to nothing and can be given to a node.
	PoolAddressFree PoolAddressState = "Free"
	// PoolAddressBound means the address is bound to a node running a pod that uses the pool.
	PoolAddressBound PoolAddressState = "Bound"
	// PoolAddressIdle means the address is bound to a node with no pod using the pool, such as a spare. It stays
	// there until a node that needs an address finds no free one.
	PoolAddressIdle PoolAddressState = "Idle"
	// PoolAddressFenced means the address is bound to a node a failover fenced, and the pool may not take it off
	// (its provider's fencing is Advisory, and it doesn't set allowAdvisoryFencing), since the node may still answer
	// for it. It stays there, unused and never moved, until the node is unfenced or leaves the cluster.
	PoolAddressFenced PoolAddressState = "Fenced"
	// PoolAddressConflict means the address is bound to something outside the cluster that the pool didn't bind it
	// to. The operator leaves it alone.
	PoolAddressConflict PoolAddressState = "Conflict"
	// PoolAddressError means the provider couldn't report on the address, or binding or unbinding it failed.
	PoolAddressError PoolAddressState = "Error"
)

// AddressClaim is a pod that uses a pool's address.
type AddressClaim struct {
	// kind of the fleet the pod belongs to: FreeswitchCluster or KamailioCluster.
	Kind string `json:"kind"`
	// namespace of the fleet and pod.
	Namespace string `json:"namespace"`
	// name of the fleet.
	Name string `json:"name"`
	// pod name.
	Pod string `json:"pod"`
}

// PoolAddress is one of a pool's addresses as the provider last reported it.
type PoolAddress struct {
	// id is the provider's name for the address, such as an Elastic IP's allocation ID.
	ID string `json:"id"`
	// address is the IP address peers see.
	// +optional
	Address string `json:"address,omitempty"`
	// bindAddress is the address on the node that pods bind to send and receive on this one, when it isn't the
	// node's own: the secondary private address a NAT translates it to, or the address itself on a Local datapath.
	// +optional
	BindAddress string `json:"bindAddress,omitempty"`
	// bindAddressReady means the address agent on the node reports bindAddress on the node's interface. Pods are
	// given an address with a bindAddress only once it is: a pod that binds an address the node doesn't have yet
	// fails, and both are read once, at start.
	// +optional
	BindAddressReady bool `json:"bindAddressReady,omitempty"`
	// state of the address.
	State PoolAddressState `json:"state"`
	// node the address is bound to.
	// +optional
	Node string `json:"node,omitempty"`
	// nodeID is the provider's name for what the address is bound to, such as an instance ID.
	// +optional
	NodeID string `json:"nodeID,omitempty"`
	// claimedBy are the pods on the node that use the address.
	// +optional
	ClaimedBy []AddressClaim `json:"claimedBy,omitempty"`
	// lastClaimedBy are the pods that used the address most recently. A pod that comes back (a StatefulSet pod
	// replaced onto a new node) gets the same address when it is free.
	// +optional
	LastClaimedBy []AddressClaim `json:"lastClaimedBy,omitempty"`
	// message explains a Conflict or Error.
	// +optional
	Message string `json:"message,omitempty"`
}

// SIPAddressPoolStatus is the pool as the provider last reported it.
type SIPAddressPoolStatus struct {
	// observedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// datapath of the pool's provider.
	// +optional
	Datapath AddressDatapath `json:"datapath,omitempty"`

	// fencing of the pool's provider.
	// +optional
	Fencing AddressFencing `json:"fencing,omitempty"`

	// addresses in the pool, in the order of the spec.
	// +listType=map
	// +listMapKey=id
	// +optional
	Addresses []PoolAddress `json:"addresses,omitempty"`

	// conditions: Ready (every node running a pod that uses the pool holds one of its addresses, ready to use, and
	// the provider answered), Degraded (a node is waiting for an address, or an address is in Conflict or Error),
	// and ProviderAvailable (the pool read its provider on its last pass).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sap
// +kubebuilder:printcolumn:name="Datapath",type=string,JSONPath=`.status.datapath`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SIPAddressPool is a fixed list of public addresses the operator binds to edge nodes, one per node, for the
// FreeswitchClusters and KamailioClusters that use it. It is cluster-scoped: addresses and nodes are cluster-wide.
type SIPAddressPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SIPAddressPool
	// +required
	Spec SIPAddressPoolSpec `json:"spec"`

	// status defines the observed state of SIPAddressPool
	// +optional
	Status SIPAddressPoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SIPAddressPoolList contains a list of SIPAddressPool
type SIPAddressPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SIPAddressPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SIPAddressPool{}, &SIPAddressPoolList{})
		return nil
	})
}
