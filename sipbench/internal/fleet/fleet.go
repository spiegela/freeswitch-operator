// Package fleet lets scenarios act on the FreeSWITCH fleet under test, a FreeswitchCluster on Kubernetes: drain
// the node a call is on, roll the fleet, break and restore its config, and watch its status. It talks to the API
// server with client-go and doesn't import the operator, so the bench stays its own module.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
)

// RollEnv is the environment variable Roll sets on the fleet's pods.
const RollEnv = "SIPBENCH_ROLL"

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
		_, err = res.Update(ctx, u, metav1.UpdateOptions{FieldManager: "sipbench"})
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
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{FieldManager: "sipbench"})
		return err
	})
}
