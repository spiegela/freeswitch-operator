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

// Annotation and label keys the operator reads and writes.
const (
	// ExternalAddressAnnotation is set by the operator on each FreeSWITCH pod to the public address peers
	// should see. The pod reads it from its downward-API volume, which updates in place.
	ExternalAddressAnnotation = "sip.instruments.com/external-address"
	// BindAddressAnnotation is set by the operator on each pod of a fleet whose pool needs an address on the node
	// (a secondary private address, or a Local datapath): the address the pod binds its peer-facing traffic to
	// (FreeSWITCH's RTP, Kamailio's peer listener). The operator sets it, and the external address with it, only
	// once the address agent reports the node has the address on an interface. When it differs from the pod's own
	// address, the external address reaches only it: whatever listens elsewhere (FreeSWITCH's SIP, on the pod's
	// own address) must not advertise the external address (ext-sip-ip), or in-dialog requests sent to it are lost.
	BindAddressAnnotation = "sip.instruments.com/bind-address"
	// AgentBindAddressesAnnotation is set by the address agent on its own pod: the pools' bind addresses its node
	// has on an interface, comma-separated and sorted.
	AgentBindAddressesAnnotation = "sip.instruments.com/bind-addresses"
	// AppNameLabel is the well-known label naming an application.
	AppNameLabel = "app.kubernetes.io/name"
	// AddressAgentName is the AppNameLabel of the address agent's pods.
	AddressAgentName = "freeswitch-operator-address-agent"

	// ClusterLabel names the FreeswitchCluster a pod belongs to.
	ClusterLabel = "sip.instruments.com/cluster"

	// DispatcherHashAnnotation records the content hash of a rendered dispatcher list on its ConfigMap.
	DispatcherHashAnnotation = "sip.instruments.com/dispatcher-hash"
	// DispatcherUpdatedAtAnnotation records when the dispatcher list's content last changed (RFC 3339), written in
	// the same update as the list, so the reload delay is measured from the change itself.
	DispatcherUpdatedAtAnnotation = "sip.instruments.com/dispatcher-updated-at"
	// FailOverAnnotation, set to "true" on a FreeSWITCH pod by a person, has the operator fail that member over now,
	// whatever spec.failover.mode says and whatever its health: fence its node, delete the pod at once, and let the
	// StatefulSet start it again on another node. Any other value ("false" included) does nothing.
	FailOverAnnotation = "sip.instruments.com/fail-over"
	// FirstReadyAnnotation is set by the operator on a FreeSWITCH pod the first time it sees the pod ready (RFC 3339).
	// Only a pod that has been ready is failed over automatically, or counts as NotReady: one that never was (a bad
	// configuration, a broken image) would fail the same way on any node.
	FirstReadyAnnotation = "sip.instruments.com/first-ready"
	// FencedLabel marks a node fenced: the operator puts it on a node when it fails over a member there. No fleet
	// pod is scheduled to the node again (each fleet's pods have node affinity against it), and every SIPAddressPool
	// that may fence takes its addresses off the node. The operator never removes it; a person does, once the node
	// is known to be sound (kubectl label node <node> sip.instruments.com/fenced-), or the node is replaced.
	FencedLabel = "sip.instruments.com/fenced"
	// FencedTaint (effect NoSchedule) is put on a fenced node beside FencedLabel, to keep other pods off it too.
	// Remove it with the label (kubectl taint node <node> sip.instruments.com/fenced-).
	FencedTaint = "sip.instruments.com/fenced"
	// FencedByAnnotation records on a fenced node which member's failover fenced it, when, and why.
	FencedByAnnotation = "sip.instruments.com/fenced-by"
)

// ExternalAddressSource says where a FreeSWITCH pod's public address comes from.
// +kubebuilder:validation:Enum=NodeExternalIP;AddressPool;None
type ExternalAddressSource string

const (
	// ExternalAddressNodeExternalIP uses the ExternalIP address of the node the pod runs on.
	ExternalAddressNodeExternalIP ExternalAddressSource = "NodeExternalIP"
	// ExternalAddressAddressPool uses the address a SIPAddressPool binds to the node the pod runs on.
	ExternalAddressAddressPool ExternalAddressSource = "AddressPool"
	// ExternalAddressNone gives the pod no external address; FreeSWITCH uses its local address.
	ExternalAddressNone ExternalAddressSource = "None"
)

// DispatcherTransport is the transport Kamailio uses to reach FreeSWITCH.
// +kubebuilder:validation:Enum=udp;tcp
type DispatcherTransport string

const (
	DispatcherTransportUDP DispatcherTransport = "udp"
	DispatcherTransportTCP DispatcherTransport = "tcp"
)

// MemberState is a FreeSWITCH pod's place in the fleet.
type MemberState string

const (
	// MemberStarting means the pod exists but is not ready to take calls.
	MemberStarting MemberState = "Starting"
	// MemberActive means the pod is ready and takes new calls.
	MemberActive MemberState = "Active"
	// MemberDraining means the pod is terminating: it takes no new calls and waits for live calls to end.
	MemberDraining MemberState = "Draining"
)

// FailoverMode says what the operator does about a member it finds Failed.
// +kubebuilder:validation:Enum=Detect;Automatic
type FailoverMode string

const (
	// FailoverDetect reports each member's health (status, metrics, events) and acts only on pods annotated
	// sip.instruments.com/fail-over.
	FailoverDetect FailoverMode = "Detect"
	// FailoverAutomatic also fails over a member found Failed, one member at a time.
	FailoverAutomatic FailoverMode = "Automatic"
)

// HealthState is the operator's verdict on one member.
type HealthState string

const (
	// HealthHealthy means no health signal is failing.
	HealthHealthy HealthState = "Healthy"
	// HealthSuspect means at least one signal is failing, but not enough of them, or not for long enough, to call
	// the member Failed.
	HealthSuspect HealthState = "Suspect"
	// HealthFailed means spec.failover.signalsRequired signals have failed together for failAfterSeconds.
	HealthFailed HealthState = "Failed"
)

// HealthSignalType is one independent view of a member's health.
type HealthSignalType string

const (
	// SignalNodeLost means the control plane has lost the member's node: its Ready condition is Unknown (the kubelet
	// stopped reporting) or False, or the Node is gone.
	SignalNodeLost HealthSignalType = "NodeLost"
	// SignalNotReady means a pod that has been ready is unready on a node the control plane still hears from: the
	// kubelet's own checks of FreeSWITCH fail, or the container is restarting.
	SignalNotReady HealthSignalType = "NotReady"
	// SignalSIPUnreachable means the operator's SIP OPTIONS probes to the member's SIP port go unanswered.
	SignalSIPUnreachable HealthSignalType = "SIPUnreachable"
)

// FailoverTrigger says what started a failover.
type FailoverTrigger string

const (
	// FailoverTriggerAutomatic is a failover the operator started on its own (spec.failover.mode Automatic).
	FailoverTriggerAutomatic FailoverTrigger = "Automatic"
	// FailoverTriggerManual is a failover a person asked for with the sip.instruments.com/fail-over annotation.
	FailoverTriggerManual FailoverTrigger = "Manual"
)

// FailoverSpec configures how the operator watches each member's health and what it does about a member that has
// failed. Three independent signals feed the verdict: the node (the control plane's view of the kubelet), the
// pod's readiness (the kubelet's checks of FreeSWITCH itself), and SIP OPTIONS probes the operator sends to each
// member. A signal that fails for every member of a fleet of two or more at once is taken to be the observer's
// problem rather than the members', and doesn't count. Failing over a member fences its node (the
// sip.instruments.com/fenced taint), so no fleet pod lands there again and address pools take their addresses off
// it, then deletes the pod at once: the StatefulSet starts it again on another node, normally the spare. Calls up on
// the failed member are lost.
type FailoverSpec struct {
	// mode is Detect (report health; fail over only pods a person annotates sip.instruments.com/fail-over) or
	// Automatic (also fail over a member found Failed, one at a time, and only one that has been ready).
	// +kubebuilder:default=Detect
	// +optional
	Mode FailoverMode `json:"mode,omitempty"`
	// signalsRequired is how many independent signals must fail together before a member is Failed. With 2, a node
	// whose kubelet is lost but whose FreeSWITCH still answers SIP stays Suspect; with 1, any one signal is enough.
	// The readiness signal doesn't count while the node is lost (the node's loss is what makes the pod unready), so
	// 2 is the most there can be.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2
	// +kubebuilder:default=2
	// +optional
	SignalsRequired int32 `json:"signalsRequired,omitempty"`
	// failAfterSeconds is how long that many signals must keep failing before the member is Failed.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=15
	// +optional
	FailAfterSeconds *int32 `json:"failAfterSeconds,omitempty"`
	// notReadySeconds is how long a pod that has been ready must stay unready before readiness counts as a failing
	// signal, so a brief blip doesn't make a member Suspect.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=20
	// +optional
	NotReadySeconds *int32 `json:"notReadySeconds,omitempty"`
	// sipProbe configures the operator's SIP OPTIONS probes. Any SIP response counts as an answer (a 403 from an ACL
	// or a 503 while busy too): the probe asks whether FreeSWITCH's SIP stack is alive and reachable, not whether it
	// would take a call.
	// +optional
	SIPProbe SIPProbeSpec `json:"sipProbe,omitzero"`
}

// SIPProbeSpec configures the SIP OPTIONS probes the operator sends to each member, over the fleet's dispatcher
// transport (UDP unless spec.dispatcher says tcp) to its SIP port. The operator's pods must be able to reach the
// edge nodes' SIP port, as Kamailio's do.
// +kubebuilder:validation:XValidation:rule="self.timeoutSeconds <= self.periodSeconds",message="timeoutSeconds must not exceed periodSeconds"
type SIPProbeSpec struct {
	// periodSeconds between probes of each member.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	// +kubebuilder:default=5
	// +optional
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`
	// timeoutSeconds each probe waits for an answer, retransmitting over UDP as a SIP client would.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	// +kubebuilder:default=2
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// failureThreshold is how many probes in a row must go unanswered before the signal fails.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=3
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// SIPSpec configures the SIP side of each FreeSWITCH node.
type SIPSpec struct {
	// port is the SIP port of the profile Kamailio dispatches to. With host networking it is opened on the node.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5060
	// +optional
	Port int32 `json:"port,omitempty"`
}

// RTPSpec configures the media port range. It must match the node security group or firewall.
// +kubebuilder:validation:XValidation:rule="self.portRangeStart < self.portRangeEnd",message="portRangeStart must be below portRangeEnd"
type RTPSpec struct {
	// portRangeStart is the first RTP port.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=16384
	PortRangeStart int32 `json:"portRangeStart"`

	// portRangeEnd is the last RTP port.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=32768
	PortRangeEnd int32 `json:"portRangeEnd"`
}

// ExternalAddressSpec says how each pod learns the public address it advertises.
// +kubebuilder:validation:XValidation:rule="(has(self.source) && self.source == 'AddressPool') == has(self.addressPoolRef)",message="addressPoolRef is set exactly when source is AddressPool"
type ExternalAddressSpec struct {
	// source of the address.
	// +kubebuilder:default=NodeExternalIP
	// +optional
	Source ExternalAddressSource `json:"source,omitempty"`

	// addressPoolRef names the SIPAddressPool to draw from when source is AddressPool. Pools are cluster-scoped.
	// +optional
	AddressPoolRef *AddressPoolReference `json:"addressPoolRef,omitempty"`

	// required makes a pod unready until it has an external address, so it takes no calls it couldn't
	// answer correctly. Ignored when source is None.
	// +kubebuilder:default=true
	// +optional
	Required *bool `json:"required,omitempty"`
}

// AddressPoolReference names a SIPAddressPool.
type AddressPoolReference struct {
	// name of the SIPAddressPool.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ConfigSpec assembles FreeSWITCH's configuration directory from ConfigMaps.
type ConfigSpec struct {
	// mountPath is the read-only directory the configuration is assembled in. Point FreeSWITCH at it with
	// `-conf`, and keep the image's entrypoint from writing to it. It must neither contain nor sit inside the
	// operator's own directories (/etc/freeswitch-operator/scripts and /etc/freeswitch-operator/podinfo), and
	// must not hide /tmp.
	// +kubebuilder:default="/etc/freeswitch-operator/config"
	// +kubebuilder:validation:Pattern=`^/[^:]*$`
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// configMapRefs are laid out under mountPath, each in its own directory, so a tree such as
	// autoload_configs/, sip_profiles/ and dialplan/ can be built from several ConfigMaps. Pods mount immutable
	// copies the operator takes, never the ConfigMaps themselves, so an edit reaches the fleet only by rolling
	// it, one drained node at a time. The order of the list doesn't matter.
	// +listType=map
	// +listMapKey=name
	// +optional
	ConfigMapRefs []ConfigMapRef `json:"configMapRefs,omitempty"`
}

// ConfigMapRef places one ConfigMap's keys, as files, in a directory under spec.config.mountPath.
type ConfigMapRef struct {
	// name of a ConfigMap in the cluster's namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// path is the directory under mountPath that receives the ConfigMap's keys: "." for mountPath itself,
	// or a relative path such as "autoload_configs". Defaults to the ConfigMap's name.
	// +kubebuilder:validation:Pattern=`^(\.|[A-Za-z0-9_-][A-Za-z0-9_.-]*(/[A-Za-z0-9_-][A-Za-z0-9_.-]*)*)$`
	// +optional
	Path string `json:"path,omitempty"`
}

// DrainSpec bounds how long a node waits for its calls to finish before it stops.
type DrainSpec struct {
	// maxDrainSeconds is the longest a terminating pod waits for live calls to end.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=7200
	// +optional
	MaxDrainSeconds int64 `json:"maxDrainSeconds,omitempty"`
}

// LivenessSpec turns on hang detection: the kubelet restarts a FreeSWITCH that stops answering fs_cli for
// periodSeconds * failureThreshold seconds in a row. A restart drops every call on the node, and fs_cli can be
// slow under load, so the defaults wait about five minutes before acting.
// +kubebuilder:validation:XValidation:rule="self.timeoutSeconds <= self.periodSeconds",message="timeoutSeconds must not exceed periodSeconds"
type LivenessSpec struct {
	// periodSeconds between checks.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:default=30
	// +optional
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// timeoutSeconds is how long FreeSWITCH has to answer each check.
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:default=15
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// failureThreshold is how many checks in a row must go unanswered before the container restarts.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=10
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// RecoverySpec turns on call recovery when FreeSWITCH starts. Each time FreeSWITCH starts in a pod (a new pod, or
// the kubelet restarting a crashed one) the operator's postStart hook waits for its SIP profiles to run, then runs
// `sofia recover`: FreeSWITCH takes over every call leg tracked under the name of a profile it runs, and re-invites
// both far ends. The pod turns ready only after that, so it takes no new calls before the old ones are back.
//
// Recovery takes the calls of the pod's address only when every SIP profile is named for it, as NAME-<address>
// (each character but letters and digits a dash: trunks-203-0-113-10; docs/call-tracking.md). The hook checks the
// names and recovers nothing when one doesn't end that way: a profile under a name every node shares would take
// other nodes' live calls. The result is in the container's log and in /tmp/freeswitch-operator-recovered.
type RecoverySpec struct{}

// FreeswitchMetricsSpec runs a Prometheus exporter in each FreeSWITCH pod. It reads FreeSWITCH over its event
// socket on 127.0.0.1 (sessions, calls, whether inbound calls are paused, each SIP profile's state and call
// counters) and serves them at /metrics on the pod's own address, for a Prometheus server or an OpenTelemetry
// Collector's prometheus receiver to scrape. It runs as a native sidecar, so it keeps reporting while the node
// drains and stops only after FreeSWITCH has.
type FreeswitchMetricsSpec struct {
	// exporterImage is the image of the operator's freeswitch-exporter; pin it by digest. The exporter is part of
	// the pod template, so changing the image rolls the fleet one drained node at a time, like any other
	// template change.
	// +kubebuilder:validation:MinLength=1
	ExporterImage string `json:"exporterImage"`

	// exporterImagePullPolicy for the exporter container.
	// +optional
	ExporterImagePullPolicy corev1.PullPolicy `json:"exporterImagePullPolicy,omitempty"`

	// port for GET /metrics. With host networking it is a port on the node: keep it closed to the outside.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=9282
	// +optional
	Port int32 `json:"port,omitempty"`

	// eventSocket is where the exporter reaches FreeSWITCH.
	// +optional
	EventSocket EventSocketSpec `json:"eventSocket,omitzero"`

	// resources for the exporter container. Unset: a small request (10m CPU, 16Mi) and a 64Mi memory limit.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// EventSocketSpec says how to reach FreeSWITCH's event socket (mod_event_socket) from inside the pod.
type EventSocketSpec struct {
	// port the event socket listens on at 127.0.0.1 (or on all addresses).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8021
	// +optional
	Port int32 `json:"port,omitempty"`

	// passwordSecretRef names a key of a Secret in this namespace holding the event socket password. Unset means
	// FreeSWITCH's default, ClueCon, which fs_cli (and so the operator's probe and drain scripts) also use.
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`
}

// DispatcherSpec describes the Kamailio dispatcher set that sends calls to this fleet.
type DispatcherSpec struct {
	// setID is the dispatcher set for this fleet. Sets 1-99 are for fleets; the operator assigns sets it creates
	// (peers, inside profiles) from 100 up, so the two never collide.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=99
	// +kubebuilder:default=1
	// +optional
	SetID int32 `json:"setID,omitempty"`

	// configMapName is the ConfigMap, in this namespace, that the operator writes dispatcher.list into.
	// Kamailio mounts it (never with subPath, which does not update).
	// +kubebuilder:validation:MinLength=1
	ConfigMapName string `json:"configMapName"`

	// transport Kamailio uses to reach FreeSWITCH.
	// +kubebuilder:default=udp
	// +optional
	Transport DispatcherTransport `json:"transport,omitempty"`

	// jsonrpcURL is for a Kamailio you run yourself; leave it empty when a KamailioCluster dispatches to this
	// fleet, since that drives its own pods. When set, it is Kamailio's JSON-RPC endpoint (jsonrpcs over xhttp,
	// without authentication). The operator uses it to
	// take a leaving node out at once (dispatcher.set_state) and to reload the list once the kubelet has
	// written the new file (dispatcher.reload). When the host names a Service in this cluster ("name",
	// "name.namespace" or "name.namespace.svc[.<cluster domain>]"), each call goes to every ready pod behind it,
	// on the target port of the Service port the URL names, so that every Kamailio hears of each change; any
	// other URL is called as given.
	// +optional
	JSONRPCURL string `json:"jsonrpcURL,omitempty"`

	// reloadDelaySeconds is how long after writing the ConfigMap the operator waits before asking Kamailio to
	// reload, so the kubelet has synced the mounted file. After each reload the operator checks (dispatcher.list)
	// that the Kamailio holds the list it wrote, and reloads again every 10 seconds until it does, so a kubelet
	// slower than this delay costs time but never leaves a Kamailio on the old list.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=75
	// +optional
	ReloadDelaySeconds int32 `json:"reloadDelaySeconds,omitempty"`
}

// FreeswitchClusterSpec defines the desired state of FreeswitchCluster.
// +kubebuilder:validation:XValidation:rule="!has(self.metrics) || self.metrics.port != (has(self.sip) && has(self.sip.port) ? self.sip.port : 5060)",message="metrics.port must differ from sip.port"
// +kubebuilder:validation:XValidation:rule="!has(self.recovery) || !has(self.externalAddress) || ((!has(self.externalAddress.source) || self.externalAddress.source != 'None') && (!has(self.externalAddress.required) || self.externalAddress.required))",message="recovery needs a required external address: externalAddress.source other than None, and required true"
type FreeswitchClusterSpec struct {
	// replicas is the number of FreeSWITCH nodes. Each runs on its own Kubernetes node. Keep one spare.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// image is the FreeSWITCH image. It must contain freeswitch, fs_cli and /bin/sh.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// imagePullPolicy for the FreeSWITCH container.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// imagePullSecrets for the FreeSWITCH image.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// command overrides the image's entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// args overrides the image's arguments.
	// +optional
	Args []string `json:"args,omitempty"`

	// env adds environment variables to the FreeSWITCH container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// resources for the FreeSWITCH container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// nodeSelector pins the fleet to the edge node group.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations let the fleet run on tainted edge nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// sip settings.
	// +optional
	SIP SIPSpec `json:"sip,omitempty"`

	// rtp settings.
	// +optional
	RTP *RTPSpec `json:"rtp,omitempty"`

	// externalAddress settings.
	// +optional
	ExternalAddress ExternalAddressSpec `json:"externalAddress,omitempty"`

	// config to mount.
	// +optional
	Config ConfigSpec `json:"config,omitempty"`

	// drain settings.
	// +optional
	Drain DrainSpec `json:"drain,omitempty"`

	// liveness turns on hang detection. Unset means no liveness probe; {} gives the lax defaults.
	// +optional
	Liveness *LivenessSpec `json:"liveness,omitempty"`

	// dispatcher, when set, makes the operator maintain a Kamailio dispatcher list for this fleet.
	// +optional
	Dispatcher *DispatcherSpec `json:"dispatcher,omitempty"`

	// metrics, when set, runs a Prometheus exporter beside FreeSWITCH in each pod.
	// +optional
	Metrics *FreeswitchMetricsSpec `json:"metrics,omitempty"`
	// failover configures health monitoring of each member and failover to a spare node. Unset means mode Detect
	// with the defaults: health is reported, and only pods annotated sip.instruments.com/fail-over are failed over.
	// +optional
	Failover *FailoverSpec `json:"failover,omitempty"`

	// recovery, when set, recovers the calls tracked under a pod's address each time FreeSWITCH starts there. It
	// needs call tracking in the configuration and a required external address. Unset, nothing recovers calls.
	// +optional
	Recovery *RecoverySpec `json:"recovery,omitempty"`
}

// Member is one FreeSWITCH pod as the operator sees it.
type Member struct {
	// pod name.
	Pod string `json:"pod"`
	// node the pod runs on.
	// +optional
	Node string `json:"node,omitempty"`
	// address Kamailio dispatches to: the node's private address, since the pod uses the host network.
	// +optional
	Address string `json:"address,omitempty"`
	// externalAddress the pod advertises to peers.
	// +optional
	ExternalAddress string `json:"externalAddress,omitempty"`
	// state of the member.
	State MemberState `json:"state"`
	// revision of the pod template the pod runs.
	// +optional
	Revision string `json:"revision,omitempty"`
}

// FreeswitchMember is one FreeSWITCH pod as the operator sees it, with its health.
type FreeswitchMember struct {
	Member `json:",inline"`
	// health is the operator's verdict on the member, from its signals.
	// +optional
	Health *MemberHealth `json:"health,omitempty"`
}

// MemberHealth is the operator's verdict on one member and the signals behind it.
type MemberHealth struct {
	// state is Healthy, Suspect or Failed.
	State HealthState `json:"state"`
	// signals failing now. A signal still within its grace period (notReadySeconds, the probe's failureThreshold)
	// isn't listed yet.
	// +listType=map
	// +listMapKey=type
	// +optional
	Signals []HealthSignal `json:"signals,omitempty"`
	// message says why the member is in its state, and what the operator will or won't do about it.
	// +optional
	Message string `json:"message,omitempty"`
}

// HealthSignal is one failing view of a member.
type HealthSignal struct {
	// type of signal: NodeLost, NotReady or SIPUnreachable.
	Type HealthSignalType `json:"type"`
	// since is when the problem the signal reports began.
	Since metav1.Time `json:"since"`
	// counted is false when the signal fails for every member of the fleet at once and so doesn't count toward
	// Failed.
	Counted bool `json:"counted"`
	// message gives the detail: the node's condition, the last probe's error.
	// +optional
	Message string `json:"message,omitempty"`
}

// FailoverRecord describes the fleet's latest failover.
type FailoverRecord struct {
	// pod failed over.
	Pod string `json:"pod"`
	// podUID of the pod that was deleted, so its replacement can be told apart.
	PodUID string `json:"podUID"`
	// node the pod ran on, which the operator fenced.
	// +optional
	Node string `json:"node,omitempty"`
	// trigger is Automatic or Manual.
	Trigger FailoverTrigger `json:"trigger"`
	// reason gives the signals or the request behind the failover.
	// +optional
	Reason string `json:"reason,omitempty"`
	// startedAt is when the operator fenced the node and deleted the pod.
	StartedAt metav1.Time `json:"startedAt"`
	// completedAt is when the replacement pod became ready, or when the fleet was scaled down past the pod.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// replacementNode is where the replacement pod runs.
	// +optional
	ReplacementNode string `json:"replacementNode,omitempty"`
}

// FreeswitchClusterStatus defines the observed state of FreeswitchCluster.
type FreeswitchClusterStatus struct {
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
	Members []FreeswitchMember `json:"members,omitempty"`

	// dispatcherHash is the content hash of the dispatcher list last written.
	// +optional
	DispatcherHash string `json:"dispatcherHash,omitempty"`

	// dispatcherReloadedHash is the content hash Kamailio was last asked to reload.
	// +optional
	DispatcherReloadedHash string `json:"dispatcherReloadedHash,omitempty"`

	// dispatcherUpdatedAt is when the dispatcher list last changed.
	// +optional
	DispatcherUpdatedAt *metav1.Time `json:"dispatcherUpdatedAt,omitempty"`

	// lastFailover is the fleet's latest failover. Another automatic failover waits until its replacement is ready
	// (however long that takes: a failover with nowhere to go must not fence node after node); a manual one doesn't.
	// +optional
	LastFailover *FailoverRecord `json:"lastFailover,omitempty"`
	// conditions: Ready (every member ready and current), Available (at least one member takes calls),
	// Progressing (a rolling update is under way), Degraded (fewer ready members than replicas) and MembersHealthy
	// (no member Suspect or Failed).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fsc
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Updated",type=integer,JSONPath=`.status.updatedReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FreeswitchCluster is a fleet of FreeSWITCH nodes running as a SIP edge.
type FreeswitchCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of FreeswitchCluster
	// +required
	Spec FreeswitchClusterSpec `json:"spec"`

	// status defines the observed state of FreeswitchCluster
	// +optional
	Status FreeswitchClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FreeswitchClusterList contains a list of FreeswitchCluster
type FreeswitchClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FreeswitchCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FreeswitchCluster{}, &FreeswitchClusterList{})
		return nil
	})
}
