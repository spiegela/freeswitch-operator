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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KamailioClusterLabel names the KamailioCluster a pod belongs to. It is its own key, so a Kamailio pod is never
// taken for a FreeSWITCH one (ClusterLabel).
const KamailioClusterLabel = "sip.instruments.com/kamailio-cluster"

// KamailioRPCSpec configures the JSON-RPC port the operator drives Kamailio through.
type KamailioRPCSpec struct {
	// port for JSON-RPC over HTTP. It listens on the pod's own address only, and every request needs the
	// token in the <name>-jsonrpc Secret. Keep it closed to the outside all the same.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5070
	// +optional
	Port int32 `json:"port,omitempty"`
}

// KamailioDrainSpec configures how a Kamailio pod stops.
type KamailioDrainSpec struct {
	// maxDrainSeconds is how long a stopping pod waits for its open transactions to finish, answering new
	// calls with 503 meanwhile. Kamailio holds transactions, not calls, so this is short.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=40
	// +optional
	MaxDrainSeconds int64 `json:"maxDrainSeconds,omitempty"`
}

// DispatchTarget is one FreeSWITCH fleet Kamailio sends calls to.
type DispatchTarget struct {
	// freeswitchCluster names a FreeswitchCluster in this namespace. It must set spec.dispatcher: its setID and
	// the list it publishes are what Kamailio loads.
	// +kubebuilder:validation:MinLength=1
	FreeswitchCluster string `json:"freeswitchCluster"`
}

// KamailioClusterSpec defines the desired state of KamailioCluster.
// +kubebuilder:validation:XValidation:rule="self.sip.port != self.rpc.port",message="sip.port and rpc.port must differ"
type KamailioClusterSpec struct {
	// replicas is the number of Kamailio pods, one per node.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// image is the Kamailio image (6.0 or later), with kamailio, /bin/sh, wget and nc. Pin it by digest.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// imagePullPolicy for the image.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// imagePullSecrets for the image.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// resources for the Kamailio container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// env adds environment variables to the Kamailio container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// nodeSelector picks the nodes that hold the edge's signaling addresses.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations for those nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// sip configures the SIP port peers reach Kamailio on.
	// +kubebuilder:default={}
	// +optional
	SIP SIPSpec `json:"sip,omitzero"`

	// rpc configures the operator's JSON-RPC port.
	// +kubebuilder:default={}
	// +optional
	RPC KamailioRPCSpec `json:"rpc,omitzero"`

	// externalAddress says what each pod advertises in Via and Record-Route. A pod reads it once, when it
	// starts. With source None it is the node's own address.
	// +optional
	ExternalAddress ExternalAddressSpec `json:"externalAddress,omitzero"`

	// drain settings.
	// +optional
	Drain KamailioDrainSpec `json:"drain,omitzero"`

	// dispatch lists the FreeSWITCH fleets new calls go to, in priority order: a call goes to the first fleet
	// with an active node, and fails over to the next. Changing this list rolls the Kamailio pods; changes in
	// each fleet's membership reach them live.
	// +listType=map
	// +listMapKey=freeswitchCluster
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Dispatch []DispatchTarget `json:"dispatch,omitempty"`

	// reloadDelaySeconds is how long after a dispatcher list change the operator asks Kamailio to reload it,
	// to give the kubelet time to update the mounted file. It then checks the list was loaded, and reloads
	// again every 10 seconds until it is.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=75
	// +optional
	ReloadDelaySeconds int32 `json:"reloadDelaySeconds,omitempty"`
}

// DispatchSet is one fleet in the combined dispatcher list.
type DispatchSet struct {
	// freeswitchCluster the set comes from.
	FreeswitchCluster string `json:"freeswitchCluster"`
	// setID of the set in Kamailio's dispatcher.
	SetID int32 `json:"setID"`
	// destinations is the number of FreeSWITCH nodes in the set.
	Destinations int32 `json:"destinations"`
}

// KamailioClusterStatus defines the observed state of KamailioCluster.
type KamailioClusterStatus struct {
	// observedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// replicas is the number of pods that exist.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// readyReplicas is the number of pods ready to take calls.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// updatedReplicas is the number of pods on the latest pod template.
	// +optional
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`

	// currentRevision is the revision most pods were created from.
	// +optional
	CurrentRevision string `json:"currentRevision,omitempty"`

	// updateRevision is the revision pods are being moved to.
	// +optional
	UpdateRevision string `json:"updateRevision,omitempty"`

	// members, one per pod, in name order.
	// +listType=map
	// +listMapKey=pod
	// +optional
	Members []Member `json:"members,omitempty"`

	// sets in the combined dispatcher list, in priority order.
	// +optional
	Sets []DispatchSet `json:"sets,omitempty"`

	// dispatcherHash is the content hash of the combined dispatcher list last written.
	// +optional
	DispatcherHash string `json:"dispatcherHash,omitempty"`

	// dispatcherReloadedHash is the content hash every ready Kamailio pod was last confirmed to hold.
	// +optional
	DispatcherReloadedHash string `json:"dispatcherReloadedHash,omitempty"`

	// dispatcherUpdatedAt is when the combined list last changed.
	// +optional
	DispatcherUpdatedAt *metav1.Time `json:"dispatcherUpdatedAt,omitempty"`

	// conditions: Ready (every member ready and current), Available (at least one member takes calls),
	// Progressing (a rolling update is under way), Degraded (fewer ready members than replicas) and SetConflict
	// (a fleet in dispatch is left out: missing, without spec.dispatcher, or sharing another's setID).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kc
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KamailioCluster is a fleet of Kamailio proxies holding the edge's signaling addresses and spreading calls over
// one or more FreeswitchClusters.
type KamailioCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of KamailioCluster
	// +required
	Spec KamailioClusterSpec `json:"spec"`

	// status defines the observed state of KamailioCluster
	// +optional
	Status KamailioClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// KamailioClusterList contains a list of KamailioCluster
type KamailioClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []KamailioCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &KamailioCluster{}, &KamailioClusterList{})
		return nil
	})
}
