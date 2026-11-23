// Package fleet lets scenarios act on the FreeSWITCH fleet under test, a FreeswitchCluster on Kubernetes: drain
// the node a call is on or take it off the network, roll the fleet, break and restore its config, and watch its
// status. It talks to the API server with client-go and doesn't import the operator, so the bench stays its own
// module.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

// RollEnv is the environment variable Roll sets on the fleet's pods.
const RollEnv = "SIPBENCH_ROLL"

// fieldManager is who sipbench's server-side writes are recorded as.
const fieldManager = "sipbench"

// ClusterLabel names the FreeswitchCluster a pod belongs to. The operator's own label; the bench doesn't import it.
const ClusterLabel = "sip.instruments.com/cluster"

// ExternalAddressAnnotation is where the operator records the address a FreeSWITCH pod advertises to far ends
// (its SIPAddressPool's binding, or the node's address). The operator's own constant; the bench doesn't import it.
const ExternalAddressAnnotation = "sip.instruments.com/external-address"

// Member is one FreeSWITCH pod, as the FreeswitchCluster's status lists it.
type Member struct {
	Pod      string `json:"pod"`
	Node     string `json:"node,omitempty"`
	Address  string `json:"address,omitempty"`
	State    string `json:"state"`
	Revision string `json:"revision,omitempty"`
}

// Status is what the scenarios need of a FreeswitchCluster.
type Status struct {
	Generation         int64
	ObservedGeneration int64
	// Want is spec.replicas.
	Want            int32
	Ready           int32
	Updated         int32
	CurrentRevision string
	UpdateRevision  string
	Members         []Member
}

// Current reports whether every pod runs the latest template and takes calls.
func (s Status) Current() bool {
	if s.ObservedGeneration != s.Generation || int32(len(s.Members)) != s.Want {
		return false
	}
	for _, m := range s.Members {
		if m.State != "Active" || m.Revision != s.UpdateRevision {
			return false
		}
	}
	return s.Ready == s.Want && s.Updated == s.Want
}

// MemberAt returns the member whose address is host.
func (s Status) MemberAt(host string) (Member, bool) {
	for _, m := range s.Members {
		if m.Address == host {
			return m, true
		}
	}
	return Member{}, false
}

// Member returns the member named pod.
func (s Status) Member(pod string) (Member, bool) {
	for _, m := range s.Members {
		if m.Pod == pod {
			return m, true
		}
	}
	return Member{}, false
}

// Fleet is the FreeSWITCH fleet a scenario acts on.
type Fleet interface {
	// Name is namespace/name.
	Name() string
	Status(ctx context.Context) (Status, error)
	// Cordon marks a node unschedulable, or schedulable again.
	Cordon(ctx context.Context, node string, unschedulable bool) error
	// Evict evicts a pod through the eviction API, so its PodDisruptionBudget applies, retrying while the
	// budget refuses.
	Evict(ctx context.Context, pod string) error
	// Roll sets RollEnv to value on the fleet's pod template.
	Roll(ctx context.Context, value string) error
	ConfigMapKey(ctx context.Context, name, key string) (string, error)
	SetConfigMapKey(ctx context.Context, name, key, value string) error
	// ExternalAddresses returns, by pod, each member's ExternalAddressAnnotation ("" for a pod without one). A
	// member whose pod is already gone is left out.
	ExternalAddresses(ctx context.Context) (map[string]string, error)
	// NodeReady reports whether a node's Ready condition is True.
	NodeReady(ctx context.Context, node string) (bool, error)
	// StartBlackout starts a pod on node that takes the node off the network for d, BlackoutDelay after it
	// starts (sipbench blackout, from image), and returns the pod's name.
	StartBlackout(ctx context.Context, node string, d time.Duration, image string) (string, error)
	// Blackout reports on a pod StartBlackout started.
	Blackout(ctx context.Context, pod string) (BlackoutState, error)
	// PodLog returns a pod's log.
	PodLog(ctx context.Context, pod string) (string, error)
	// DeletePod deletes a pod; one already gone is no error.
	DeletePod(ctx context.Context, pod string) error
}

// BlackoutDelay is how long a blackout pod waits after starting before it takes its node dark, so the kubelet
// can report it running while the node can still reach the API server.
const BlackoutDelay = 3 * time.Second

// BlackoutState is what the runner sees of a blackout pod.
type BlackoutState struct {
	// Phase is the pod's: Pending, Running, Succeeded or Failed.
	Phase string
	// StartedAt is when its container started (to the second); zero before then.
	StartedAt time.Time
	// Reason explains a pod that is pending or failed, when Kubernetes gives one.
	Reason string
}

// Finished reports whether the pod has stopped: a blackout that has ended, and been seen to end, which needs the
// node back on the network.
func (b BlackoutState) Finished() bool {
	return b.Phase == string(corev1.PodSucceeded) || b.Phase == string(corev1.PodFailed)
}

var fscResource = schema.GroupVersionResource{Group: "sip.instruments.com", Version: "v1alpha1", Resource: "freeswitchclusters"}

type kube struct {
	ns, name string
	core     kubernetes.Interface
	dyn      dynamic.Interface
}

// Connect returns the fleet ref names (namespace/name). It uses the pod's service account inside a cluster, and
// the kubeconfig (KUBECONFIG, ~/.kube/config) outside one.
func Connect(ref string) (Fleet, error) {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || name == "" {
		return nil, fmt.Errorf("fleet %q: want namespace/name", ref)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("fleet %s: no cluster to talk to: %w", ref, err)
		}
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &kube{ns: ns, name: name, core: core, dyn: dyn}, nil
}

func (k *kube) Name() string { return k.ns + "/" + k.name }

func (k *kube) Status(ctx context.Context) (Status, error) {
	u, err := k.dyn.Resource(fscResource).Namespace(k.ns).Get(ctx, k.name, metav1.GetOptions{})
	if err != nil {
		return Status{}, err
	}
	b, err := json.Marshal(u.Object)
	if err != nil {
		return Status{}, err
	}
	var obj struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64    `json:"observedGeneration"`
			ReadyReplicas      int32    `json:"readyReplicas"`
			UpdatedReplicas    int32    `json:"updatedReplicas"`
			CurrentRevision    string   `json:"currentRevision"`
			UpdateRevision     string   `json:"updateRevision"`
			Members            []Member `json:"members"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return Status{}, err
	}
	s := Status{
		Generation:         obj.Metadata.Generation,
		ObservedGeneration: obj.Status.ObservedGeneration,
		Want:               1,
		Ready:              obj.Status.ReadyReplicas,
		Updated:            obj.Status.UpdatedReplicas,
		CurrentRevision:    obj.Status.CurrentRevision,
		UpdateRevision:     obj.Status.UpdateRevision,
		Members:            obj.Status.Members,
	}
	if obj.Spec.Replicas != nil {
		s.Want = *obj.Spec.Replicas
	}
	return s, nil
}

func (k *kube) Cordon(ctx context.Context, node string, unschedulable bool) error {
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable)
	_, err := k.core.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func (k *kube) Evict(ctx context.Context, pod string) error {
	ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: k.ns}}
	for {
		err := k.core.CoreV1().Pods(k.ns).EvictV1(ctx, ev)
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		if !apierrors.IsTooManyRequests(err) {
			return err
		}
		// The disruption budget refuses for now; kubectl drain retries the same way.
		select {
		case <-ctx.Done():
			return fmt.Errorf("evicting %s: %w (last: %v)", pod, ctx.Err(), err)
		case <-time.After(2 * time.Second):
		}
	}
}

func (k *kube) Roll(ctx context.Context, value string) error {
	res := k.dyn.Resource(fscResource).Namespace(k.ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		u, err := res.Get(ctx, k.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		spec, _ := u.Object["spec"].(map[string]any)
		if spec == nil {
			return fmt.Errorf("%s has no spec", k.Name())
		}
		env, _ := spec["env"].([]any)
		set := false
		for _, e := range env {
			if m, ok := e.(map[string]any); ok && m["name"] == RollEnv {
				m["value"] = value
				delete(m, "valueFrom")
				set = true
			}
		}
		if !set {
			env = append(env, map[string]any{"name": RollEnv, "value": value})
		}
		spec["env"] = env
		_, err = res.Update(ctx, u, metav1.UpdateOptions{FieldManager: fieldManager})
		return err
	})
}

func (k *kube) ConfigMapKey(ctx context.Context, name, key string) (string, error) {
	cm, err := k.core.CoreV1().ConfigMaps(k.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	v, ok := cm.Data[key]
	if !ok {
		return "", fmt.Errorf("ConfigMap %s has no key %q", name, key)
	}
	return v, nil
}

func (k *kube) ExternalAddresses(ctx context.Context) (map[string]string, error) {
	st, err := k.Status(ctx)
	if err != nil {
		return nil, err
	}
	pods, err := k.core.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: ClusterLabel + "=" + k.name})
	if err != nil {
		return nil, err
	}
	byName := map[string]string{}
	for _, p := range pods.Items {
		byName[p.Name] = p.Annotations[ExternalAddressAnnotation]
	}
	out := map[string]string{}
	for _, m := range st.Members {
		if addr, ok := byName[m.Pod]; ok { // a member missing has been replaced since the status was written
			out[m.Pod] = addr
		}
	}
	return out, nil
}

func (k *kube) SetConfigMapKey(ctx context.Context, name, key, value string) error {
	cms := k.core.CoreV1().ConfigMaps(k.ns)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := cms.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[key] = value
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{FieldManager: fieldManager})
		return err
	})
}

func (k *kube) NodeReady(ctx context.Context, node string) (bool, error) {
	n, err := k.core.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

// StartBlackout runs sipbench blackout on node: on the host network, as root with NET_ADMIN (tc on the node's
// own interface), placed by nodeName and tolerating every taint, so neither the scheduler nor a NotReady or
// unreachable taint keeps it off the node or evicts it while the node is dark.
func (k *kube) StartBlackout(ctx context.Context, node string, d time.Duration, image string) (string, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sipbench-blackout-",
			Namespace:    k.ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":    "sipbench-blackout",
				"app.kubernetes.io/part-of": "sipbench",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                      node,
			HostNetwork:                   true,
			RestartPolicy:                 corev1.RestartPolicyNever,
			Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			TerminationGracePeriodSeconds: ptr.To[int64](30),
			AutomountServiceAccountToken:  ptr.To(false),
			Containers: []corev1.Container{{
				Name:            "blackout",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"blackout", "-for", d.String(), "-delay", BlackoutDelay.String()},
				Env: []corev1.EnvVar{{
					Name:      "SIPBENCH_BLACKOUT_ADDRESS",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}},
				}},
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:    ptr.To[int64](0),
					RunAsNonRoot: ptr.To(false),
					Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
				},
			}},
		},
	}
	created, err := k.core.CoreV1().Pods(k.ns).Create(ctx, pod, metav1.CreateOptions{FieldManager: fieldManager})
	if err != nil {
		return "", err
	}
	return created.Name, nil
}

func (k *kube) Blackout(ctx context.Context, pod string) (BlackoutState, error) {
	p, err := k.core.CoreV1().Pods(k.ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return BlackoutState{}, err
	}
	st := BlackoutState{Phase: string(p.Status.Phase), Reason: p.Status.Reason}
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.State.Running != nil:
			st.StartedAt = cs.State.Running.StartedAt.Time
		case cs.State.Terminated != nil:
			st.StartedAt = cs.State.Terminated.StartedAt.Time
			if st.Reason == "" && cs.State.Terminated.ExitCode != 0 {
				st.Reason = fmt.Sprintf("%s (exit %d): %s", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode,
					strings.TrimSpace(cs.State.Terminated.Message))
			}
		case cs.State.Waiting != nil && st.Reason == "":
			st.Reason = cs.State.Waiting.Reason
		}
	}
	return st, nil
}

func (k *kube) PodLog(ctx context.Context, pod string) (string, error) {
	b, err := k.core.CoreV1().Pods(k.ns).GetLogs(pod, &corev1.PodLogOptions{}).DoRaw(ctx)
	return string(b), err
}

func (k *kube) DeletePod(ctx context.Context, pod string) error {
	err := k.core.CoreV1().Pods(k.ns).Delete(ctx, pod, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
