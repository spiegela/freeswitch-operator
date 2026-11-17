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

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

type fakeNotifier struct {
	mu       sync.Mutex
	inactive []string
	reloads  int
	loaded   []string // the list the last reload asked for
	stale    int      // reloads still to come that find the old file, as before the kubelet syncs it
}

func (f *fakeNotifier) SetInactive(_ context.Context, setID int32, uri string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inactive = append(f.inactive, fmt.Sprintf("%d %s", setID, uri))
	return nil
}

func (f *fakeNotifier) Reload(_ context.Context, _ int32, want []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	if f.stale > 0 {
		f.stale--
		return fmt.Errorf("%w: test", errListNotLoaded)
	}
	f.loaded = want
	return nil
}

var nsCounter int

func newNamespace(ctx context.Context) string {
	nsCounter++
	name := fmt.Sprintf("fsc-test-%d", nsCounter)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}

func newCluster(ns string, mutate func(*sipv1alpha1.FreeswitchCluster)) *sipv1alpha1.FreeswitchCluster {
	fsc := &sipv1alpha1.FreeswitchCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: ns},
		Spec:       sipv1alpha1.FreeswitchClusterSpec{Image: "example/freeswitch:1"},
	}
	if mutate != nil {
		mutate(fsc)
	}
	return fsc
}

func fsPod(ns, name, node, rev string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":              "freeswitch",
				"app.kubernetes.io/instance":          "edge",
				sipv1alpha1.ClusterLabel:              "edge",
				appsv1.ControllerRevisionHashLabelKey: rev,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "freeswitch", Image: "example/freeswitch:1"}},
		},
	}
}

func setPodReady(ctx context.Context, p *corev1.Pod, ready bool) {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.Now()}}
	Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
}

var _ = Describe("FreeswitchCluster controller", func() {
	ctx := context.Background()

	reconcileOnce := func(r *FreeswitchClusterReconciler, ns string) reconcile.Result {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "edge"}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	It("creates the fleet's objects with the shape SIP media needs", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.Replicas = ptr.To[int32](3)
			f.Spec.Drain.MaxDrainSeconds = 900
		}))).To(Succeed())

		r := &FreeswitchClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileOnce(r, ns)

		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &sts)).To(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
		Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))
		Expect(sts.Spec.Template.Spec.HostNetwork).To(BeTrue())
		Expect(*sts.Spec.Template.Spec.TerminationGracePeriodSeconds).To(Equal(int64(930)))
		Expect(sts.OwnerReferences).To(HaveLen(1))
		hash := sts.Annotations[templateHashAnnotation]
		Expect(hash).NotTo(BeEmpty())

		var svc corev1.Service
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &svc)).To(Succeed())
		Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))

		var pdb policyv1.PodDisruptionBudget
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &pdb)).To(Succeed())
		Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(1))

		var scripts corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge-operator-scripts"}, &scripts)).To(Succeed())
		Expect(scripts.Data).To(HaveKey(drainScriptName))
		Expect(scripts.Data).To(HaveKey(readyScriptName))

		By("reconciling again without changes, the template is left alone")
		rv := sts.ResourceVersion
		reconcileOnce(r, ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &sts)).To(Succeed())
		Expect(sts.ResourceVersion).To(Equal(rv))

		By("changing the image, the template and its hash change")
		var fsc sipv1alpha1.FreeswitchCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		fsc.Spec.Image = "example/freeswitch:2"
		Expect(k8sClient.Update(ctx, &fsc)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("example/freeswitch:2"))
		Expect(sts.Annotations[templateHashAnnotation]).NotTo(Equal(hash))
	})

	It("hands each pod its node's external address", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, newCluster(ns, nil))).To(Succeed())

		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: ns + "-node-a"}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		node.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.1.10"},
			{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
		}
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

		p := fsPod(ns, "edge-0", node.Name, "r1")
		Expect(k8sClient.Create(ctx, p)).To(Succeed())

		r := &FreeswitchClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileOnce(r, ns)

		Expect(k8sClient.Get(ctx, podKey(p), p)).To(Succeed())
		Expect(p.Annotations).To(HaveKeyWithValue(sipv1alpha1.ExternalAddressAnnotation, "203.0.113.10"))

		var fsc sipv1alpha1.FreeswitchCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		Expect(fsc.Status.Members).To(HaveLen(1))
		Expect(fsc.Status.Members[0].ExternalAddress).To(Equal("203.0.113.10"))
		Expect(fsc.Status.Members[0].State).To(Equal(sipv1alpha1.MemberStarting))
	})

	It("keeps the dispatcher list in step with ready endpoints and tells Kamailio", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.Dispatcher = &sipv1alpha1.DispatcherSpec{ConfigMapName: "kamailio-dispatcher", SetID: 4, JSONRPCURL: "http://kamailio.invalid/RPC", ReloadDelaySeconds: 60}
		}))).To(Succeed())

		slice := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: "edge-abcde", Namespace: ns, Labels: map[string]string{discoveryv1.LabelServiceName: "edge"}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{
				{Addresses: []string{"10.0.1.10"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "edge-0"}},
				{Addresses: []string{"10.0.1.20"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "edge-1"}},
				{Addresses: []string{"10.0.1.30"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false), Terminating: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "edge-2"}},
			},
		}
		Expect(k8sClient.Create(ctx, slice)).To(Succeed())

		notifier := &fakeNotifier{}
		now := time.Now()
		r := &FreeswitchClusterReconciler{
			Client:      k8sClient,
			Scheme:      k8sClient.Scheme(),
			NewNotifier: func(string) DispatcherNotifier { return notifier },
			Now:         func() time.Time { return now },
		}
		res := reconcileOnce(r, ns)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0), "a reload should be scheduled after the delay")

		var cm corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kamailio-dispatcher"}, &cm)).To(Succeed())
		list := cm.Data[dispatcherListKey]
		Expect(list).To(ContainSubstring("4 sip:10.0.1.10:5060 0 0 duid=edge-0"))
		Expect(list).To(ContainSubstring("4 sip:10.0.1.20:5060 0 0 duid=edge-1"))
		Expect(list).NotTo(ContainSubstring("10.0.1.30"))
		Expect(notifier.reloads).To(Equal(0))

		By("once the delay passes, Kamailio is asked to reload")
		now = now.Add(61 * time.Second)
		reconcileOnce(r, ns)
		Expect(notifier.reloads).To(Equal(1))
		var fsc sipv1alpha1.FreeswitchCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		Expect(fsc.Status.DispatcherReloadedHash).To(Equal(fsc.Status.DispatcherHash))

		By("a node going unready leaves the list and is marked inactive at once")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge-abcde"}, slice)).To(Succeed())
		slice.Endpoints[1].Conditions.Ready = ptr.To(false)
		slice.Endpoints[1].Conditions.Terminating = ptr.To(true)
		Expect(k8sClient.Update(ctx, slice)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(notifier.inactive).To(Equal([]string{"4 sip:10.0.1.20:5060"}))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kamailio-dispatcher"}, &cm)).To(Succeed())
		Expect(cm.Data[dispatcherListKey]).NotTo(ContainSubstring("10.0.1.20"))
		Expect(notifier.reloads).To(Equal(1), "the reload waits for the kubelet to sync the file")

		By("a status write lost after the list changed still ends in a reload of the new list")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		changed := fsc.Status.DispatcherHash
		Expect(changed).To(Equal(cm.Annotations[sipv1alpha1.DispatcherHashAnnotation]))
		// Roll the status back to before the change, as if that update had hit a conflict.
		fsc.Status.DispatcherHash = fsc.Status.DispatcherReloadedHash
		Expect(k8sClient.Status().Update(ctx, &fsc)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		Expect(fsc.Status.DispatcherHash).To(Equal(changed), "the status mirrors the ConfigMap")
		Expect(notifier.reloads).To(Equal(1), "still inside the delay, measured from the change itself")
		now = now.Add(61 * time.Second)
		reconcileOnce(r, ns)
		Expect(notifier.reloads).To(Equal(2))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		Expect(fsc.Status.DispatcherReloadedHash).To(Equal(changed))
	})

	It("tells every ready Kamailio behind the JSON-RPC Service, not just the one the Service picks", func() {
		ns := newNamespace(ctx)
		kns := newNamespace(ctx)
		// Kamailio in its own namespace; the Service port (5070) maps to a different target port (5071).
		Expect(k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kamailio", Namespace: kns},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
				{Name: "sip", Port: 5060, Protocol: corev1.ProtocolUDP},
				{Name: "rpc", Port: 5070, TargetPort: intstr.FromInt32(5071), Protocol: corev1.ProtocolTCP},
			}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: "kamailio-xyz", Namespace: kns, Labels: map[string]string{discoveryv1.LabelServiceName: "kamailio"}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("sip"), Port: ptr.To[int32](5060)}, {Name: ptr.To("rpc"), Port: ptr.To[int32](5071)}},
			Endpoints: []discoveryv1.Endpoint{
				{Addresses: []string{"10.0.2.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}},
				{Addresses: []string{"10.0.2.2"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}},
				{Addresses: []string{"10.0.2.3"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}},
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.Dispatcher = &sipv1alpha1.DispatcherSpec{
				ConfigMapName: "kamailio-dispatcher", SetID: 1, ReloadDelaySeconds: 60,
				JSONRPCURL: fmt.Sprintf("http://kamailio.%s.svc.cluster.local:5070/RPC", kns),
			}
		}))).To(Succeed())
		slice := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: "edge-abcde", Namespace: ns, Labels: map[string]string{discoveryv1.LabelServiceName: "edge"}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{
				{Addresses: []string{"10.0.1.10"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "edge-0"}},
				{Addresses: []string{"10.0.1.20"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "edge-1"}},
			},
		}
		Expect(k8sClient.Create(ctx, slice)).To(Succeed())

		notifiers := map[string]*fakeNotifier{}
		now := time.Now()
		r := &FreeswitchClusterReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewNotifier: func(url string) DispatcherNotifier {
				if notifiers[url] == nil {
					notifiers[url] = &fakeNotifier{}
				}
				return notifiers[url]
			},
			Now: func() time.Time { return now },
		}
		// The second Kamailio's kubelet is slow: its first reload still finds the old file.
		notifiers["http://10.0.2.2:5071/RPC"] = &fakeNotifier{stale: 1}
		reconcileOnce(r, ns)
		now = now.Add(61 * time.Second)
		res := reconcileOnce(r, ns)
		Expect(notifiers).To(HaveLen(2), "one notifier per ready Kamailio, none for the unready one")
		for _, url := range []string{"http://10.0.2.1:5071/RPC", "http://10.0.2.2:5071/RPC"} {
			Expect(notifiers[url].reloads).To(Equal(1), url)
		}
		var fsc sipv1alpha1.FreeswitchCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		Expect(fsc.Status.DispatcherReloadedHash).NotTo(Equal(fsc.Status.DispatcherHash), "not done until every Kamailio has the list")
		Expect(res.RequeueAfter).To(Equal(10*time.Second), "reload again soon")

		By("the next reload finds the file and the reload is done")
		res = reconcileOnce(r, ns)
		for _, url := range []string{"http://10.0.2.1:5071/RPC", "http://10.0.2.2:5071/RPC"} {
			Expect(notifiers[url].reloads).To(Equal(2), url)
			Expect(notifiers[url].loaded).To(ConsistOf("sip:10.0.1.10:5060", "sip:10.0.1.20:5060"), url)
		}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		Expect(fsc.Status.DispatcherReloadedHash).To(Equal(fsc.Status.DispatcherHash))

		By("a leaving node is marked inactive on every Kamailio")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge-abcde"}, slice)).To(Succeed())
		slice.Endpoints[1].Conditions.Ready = ptr.To(false)
		slice.Endpoints[1].Conditions.Terminating = ptr.To(true)
		Expect(k8sClient.Update(ctx, slice)).To(Succeed())
		reconcileOnce(r, ns)
		for url, n := range notifiers {
			Expect(n.inactive).To(Equal([]string{"1 sip:10.0.1.20:5060"}), url)
		}
	})

	It("replaces one outdated pod at a time, only when the rest of the fleet is ready", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.Replicas = ptr.To[int32](3)
			f.Spec.ExternalAddress.Source = sipv1alpha1.ExternalAddressNone
		}))).To(Succeed())
		r := &FreeswitchClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileOnce(r, ns)

		// envtest runs no StatefulSet controller or kubelet: play their parts.
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &sts)).To(Succeed())
		sts.Status.Replicas = 3
		sts.Status.CurrentRevision = "edge-old"
		sts.Status.UpdateRevision = "edge-new"
		Expect(k8sClient.Status().Update(ctx, &sts)).To(Succeed())

		pods := []*corev1.Pod{fsPod(ns, "edge-0", "", "edge-old"), fsPod(ns, "edge-1", "", "edge-old"), fsPod(ns, "edge-2", "", "edge-new")}
		for _, p := range pods {
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			setPodReady(ctx, p, true)
		}

		reconcileOnce(r, ns)
		gone := func(name string) bool {
			var p corev1.Pod
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p)
			return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
		}
		Expect(gone("edge-1")).To(BeTrue(), "the highest outdated ordinal goes first")
		Expect(gone("edge-0")).To(BeFalse())

		By("while the fleet is short, nothing else is replaced")
		reconcileOnce(r, ns)
		Expect(gone("edge-0")).To(BeFalse())

		var fsc sipv1alpha1.FreeswitchCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "edge"}, &fsc)).To(Succeed())
		var progressing *metav1.Condition
		for i := range fsc.Status.Conditions {
			if fsc.Status.Conditions[i].Type == conditionProgressing {
				progressing = &fsc.Status.Conditions[i]
			}
		}
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
		Expect(strings.Contains(progressing.Message, "older revision")).To(BeTrue())

		By("the replacement comes back on the new revision and ready; the next outdated pod goes")
		p := fsPod(ns, "edge-1", "", "edge-new")
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		setPodReady(ctx, p, true)
		reconcileOnce(r, ns)
		Expect(gone("edge-0")).To(BeTrue())
	})

	It("waits for its ConfigMaps, lays them out as a tree, and rolls when their contents change", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.Config = sipv1alpha1.ConfigSpec{
				MountPath: "/usr/local/freeswitch/conf",
				ConfigMapRefs: []sipv1alpha1.ConfigMapRef{
					{Name: "fs-root", Path: "."},
					{Name: "fs-profiles", Path: "sip_profiles"},
				},
			}
		}))).To(Succeed())
		r := &FreeswitchClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		key := types.NamespacedName{Namespace: ns, Name: "edge"}

		// Nothing to run yet: no StatefulSet, and the status says why.
		reconcileOnce(r, ns)
		var sts appsv1.StatefulSet
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &sts))).To(BeTrue())
		var fsc sipv1alpha1.FreeswitchCluster
		Expect(k8sClient.Get(ctx, key, &fsc)).To(Succeed())
		degraded := meta.FindStatusCondition(fsc.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal("ConfigInvalid"))
		Expect(degraded.Message).To(ContainSubstring("not found"))

		root := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "fs-root", Namespace: ns}, Data: map[string]string{"freeswitch.xml": "<document/>"}}
		profiles := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "fs-profiles", Namespace: ns}, Data: map[string]string{"trunks.xml": "<profile/>"}}
		Expect(k8sClient.Create(ctx, root)).To(Succeed())
		Expect(k8sClient.Create(ctx, profiles)).To(Succeed())

		reconcileOnce(r, ns)
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		var paths []string
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Name == "config" {
				for _, src := range v.Projected.Sources {
					for _, it := range src.ConfigMap.Items {
						paths = append(paths, it.Path)
					}
				}
			}
		}
		Expect(paths).To(Equal([]string{"sip_profiles/trunks.xml", "freeswitch.xml"}), "references in name order")
		configHash := sts.Spec.Template.Annotations[configHashAnnotation]
		Expect(configHash).NotTo(BeEmpty())
		Expect(k8sClient.Get(ctx, key, &fsc)).To(Succeed())
		Expect(meta.FindStatusCondition(fsc.Status.Conditions, "Degraded").Reason).NotTo(Equal("ConfigInvalid"))

		// The pods mount the operator's immutable copies, never the user's ConfigMaps (the kubelet would push an
		// edit of those into every running pod at once).
		snapshotsOf := func() []string {
			Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
			var names []string
			for _, v := range sts.Spec.Template.Spec.Volumes {
				if v.Name == "config" {
					for _, src := range v.Projected.Sources {
						names = append(names, src.ConfigMap.Name)
					}
				}
			}
			return names
		}
		first := snapshotsOf()
		Expect(first).To(HaveLen(2))
		for _, name := range first {
			Expect(name).To(HavePrefix("edge-config-"))
			var snap corev1.ConfigMap
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &snap)).To(Succeed())
			Expect(snap.Immutable).To(HaveValue(BeTrue()))
			Expect(metav1.IsControlledBy(&snap, &fsc)).To(BeTrue())
		}
		snapshotExists := func(name string) bool {
			var snap corev1.ConfigMap
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &snap)
			return err == nil && snap.DeletionTimestamp == nil
		}

		// A content change makes a new snapshot and so a new template, which the rollout loop then works through.
		profiles.Data["trunks.xml"] = "<profile name=\"trunks\"/>"
		Expect(k8sClient.Update(ctx, profiles)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		Expect(sts.Spec.Template.Annotations[configHashAnnotation]).NotTo(Equal(configHash))
		second := snapshotsOf()
		// [0] is fs-profiles, [1] fs-root.
		Expect(second[1]).To(Equal(first[1]), "the unchanged ConfigMap keeps its snapshot")
		Expect(second[0]).NotTo(Equal(first[0]))
		Expect(snapshotExists(first[0])).To(BeTrue(), "kept until the StatefulSet controller has seen the new template")

		// A pod still on the old revision keeps its snapshot; once no pod mounts it, it goes.
		sts.Status.ObservedGeneration = sts.Generation
		Expect(k8sClient.Status().Update(ctx, &sts)).To(Succeed())
		old := fsPod(ns, "edge-0", "", "edge-old")
		old.Spec.Volumes = []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: first[0]}}}},
		}}}}
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(snapshotExists(first[0])).To(BeTrue(), "an old pod still mounts it")
		Expect(k8sClient.Delete(ctx, old, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			var p corev1.Pod
			return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(old), &p))
		}).Should(BeTrue())
		reconcileOnce(r, ns)
		Expect(snapshotExists(first[0])).To(BeFalse())
		Expect(snapshotExists(second[0])).To(BeTrue())
		Expect(snapshotExists(first[1])).To(BeTrue())

		// Deleting a ConfigMap leaves the running template alone and reports it.
		before := sts.Annotations[templateHashAnnotation]
		Expect(k8sClient.Delete(ctx, root)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		Expect(sts.Annotations[templateHashAnnotation]).To(Equal(before))
		Expect(k8sClient.Get(ctx, key, &fsc)).To(Succeed())
		Expect(meta.FindStatusCondition(fsc.Status.Conditions, "Ready").Reason).To(Equal("ConfigInvalid"))
	})

	It("pauses a roll while the configuration is invalid", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.ExternalAddress.Source = sipv1alpha1.ExternalAddressNone
			f.Spec.Config.ConfigMapRefs = []sipv1alpha1.ConfigMapRef{{Name: "fs-root", Path: "."}}
		}))).To(Succeed())
		root := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "fs-root", Namespace: ns}, Data: map[string]string{"freeswitch.xml": "<document/>"}}
		Expect(k8sClient.Create(ctx, root)).To(Succeed())
		r := &FreeswitchClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		key := types.NamespacedName{Namespace: ns, Name: "edge"}
		reconcileOnce(r, ns)

		// Mid-roll: one pod on the old revision, the fleet otherwise ready.
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, key, &sts)).To(Succeed())
		sts.Status.Replicas = 2
		sts.Status.CurrentRevision = "edge-old"
		sts.Status.UpdateRevision = "edge-new"
		Expect(k8sClient.Status().Update(ctx, &sts)).To(Succeed())
		for _, p := range []*corev1.Pod{fsPod(ns, "edge-0", "", "edge-new"), fsPod(ns, "edge-1", "", "edge-old")} {
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			setPodReady(ctx, p, true)
		}
		gone := func(name string) bool {
			var p corev1.Pod
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p)
			return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
		}

		Expect(k8sClient.Delete(ctx, root)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(gone("edge-1")).To(BeFalse(), "no pod is replaced while the configuration is invalid")
		var fsc sipv1alpha1.FreeswitchCluster
		Expect(k8sClient.Get(ctx, key, &fsc)).To(Succeed())
		Expect(meta.FindStatusCondition(fsc.Status.Conditions, conditionProgressing).Message).To(ContainSubstring("paused until the configuration is fixed"))

		root.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, root)).To(Succeed())
		reconcileOnce(r, ns)
		Expect(gone("edge-1")).To(BeTrue(), "the roll resumes once the configuration is back")
	})

	It("refuses config paths that escape the mount", func() {
		ns := newNamespace(ctx)
		for _, bad := range []string{"../etc", "a/../../b", "/abs", ".hidden"} {
			err := k8sClient.Create(ctx, newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
				f.Spec.Config.ConfigMapRefs = []sipv1alpha1.ConfigMapRef{{Name: "x", Path: bad}}
			}))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "path %q: %v", bad, err)
		}
	})
})

func podKey(p *corev1.Pod) types.NamespacedName {
	return types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
}
