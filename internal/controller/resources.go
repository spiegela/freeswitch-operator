package controller

import (
	"fmt"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

const (
	containerName = "freeswitch"
	// addressWaitContainerName is the init container that waits for a pool address.
	addressWaitContainerName = "wait-for-address"
	exporterContainerName    = "exporter"
	portMetrics              = "metrics"

	// Volume and port names both fleets' pods use.
	volumeScripts = "operator-scripts"
	volumePodInfo = "podinfo"
	volumeConfig  = "config"
	// volumeRun is an emptyDir FreeSWITCH and the exporter share, at runDir: the recovery on start writes what it
	// did there (recoveredFile), for the exporter's metrics. It lasts as long as the pod, across restarts.
	volumeRun  = "operator-run"
	runDir     = "/run/freeswitch-operator"
	portSIPUDP = "sip-udp"
	portSIPTCP = "sip-tcp"

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
	addressPool     string // the SIPAddressPool, when addressSource is AddressPool
	requireAddress  bool
	maxDrainSeconds int64
	dispatcher      *sipv1alpha1.DispatcherSpec
	dispatcherSetID int32
	dispatcherProto sipv1alpha1.DispatcherTransport
	reloadDelaySecs int32
	liveness        *sipv1alpha1.LivenessSpec          // nil: no liveness probe
	metrics         *sipv1alpha1.FreeswitchMetricsSpec // nil: no exporter; otherwise with defaults applied
	failover        failoverSettings
	recovery        bool // recover the address's calls each time FreeSWITCH starts
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
	if m := s.Metrics; m != nil {
		out.metrics = m.DeepCopy()
		if out.metrics.Port == 0 {
			out.metrics.Port = 9282
		}
		if out.metrics.EventSocket.Port == 0 {
			out.metrics.EventSocket.Port = 8021
		}
	}
	out.failover = resolveFailover(s.Failover)
	out.recovery = s.Recovery != nil
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

// recoveryHook runs the recover script after each start of FreeSWITCH, or is nil when recovery is off.
func recoveryHook(on bool) *corev1.LifecycleHandler {
	if !on {
		return nil
	}
	return &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{shell, scriptsDir + "/" + recoverScriptName}}}
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
		sipv1alpha1.AppNameLabel:     "freeswitch",
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
			recoverScriptName: recoverScript,
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
				{Name: portSIPUDP, Protocol: corev1.ProtocolUDP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
				{Name: portSIPTCP, Protocol: corev1.ProtocolTCP, Port: cfg.sipPort, TargetPort: intstr.FromInt32(cfg.sipPort)},
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
	}, livenessEnv(cfg.liveness), bindAddressEnv(cfg.addressSource), fsc.Spec.Env)

	podInfo := podInfoItems(cfg.addressSource)
	if cfg.recovery {
		// The drain keeps a member's calls when a person fails it over (drainScript).
		podInfo = append(podInfo, corev1.DownwardAPIVolumeFile{Path: "fail-over",
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: fmt.Sprintf("metadata.annotations['%s']", sipv1alpha1.FailOverAnnotation)}})
	}
	volumes := []corev1.Volume{
		{
			Name: volumeScripts,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: scriptsConfigMapName(fsc)},
				DefaultMode:          ptr.To[int32](0o755),
			}},
		},
		{
			// Downward-API files update in place when the annotation changes; environment variables would not.
			Name:         volumePodInfo,
			VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: podInfo}},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: volumeScripts, MountPath: scriptsDir, ReadOnly: true},
		{Name: volumePodInfo, MountPath: podInfoDir, ReadOnly: true},
	}
	var templateAnnotations map[string]string
	if len(conf.sources) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name:         volumeConfig,
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: conf.sources}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: volumeConfig, MountPath: conf.mountPath, ReadOnly: true})
		templateAnnotations = map[string]string{configHashAnnotation: conf.hash}
	}
	if cfg.recovery {
		volumes = append(volumes, corev1.Volume{Name: volumeRun, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		mounts = append(mounts, corev1.VolumeMount{Name: volumeRun, MountPath: runDir})
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
			{Name: portSIPUDP, Protocol: corev1.ProtocolUDP, ContainerPort: cfg.sipPort, HostPort: cfg.sipPort},
			{Name: portSIPTCP, Protocol: corev1.ProtocolTCP, ContainerPort: cfg.sipPort, HostPort: cfg.sipPort},
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
			PostStart: recoveryHook(cfg.recovery),
			PreStop:   &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{shell, scriptsDir + "/" + drainScriptName}}},
		},
	}

	podSpec := corev1.PodSpec{
		HostNetwork:                   true,
		DNSPolicy:                     corev1.DNSClusterFirstWithHostNet,
		TerminationGracePeriodSeconds: ptr.To(cfg.maxDrainSeconds + drainGraceMargin),
		NodeSelector:                  fsc.Spec.NodeSelector,
		Tolerations:                   fsc.Spec.Tolerations,
		ImagePullSecrets:              fsc.Spec.ImagePullSecrets,
		Affinity: &corev1.Affinity{
			NodeAffinity: avoidFencedNodes(),
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: selectorLabels(fsc)},
					TopologyKey:   corev1.LabelHostname,
				}},
			},
		},
		Containers:     []corev1.Container{container},
		InitContainers: slices.Concat(addressWaitContainers(fsc, cfg), exporterContainers(cfg.metrics, cfg.recovery)),
		Volumes:        volumes,
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

// podInfoItems are the downward-API files a fleet's pods read their addresses from. Only a fleet that draws from a
// pool gets bind-address, so other fleets' pod templates stay as they were.
func podInfoItems(source sipv1alpha1.ExternalAddressSource) []corev1.DownwardAPIVolumeFile {
	annotation := func(key string) *corev1.ObjectFieldSelector {
		return &corev1.ObjectFieldSelector{FieldPath: fmt.Sprintf("metadata.annotations['%s']", key)}
	}
	items := []corev1.DownwardAPIVolumeFile{{Path: "external-address", FieldRef: annotation(sipv1alpha1.ExternalAddressAnnotation)}}
	if source == sipv1alpha1.ExternalAddressAddressPool {
		items = append(items, corev1.DownwardAPIVolumeFile{Path: "bind-address", FieldRef: annotation(sipv1alpha1.BindAddressAnnotation)})
	}
	return items
}

// bindAddressEnv points FreeSWITCH's config at the bind-address file, for a fleet that draws from a pool.
func bindAddressEnv(source sipv1alpha1.ExternalAddressSource) []corev1.EnvVar {
	if source != sipv1alpha1.ExternalAddressAddressPool {
		return nil
	}
	return []corev1.EnvVar{{Name: "FS_BIND_ADDRESS_FILE", Value: bindAddrFile}}
}

// addressWaitContainers holds FreeSWITCH back until the pod knows its external address, for a fleet that requires
// one (from its node or from a SIPAddressPool). FreeSWITCH reads the address once, at start, and the operator writes
// it only after the pod is on its node, a pool only once it has bound one there, which can take seconds (an Elastic
// IP association); started sooner, FreeSWITCH would advertise the wrong address, and a config that names its
// profiles for the address (sipbench's, for call recovery) would run under names other nodes share, for its whole
// life. The wait has no limit: a pool with no address left, or a node with no ExternalIP, keeps the pod here,
// unready, and reports Degraded. Nil for a fleet that doesn't require an address, or takes none.
func addressWaitContainers(fsc *sipv1alpha1.FreeswitchCluster, cfg settings) []corev1.Container {
	if cfg.addressSource == sipv1alpha1.ExternalAddressNone || !cfg.requireAddress {
		return nil
	}
	return []corev1.Container{{
		Name:            addressWaitContainerName,
		Image:           fsc.Spec.Image,
		ImagePullPolicy: fsc.Spec.ImagePullPolicy,
		Command:         []string{shell, "-c", addressWaitScript},
		Env:             []corev1.EnvVar{{Name: "FS_EXTERNAL_ADDRESS_FILE", Value: externalAddrFile}},
		VolumeMounts:    []corev1.VolumeMount{{Name: volumePodInfo, MountPath: podInfoDir, ReadOnly: true}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("8Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To[int64](65532),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}}
}

// exporterContainers is the metrics exporter as a native sidecar (an init container that keeps running): it starts
// before FreeSWITCH and the kubelet stops it only after FreeSWITCH has exited, so it reports through the whole
// drain. With recovery on start it reads what the recovery did (recoveredFile). Nil when spec.metrics is unset.
func exporterContainers(m *sipv1alpha1.FreeswitchMetricsSpec, recovery bool) []corev1.Container {
	if m == nil {
		return nil
	}
	env := []corev1.EnvVar{
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "METRICS_PORT", Value: strconv.Itoa(int(m.Port))},
		{Name: "ESL_PORT", Value: strconv.Itoa(int(m.EventSocket.Port))},
	}
	if ref := m.EventSocket.PasswordSecretRef; ref != nil {
		env = append(env, corev1.EnvVar{Name: "ESL_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: ref}})
	}
	var mounts []corev1.VolumeMount
	if recovery {
		env = append(env, corev1.EnvVar{Name: "RECOVERED_FILE", Value: recoveredFile})
		mounts = []corev1.VolumeMount{{Name: volumeRun, MountPath: runDir, ReadOnly: true}}
	}
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	if m.Resources != nil {
		resources = *m.Resources
	}
	return []corev1.Container{{
		Name:            exporterContainerName,
		Image:           m.ExporterImage,
		ImagePullPolicy: m.ExporterImagePullPolicy,
		RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways),
		Env:             env,
		VolumeMounts:    mounts,
		Resources:       resources,
		// The host network makes this a port on the node; declaring it lets a scrape config find it by name.
		Ports: []corev1.ContainerPort{{Name: portMetrics, Protocol: corev1.ProtocolTCP, ContainerPort: m.Port, HostPort: m.Port}},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To[int64](65532),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}}
}
