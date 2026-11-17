package controller

import (
	"fmt"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

const (
	containerName = "freeswitch"

	// drainGraceMargin is added to maxDrainSeconds for the pod's termination grace period, so the preStop drain
	// always finishes before the kubelet's SIGKILL.
	drainGraceMargin int64 = 30

	// startupGraceSeconds is how long a starting FreeSWITCH has to come up with its SIP profile before the
	// kubelet restarts it.
	startupGraceSeconds int32 = 120
	startupProbePeriod  int32 = 5
)

// settings is a FreeswitchClusterSpec with every default applied. The API server applies the same defaults from
// the CRD schema; applying them here too keeps the builders correct for objects that skipped it (unit tests,
// older stored objects).
type settings struct {
	replicas        int32
	sipPort         int32
	rtpStart        int32
	rtpEnd          int32
	addressSource   sipv1alpha1.ExternalAddressSource
	requireAddress  bool
	maxDrainSeconds int64
	dispatcher      *sipv1alpha1.DispatcherSpec
	dispatcherSetID int32
	dispatcherProto sipv1alpha1.DispatcherTransport
	reloadDelaySecs int32
	liveness        *sipv1alpha1.LivenessSpec // nil: no liveness probe
}

func resolve(fsc *sipv1alpha1.FreeswitchCluster) settings {
	s := fsc.Spec
	out := settings{
		replicas:        2,
		sipPort:         5060,
		rtpStart:        16384,
		rtpEnd:          32768,
		addressSource:   sipv1alpha1.ExternalAddressNodeExternalIP,
		requireAddress:  true,
		maxDrainSeconds: 7200,
		dispatcherSetID: 1,
		dispatcherProto: sipv1alpha1.DispatcherTransportUDP,
		reloadDelaySecs: 75,
	}
	if s.Replicas != nil {
		out.replicas = *s.Replicas
	}
	if s.SIP.Port != 0 {
		out.sipPort = s.SIP.Port
	}
	if s.RTP != nil {
		out.rtpStart, out.rtpEnd = s.RTP.PortRangeStart, s.RTP.PortRangeEnd
	}
	if s.ExternalAddress.Source != "" {
		out.addressSource = s.ExternalAddress.Source
	}
	if s.ExternalAddress.Required != nil {
		out.requireAddress = *s.ExternalAddress.Required
	}
	if out.addressSource == sipv1alpha1.ExternalAddressNone {
		out.requireAddress = false
	}
	if s.Drain.MaxDrainSeconds != 0 {
		out.maxDrainSeconds = s.Drain.MaxDrainSeconds
	}
	if d := s.Dispatcher; d != nil {
		out.dispatcher = d
		if d.SetID != 0 {
			out.dispatcherSetID = d.SetID
		}
		if d.Transport != "" {
			out.dispatcherProto = d.Transport
		}
		if d.ReloadDelaySeconds != 0 {
			out.reloadDelaySecs = d.ReloadDelaySeconds
		}
	}
	if l := s.Liveness; l != nil {
		out.liveness = &sipv1alpha1.LivenessSpec{PeriodSeconds: 30, TimeoutSeconds: 15, FailureThreshold: 10}
		if l.PeriodSeconds != 0 {
			out.liveness.PeriodSeconds = l.PeriodSeconds
		}
		if l.TimeoutSeconds != 0 {
			out.liveness.TimeoutSeconds = l.TimeoutSeconds
		}
		if l.FailureThreshold != 0 {
			out.liveness.FailureThreshold = l.FailureThreshold
		}
	}
	return out
}

// livenessProbe runs the live script, or is nil when liveness is off.
func livenessProbe(l *sipv1alpha1.LivenessSpec) *corev1.Probe {
	if l == nil {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{shell, scriptsDir + "/" + liveScriptName}}},
		PeriodSeconds:    l.PeriodSeconds,
		TimeoutSeconds:   l.TimeoutSeconds,
		FailureThreshold: l.FailureThreshold,
		SuccessThreshold: 1,
	}
}

// livenessEnv tells the live script the probe's timeout, so fs_cli gives up first.
func livenessEnv(l *sipv1alpha1.LivenessSpec) []corev1.EnvVar {
	if l == nil {
		return nil
	}
	return []corev1.EnvVar{{Name: "FS_LIVENESS_TIMEOUT_SECONDS", Value: strconv.Itoa(int(l.TimeoutSeconds))}}
}

// Object names. The headless Service shares the cluster's name, so its EndpointSlices carry
// kubernetes.io/service-name=<cluster name>.
func statefulSetName(fsc *sipv1alpha1.FreeswitchCluster) string { return fsc.Name }
func serviceName(fsc *sipv1alpha1.FreeswitchCluster) string     { return fsc.Name }
func pdbName(fsc *sipv1alpha1.FreeswitchCluster) string         { return fsc.Name }
func scriptsConfigMapName(fsc *sipv1alpha1.FreeswitchCluster) string {
	return fsc.Name + "-operator-scripts"
}

// selectorLabels identify a cluster's pods. They never change after creation (StatefulSet selectors are immutable).
func selectorLabels(fsc *sipv1alpha1.FreeswitchCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "freeswitch",
		"app.kubernetes.io/instance": fsc.Name,
		sipv1alpha1.ClusterLabel:     fsc.Name,
	}
}

func commonLabels(fsc *sipv1alpha1.FreeswitchCluster) map[string]string {
	l := selectorLabels(fsc)
	l["app.kubernetes.io/managed-by"] = "freeswitch-operator"
	l["app.kubernetes.io/component"] = "sip-edge"
	return l
}

func buildScriptsConfigMap(fsc *sipv1alpha1.FreeswitchCluster) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scriptsConfigMapName(fsc),
			Namespace: fsc.Namespace,
			Labels:    commonLabels(fsc),
		},
		Data: map[string]string{
			drainScriptName:   drainScript,
			readyScriptName:   readyScript,
			startupScriptName: startupScript,
			liveScriptName:    liveScript,
		},
	}
}

func buildService(fsc *sipv1alpha1.FreeswitchCluster) *corev1.Service {
	cfg := resolve(fsc)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName(fsc),
			Namespace: fsc.Namespace,
			Labels:    commonLabels(fsc),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  selectorLabels(fsc),
			// Not-ready pods stay out of the EndpointSlices' ready set; that is the dispatcher's signal.
			PublishNotReadyAddresses: false,
			Ports: []corev1.ServicePort{
				{Name: "sip-udp", Protocol: corev1.ProtocolUDP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
				{Name: "sip-tcp", Protocol: corev1.ProtocolTCP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
			},
		},
	}
}

func buildPDB(fsc *sipv1alpha1.FreeswitchCluster) *policyv1.PodDisruptionBudget {
	maxUnavailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbName(fsc),
			Namespace: fsc.Namespace,
			Labels:    commonLabels(fsc),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: selectorLabels(fsc)},
		},
	}
}

// buildStatefulSet renders the fleet's StatefulSet, with conf (from layoutConfig) as its configuration directory.
func buildStatefulSet(fsc *sipv1alpha1.FreeswitchCluster, conf configLayout) *appsv1.StatefulSet {
	cfg := resolve(fsc)
	labels := commonLabels(fsc)

	env := slices.Concat([]corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
		{Name: "FS_SIP_PORT", Value: strconv.Itoa(int(cfg.sipPort))},
		{Name: "FS_RTP_START_PORT", Value: strconv.Itoa(int(cfg.rtpStart))},
		{Name: "FS_RTP_END_PORT", Value: strconv.Itoa(int(cfg.rtpEnd))},
		{Name: "FS_EXTERNAL_ADDRESS_FILE", Value: externalAddrFile},
		{Name: "FS_MAX_DRAIN_SECONDS", Value: strconv.FormatInt(cfg.maxDrainSeconds, 10)},
		{Name: "FS_REQUIRE_EXTERNAL_ADDRESS", Value: strconv.FormatBool(cfg.requireAddress)},
	}, livenessEnv(cfg.liveness), fsc.Spec.Env)

	volumes := []corev1.Volume{
		{
			Name: "operator-scripts",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: scriptsConfigMapName(fsc)},
				DefaultMode:          ptr.To[int32](0o755),
			}},
		},
		{
			// Downward-API files update in place when the annotation changes; environment variables would not.
			Name: "podinfo",
			VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{
				Items: []corev1.DownwardAPIVolumeFile{{
					Path:     "external-address",
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: fmt.Sprintf("metadata.annotations['%s']", sipv1alpha1.ExternalAddressAnnotation)},
				}},
			}},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: "operator-scripts", MountPath: scriptsDir, ReadOnly: true},
		{Name: "podinfo", MountPath: podInfoDir, ReadOnly: true},
	}
	var templateAnnotations map[string]string
	if len(conf.sources) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name:         "config",
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: conf.sources}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: conf.mountPath, ReadOnly: true})
		templateAnnotations = map[string]string{configHashAnnotation: conf.hash}
	}

	container := corev1.Container{
		Name:            containerName,
		Image:           fsc.Spec.Image,
		ImagePullPolicy: fsc.Spec.ImagePullPolicy,
		Command:         fsc.Spec.Command,
		Args:            fsc.Spec.Args,
		Env:             env,
		Resources:       fsc.Spec.Resources,
		// With host networking the container port is the node port. Declaring SIP makes the scheduler refuse a
		// second pod on a node where it is taken; the RTP range is too large to declare and is guarded by the
		// anti-affinity below.
		Ports: []corev1.ContainerPort{
			{Name: "sip-udp", Protocol: corev1.ProtocolUDP, ContainerPort: cfg.sipPort, HostPort: cfg.sipPort},
			{Name: "sip-tcp", Protocol: corev1.ProtocolTCP, ContainerPort: cfg.sipPort, HostPort: cfg.sipPort},
		},
		VolumeMounts: mounts,
		// Startup: FreeSWITCH up with a SIP profile on the SIP port, within startupGraceSeconds. A config that
		// leaves no profile running keeps the new pod restarting and never ready, so a roll stops at that node.
		StartupProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{shell, scriptsDir + "/" + startupScriptName}}},
			PeriodSeconds:    startupProbePeriod,
			TimeoutSeconds:   3,
			FailureThreshold: startupGraceSeconds / startupProbePeriod,
			SuccessThreshold: 1,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{shell, scriptsDir + "/" + readyScriptName}}},
			PeriodSeconds:    5,
			TimeoutSeconds:   3,
			FailureThreshold: 1,
			SuccessThreshold: 1,
		},
		// Liveness only when spec.liveness asks for it: a failure restarts the container and drops every call on
		// the node, and fs_cli can be slow under load, so a false positive at peak would do more harm than the
		// hang it guards against. A crash needs no probe: the process exits and the container restarts.
		LivenessProbe: livenessProbe(cfg.liveness),
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{shell, scriptsDir + "/" + drainScriptName}}},
		},
	}

	podSpec := corev1.PodSpec{
		HostNetwork:                   true,
		DNSPolicy:                     corev1.DNSClusterFirstWithHostNet,
		TerminationGracePeriodSeconds: ptr.To(cfg.maxDrainSeconds + drainGraceMargin),
		NodeSelector:                  fsc.Spec.NodeSelector,
		Tolerations:                   fsc.Spec.Tolerations,
		ImagePullSecrets:              fsc.Spec.ImagePullSecrets,
		Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: selectorLabels(fsc)},
				TopologyKey:   corev1.LabelHostname,
			}},
		}},
		Containers: []corev1.Container{container},
		Volumes:    volumes,
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName(fsc),
			Namespace: fsc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To(cfg.replicas),
			ServiceName:         serviceName(fsc),
			Selector:            &metav1.LabelSelector{MatchLabels: selectorLabels(fsc)},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			// The operator replaces pods itself, one at a time, only when the rest of the fleet is ready.
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: templateAnnotations},
				Spec:       podSpec,
			},
		},
	}
}
