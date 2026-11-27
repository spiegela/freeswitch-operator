package controller

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

const (
	kamailioContainerName = "kamailio"
	// kamailioTokenKey is the key of the JSON-RPC token in the <name>-jsonrpc Secret.
	kamailioTokenKey = "token"
	// kamailioDrainGraceMargin is added to maxDrainSeconds for the termination grace period.
	kamailioDrainGraceMargin int64 = 10
	// kamailioProbingFlag marks every destination for probing (ds_probing_mode 3).
	kamailioProbingFlag = "8"
)

// kamailioSettings is a KamailioClusterSpec with every default applied (see settings).
type kamailioSettings struct {
	replicas        int32
	sipPort         int32
	rpcPort         int32
	metricsPort     int32
	addressSource   sipv1alpha1.ExternalAddressSource
	addressPool     string // the SIPAddressPool, when addressSource is AddressPool
	requireAddress  bool
	maxDrainSeconds int64
	reloadDelaySecs int32
}

func resolveKamailio(kc *sipv1alpha1.KamailioCluster) kamailioSettings {
	s := kc.Spec
	out := kamailioSettings{
		replicas:        2,
		sipPort:         5060,
		rpcPort:         5070,
		metricsPort:     5071,
		addressSource:   sipv1alpha1.ExternalAddressNodeExternalIP,
		requireAddress:  true,
		maxDrainSeconds: 40,
		reloadDelaySecs: 75,
	}
	if s.Replicas != nil {
		out.replicas = *s.Replicas
	}
	if s.SIP.Port != 0 {
		out.sipPort = s.SIP.Port
	}
	if s.RPC.Port != 0 {
		out.rpcPort = s.RPC.Port
	}
	if s.Metrics.Port != 0 {
		out.metricsPort = s.Metrics.Port
	}
	if s.ExternalAddress.Source != "" {
		out.addressSource = s.ExternalAddress.Source
	}
	out.addressPool = addressPoolName(s.ExternalAddress)
	if s.ExternalAddress.Required != nil {
		out.requireAddress = *s.ExternalAddress.Required
	}
	if out.addressSource == sipv1alpha1.ExternalAddressNone {
		out.requireAddress = false
	}
	if s.Drain.MaxDrainSeconds != 0 {
		out.maxDrainSeconds = s.Drain.MaxDrainSeconds
	}
	if s.ReloadDelaySeconds != 0 {
		out.reloadDelaySecs = s.ReloadDelaySeconds
	}
	return out
}

// Object names. The pods' objects carry a -kamailio suffix so a KamailioCluster and a FreeswitchCluster can share a
// name without their children colliding.
func kamailioName(kc *sipv1alpha1.KamailioCluster) string { return kc.Name + "-kamailio" }

// kamailioRouteName is the Service under which the operator keeps a DNS name for each Kamailio pod, the one it
// advertises to FreeSWITCH (PRIVATE_NAME): <pod>.<this>.<namespace>.svc.<cluster domain>.
func kamailioRouteName(kc *sipv1alpha1.KamailioCluster) string { return kc.Name + "-kamailio-route" }

func kamailioScriptsName(kc *sipv1alpha1.KamailioCluster) string {
	return kc.Name + "-kamailio-scripts"
}
func kamailioDispatcherName(kc *sipv1alpha1.KamailioCluster) string {
	return kc.Name + "-kamailio-dispatcher"
}
func kamailioTokenName(kc *sipv1alpha1.KamailioCluster) string { return kc.Name + "-jsonrpc" }

// kamailioConfigName is the snapshot of a rendered kamailio.cfg with this hash.
func kamailioConfigName(kc *sipv1alpha1.KamailioCluster, hash string) string {
	prefix := kc.Name
	if len(prefix) > 200 {
		prefix = prefix[:200]
	}
	return prefix + "-kamailio-cfg-" + hash
}

func kamailioSelectorLabels(kc *sipv1alpha1.KamailioCluster) map[string]string {
	return map[string]string{
		sipv1alpha1.AppNameLabel:         "kamailio",
		"app.kubernetes.io/instance":     kc.Name,
		sipv1alpha1.KamailioClusterLabel: kc.Name,
	}
}

func kamailioLabels(kc *sipv1alpha1.KamailioCluster) map[string]string {
	l := kamailioSelectorLabels(kc)
	l["app.kubernetes.io/managed-by"] = "freeswitch-operator"
	l["app.kubernetes.io/component"] = "sip-proxy"
	return l
}

func buildKamailioScriptsConfigMap(kc *sipv1alpha1.KamailioCluster) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: kamailioScriptsName(kc), Namespace: kc.Namespace, Labels: kamailioLabels(kc)},
		Data: map[string]string{
			kamailioStartScriptName: kamailioStartScript,
			kamailioReadyScriptName: kamailioReadyScript,
			kamailioDrainScriptName: kamailioDrainScript,
		},
	}
}

func buildKamailioService(kc *sipv1alpha1.KamailioCluster) *corev1.Service {
	cfg := resolveKamailio(kc)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: kamailioName(kc), Namespace: kc.Namespace, Labels: kamailioLabels(kc)},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  kamailioSelectorLabels(kc),
			Ports: []corev1.ServicePort{
				{Name: portSIPUDP, Protocol: corev1.ProtocolUDP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
				{Name: portSIPTCP, Protocol: corev1.ProtocolTCP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
				{Name: "rpc", Protocol: corev1.ProtocolTCP, Port: cfg.rpcPort, TargetPort: intstr.FromInt32(cfg.rpcPort)},
				{Name: "metrics", Protocol: corev1.ProtocolTCP, Port: cfg.metricsPort, TargetPort: intstr.FromInt32(cfg.metricsPort)},
			},
		},
	}
}

// buildKamailioRouteService is the headless Service, with no selector, that holds the pods' route names: the
// operator writes its EndpointSlice itself (buildKamailioRouteSlice).
func buildKamailioRouteService(kc *sipv1alpha1.KamailioCluster) *corev1.Service {
	cfg := resolveKamailio(kc)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: kamailioRouteName(kc), Namespace: kc.Namespace, Labels: kamailioLabels(kc)},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{
				{Name: portSIPUDP, Protocol: corev1.ProtocolUDP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
				{Name: portSIPTCP, Protocol: corev1.ProtocolTCP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
			},
		},
	}
}

// kamailioRouteManager marks the route EndpointSlice as the operator's, so Kubernetes' own controllers leave it be.
const kamailioRouteManager = "freeswitch-operator"

// buildKamailioRouteSlice names each of the fleet's pods, <pod>.<route Service>, at the address it runs on. Unlike
// the names a StatefulSet's own Service gives, these outlast the pod: while a pod has no address (deleted, or a
// replacement not running yet) its name keeps the address it had in prev. So FreeSWITCH, whose in-dialog requests
// go to the name, still finds a Kamailio restarting in place where it was, and retransmits until it answers,
// rather than failing at once on a name that doesn't resolve; and finds a replacement on another node as soon as
// that has its address. A pod beyond the fleet's replicas loses its name. The slice holds one address family: the
// one prev has, else the first pod's.
func buildKamailioRouteSlice(kc *sipv1alpha1.KamailioCluster, pods []corev1.Pod, prev *discoveryv1.EndpointSlice) *discoveryv1.EndpointSlice {
	cfg := resolveKamailio(kc)
	family := discoveryv1.AddressType("")
	if prev != nil {
		family = prev.AddressType
	}
	familyOf := func(ip string) discoveryv1.AddressType {
		a, err := netip.ParseAddr(ip)
		switch {
		case err != nil:
			return ""
		case a.Is4():
			return discoveryv1.AddressTypeIPv4
		default:
			return discoveryv1.AddressTypeIPv6
		}
	}
	current := map[string]string{}
	for _, p := range pods {
		if f := familyOf(p.Status.PodIP); f != "" {
			if family == "" {
				family = f
			}
			if f == family {
				current[p.Name] = p.Status.PodIP
			}
		}
	}
	if family == "" {
		family = discoveryv1.AddressTypeIPv4
	}
	held := map[string][]string{}
	if prev != nil {
		for _, ep := range prev.Endpoints {
			if ep.Hostname != nil {
				held[*ep.Hostname] = ep.Addresses
			}
		}
	}
	labels := kamailioLabels(kc)
	labels[discoveryv1.LabelServiceName] = kamailioRouteName(kc)
	labels[discoveryv1.LabelManagedBy] = kamailioRouteManager
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: kamailioRouteName(kc), Namespace: kc.Namespace, Labels: labels},
		AddressType: family,
		Ports: []discoveryv1.EndpointPort{
			{Name: ptr.To(portSIPUDP), Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(cfg.sipPort)},
			{Name: ptr.To(portSIPTCP), Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(cfg.sipPort)},
		},
	}
	for i := range cfg.replicas {
		name := fmt.Sprintf("%s-%d", kamailioName(kc), i)
		addrs := held[name]
		if ip, ok := current[name]; ok {
			addrs = []string{ip}
		}
		if len(addrs) == 0 {
			continue
		}
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{
			Addresses:  addrs,
			Hostname:   ptr.To(name),
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		})
	}
	return slice
}

// buildKamailioStatefulSet renders the proxy fleet. configName is the snapshot holding the rendered kamailio.cfg,
// and configHash its hash: a new config is a new template, which the rollout loop works through one pod at a time.
func buildKamailioStatefulSet(kc *sipv1alpha1.KamailioCluster, configName, configHash string) *appsv1.StatefulSet {
	cfg := resolveKamailio(kc)
	labels := kamailioLabels(kc)
	field := func(path string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}
	}
	env := slices.Concat([]corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: field("metadata.name")},
		{Name: "POD_NAMESPACE", ValueFrom: field("metadata.namespace")},
		// The Service holding the pod's route name: <pod>.<service>.<namespace>.svc.<cluster domain>.
		{Name: "KAMAILIO_SERVICE", Value: kamailioRouteName(kc)},
		{Name: "NODE_NAME", ValueFrom: field("spec.nodeName")},
		{Name: "HOST_IP", ValueFrom: field("status.hostIP")},
		{Name: "POD_IP", ValueFrom: field("status.podIP")},
		{Name: "KAMAILIO_SIP_PORT", Value: strconv.Itoa(int(cfg.sipPort))},
		{Name: "KAMAILIO_RPC_PORT", Value: strconv.Itoa(int(cfg.rpcPort))},
		{Name: "KAMAILIO_RPC_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: kamailioTokenName(kc)},
			Key:                  kamailioTokenKey,
		}}},
		{Name: "KAMAILIO_MAX_DRAIN_SECONDS", Value: strconv.FormatInt(cfg.maxDrainSeconds, 10)},
		{Name: "KAMAILIO_REQUIRE_EXTERNAL_ADDRESS", Value: strconv.FormatBool(cfg.requireAddress)},
	}, kc.Spec.Env)

	volumes := []corev1.Volume{
		{Name: volumeScripts, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: kamailioScriptsName(kc)},
			DefaultMode:          ptr.To[int32](0o755),
		}}},
		{Name: volumePodInfo, VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: podInfoItems(cfg.addressSource)}}},
		{Name: volumeConfig, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configName},
		}}},
		// A directory mount, so the kubelet updates the list in place; never subPath. Optional, since the operator
		// writes it in the same reconcile.
		{Name: "dispatcher", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: kamailioDispatcherName(kc)},
			Optional:             ptr.To(true),
		}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: volumeScripts, MountPath: scriptsDir, ReadOnly: true},
		{Name: volumePodInfo, MountPath: podInfoDir, ReadOnly: true},
		{Name: volumeConfig, MountPath: kamailioConfigDir, ReadOnly: true},
		{Name: "dispatcher", MountPath: kamailioDispatcherDir, ReadOnly: true},
	}
	script := func(name string) []string { return []string{shell, scriptsDir + "/" + name} }

	container := corev1.Container{
		Name:            kamailioContainerName,
		Image:           kc.Spec.Image,
		ImagePullPolicy: kc.Spec.ImagePullPolicy,
		Command:         script(kamailioStartScriptName),
		Env:             env,
		Resources:       kc.Spec.Resources,
		// Host networking: the container ports are the node's. Declaring them keeps a second pod off the node.
		Ports: []corev1.ContainerPort{
			{Name: portSIPUDP, Protocol: corev1.ProtocolUDP, ContainerPort: cfg.sipPort, HostPort: cfg.sipPort},
			{Name: portSIPTCP, Protocol: corev1.ProtocolTCP, ContainerPort: cfg.sipPort, HostPort: cfg.sipPort},
			{Name: "rpc", Protocol: corev1.ProtocolTCP, ContainerPort: cfg.rpcPort, HostPort: cfg.rpcPort},
			{Name: "metrics", Protocol: corev1.ProtocolTCP, ContainerPort: cfg.metricsPort, HostPort: cfg.metricsPort},
		},
		VolumeMounts: mounts,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: script(kamailioReadyScriptName)}},
			PeriodSeconds:    5,
			TimeoutSeconds:   3,
			FailureThreshold: 1,
			SuccessThreshold: 1,
		},
		// No liveness probe, as for FreeSWITCH: a crash exits the process, and a restart drops what is in flight.
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: script(kamailioDrainScriptName)}},
		},
	}

	podSpec := corev1.PodSpec{
		HostNetwork: true,
		DNSPolicy:   corev1.DNSClusterFirstWithHostNet,
		// Keeps Kamailio from being PID 1. As PID 1 it inherits every orphaned process in the container (from a
		// kubectl exec, say), and when one exits Kamailio takes it for a child of its own and shuts down. With a
		// shared namespace the pod's pause process is PID 1 and reaps them.
		ShareProcessNamespace:         ptr.To(true),
		TerminationGracePeriodSeconds: ptr.To(cfg.maxDrainSeconds + kamailioDrainGraceMargin),
		NodeSelector:                  kc.Spec.NodeSelector,
		Tolerations:                   kc.Spec.Tolerations,
		ImagePullSecrets:              kc.Spec.ImagePullSecrets,
		Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: kamailioSelectorLabels(kc)},
				TopologyKey:   corev1.LabelHostname,
			}},
		}},
		Containers: []corev1.Container{container},
		Volumes:    volumes,
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: kamailioName(kc), Namespace: kc.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To(cfg.replicas),
			ServiceName:         kamailioName(kc),
			Selector:            &metav1.LabelSelector{MatchLabels: kamailioSelectorLabels(kc)},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{configHashAnnotation: configHash}},
				Spec:       podSpec,
			},
		},
	}
}
