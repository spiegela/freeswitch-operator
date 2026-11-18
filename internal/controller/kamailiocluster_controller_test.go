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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

func newProxy(ns string, mutate func(*sipv1alpha1.KamailioCluster)) *sipv1alpha1.KamailioCluster {
	kc := &sipv1alpha1.KamailioCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: ns},
		Spec: sipv1alpha1.KamailioClusterSpec{
			Image:           "example/kamailio:6",
			ExternalAddress: sipv1alpha1.ExternalAddressSpec{Source: sipv1alpha1.ExternalAddressNone},
		},
	}
	if mutate != nil {
		mutate(kc)
	}
	return kc
}

// publishFleetList plays a FreeswitchCluster's part: its dispatcher ConfigMap, controlled by it.
func publishFleetList(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, dests ...destination) {
	cfg := resolve(fsc)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fsc.Spec.Dispatcher.ConfigMapName, Namespace: fsc.Namespace}}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}, cm)
	cm.Data = map[string]string{dispatcherListKey: renderDispatcherList(cfg.dispatcherSetID, dests)}
	cm.Annotations = map[string]string{sipv1alpha1.DispatcherHashAnnotation: contentHash(cm.Data[dispatcherListKey])}
	if apierrors.IsNotFound(err) {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: fsc.Namespace, Name: fsc.Name}, fsc)).To(Succeed())
		cm.OwnerReferences = []metav1.OwnerReference{{APIVersion: sipv1alpha1.GroupVersion.String(), Kind: "FreeswitchCluster", Name: fsc.Name, UID: fsc.UID, Controller: ptr.To(true)}}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())
		return
	}
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.Update(ctx, cm)).To(Succeed())
}

func kamailioPod(ns, name, ip, rev string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":              "kamailio",
				"app.kubernetes.io/instance":          "proxy",
				sipv1alpha1.KamailioClusterLabel:      "proxy",
				appsv1.ControllerRevisionHashLabelKey: rev,
			},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "kamailio", Image: "example/kamailio:6"}}},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

func setKamailioPodReady(ctx context.Context, p *corev1.Pod) {
	ip := p.Status.PodIP
	p.Status = corev1.PodStatus{
		PodIP:      ip,
		PodIPs:     []corev1.PodIP{{IP: ip}},
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}},
	}
	Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
}

var _ = Describe("KamailioCluster controller", func() {
	ctx := context.Background()

	reconcileProxy := func(r *KamailioClusterReconciler, ns string) reconcile.Result {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "proxy"}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	getProxy := func(ns string) *sipv1alpha1.KamailioCluster {
		var kc sipv1alpha1.KamailioCluster
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy"}, &kc)).To(Succeed())
		return &kc
	}
	dispatchedFleet := func(ns, name string, setID int32) *sipv1alpha1.FreeswitchCluster {
		fsc := newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Name = name
			f.Spec.Dispatcher = &sipv1alpha1.DispatcherSpec{ConfigMapName: name + "-dispatcher", SetID: setID}
		})
		Expect(k8sClient.Create(ctx, fsc)).To(Succeed())
		return fsc
	}

	It("creates the proxy fleet with its token, rendered config and pod scripts", func() {
		ns := newNamespace(ctx)
		dispatchedFleet(ns, "edge", 1)
		Expect(k8sClient.Create(ctx, newProxy(ns, func(kc *sipv1alpha1.KamailioCluster) {
			kc.Spec.Dispatch = []sipv1alpha1.DispatchTarget{{FreeswitchCluster: "edge"}}
		}))).To(Succeed())
		r := &KamailioClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileProxy(r, ns)

		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-jsonrpc"}, &secret)).To(Succeed())
		token := string(secret.Data[kamailioTokenKey])
		Expect(token).To(HaveLen(64))
		Expect(metav1.IsControlledBy(&secret, getProxy(ns))).To(BeTrue())
		reconcileProxy(r, ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-jsonrpc"}, &secret)).To(Succeed())
		Expect(string(secret.Data[kamailioTokenKey])).To(Equal(token), "the token is made once")

		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-kamailio"}, &sts)).To(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(2)))
		var configName string
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Name == "config" {
				configName = v.ConfigMap.Name
			}
		}
		var cfg corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: configName}, &cfg)).To(Succeed())
		Expect(*cfg.Immutable).To(BeTrue())
		Expect(cfg.Data["kamailio.cfg"]).To(ContainSubstring(`ds_select_routes("1=4", "0")`))

		var scripts corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-kamailio-scripts"}, &scripts)).To(Succeed())
		Expect(scripts.Data).To(HaveKey(kamailioDrainScriptName))
		for _, kind := range []client.Object{&corev1.Service{}, &corev1.ConfigMap{}} {
			name := "proxy-kamailio"
			if _, ok := kind.(*corev1.ConfigMap); ok {
				name = "proxy-dispatcher"
			}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, kind)).To(Succeed(), name)
		}
	})

	It("combines the fleets' lists in priority order and leaves out a fleet whose setID is taken", func() {
		ns := newNamespace(ctx)
		edge := dispatchedFleet(ns, "edge", 1)
		overflow := dispatchedFleet(ns, "overflow", 2)
		dispatchedFleet(ns, "clash", 1)
		publishFleetList(ctx, edge, destination{URI: "sip:10.0.1.10:5060", Pod: "edge-0"})
		publishFleetList(ctx, overflow, destination{URI: "sip:10.0.3.10:5060", Pod: "overflow-0"})
		Expect(k8sClient.Create(ctx, newProxy(ns, func(kc *sipv1alpha1.KamailioCluster) {
			kc.Spec.Dispatch = []sipv1alpha1.DispatchTarget{{FreeswitchCluster: "overflow"}, {FreeswitchCluster: "edge"}, {FreeswitchCluster: "clash"}, {FreeswitchCluster: "nowhere"}}
		}))).To(Succeed())
		r := &KamailioClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileProxy(r, ns)

		var list corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-dispatcher"}, &list)).To(Succeed())
		content := list.Data[dispatcherListKey]
		Expect(content).To(ContainSubstring("2 sip:10.0.3.10:5060 8 0 duid=overflow-0\n# set 1: FreeswitchCluster edge\n1 sip:10.0.1.10:5060 8 0 duid=edge-0\n"))
		Expect(list.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation]).NotTo(BeEmpty())

		kc := getProxy(ns)
		Expect(kc.Status.Sets).To(Equal([]sipv1alpha1.DispatchSet{
			{FreeswitchCluster: "overflow", SetID: 2, Destinations: 1},
			{FreeswitchCluster: "edge", SetID: 1, Destinations: 1},
		}))
		conflict := meta.FindStatusCondition(kc.Status.Conditions, conditionSetConflict)
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue))
		Expect(conflict.Message).To(ContainSubstring("clash uses setID 1, as edge does"))
		Expect(conflict.Message).To(ContainSubstring("nowhere not found"))

		By("a fleet's membership change reaches the combined list; the config stays as it is")
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-kamailio"}, &sts)).To(Succeed())
		template := sts.Annotations[templateHashAnnotation]
		publishFleetList(ctx, edge, destination{URI: "sip:10.0.1.10:5060", Pod: "edge-0"}, destination{URI: "sip:10.0.1.20:5060", Pod: "edge-1"})
		reconcileProxy(r, ns)
		Expect(getProxy(ns).Status.Sets[1].Destinations).To(Equal(int32(2)))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-kamailio"}, &sts)).To(Succeed())
		Expect(sts.Annotations[templateHashAnnotation]).To(Equal(template), "membership is live, never a roll")

		By("a change to spec.dispatch is a change to the config, so it rolls")
		kc = getProxy(ns)
		kc.Spec.Dispatch = []sipv1alpha1.DispatchTarget{{FreeswitchCluster: "edge"}}
		Expect(k8sClient.Update(ctx, kc)).To(Succeed())
		reconcileProxy(r, ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-kamailio"}, &sts)).To(Succeed())
		Expect(sts.Annotations[templateHashAnnotation]).NotTo(Equal(template))
		Expect(meta.IsStatusConditionFalse(getProxy(ns).Status.Conditions, conditionSetConflict)).To(BeTrue())
	})

	It("reloads every ready Kamailio pod at its own address, with the token, until each holds the list", func() {
		ns := newNamespace(ctx)
		edge := dispatchedFleet(ns, "edge", 1)
		publishFleetList(ctx, edge, destination{URI: "sip:10.0.1.10:5060", Pod: "edge-0"}, destination{URI: "sip:10.0.1.20:5060", Pod: "edge-1"})
		Expect(k8sClient.Create(ctx, newProxy(ns, func(kc *sipv1alpha1.KamailioCluster) {
			kc.Spec.Dispatch = []sipv1alpha1.DispatchTarget{{FreeswitchCluster: "edge"}}
			kc.Spec.ReloadDelaySeconds = 60
		}))).To(Succeed())
		for _, p := range []*corev1.Pod{kamailioPod(ns, "proxy-kamailio-0", "10.0.9.1", "r1"), kamailioPod(ns, "proxy-kamailio-1", "10.0.9.2", "r1"), kamailioPod(ns, "proxy-kamailio-2", "10.0.9.3", "r1")} {
			ip := p.Status.PodIP
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.PodIP = ip
			if p.Name != "proxy-kamailio-2" {
				setKamailioPodReady(ctx, p)
			}
		}

		notifiers := map[string]*fakeNotifier{}
		now := time.Now()
		r := &KamailioClusterReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewNotifier: func(url, token string) DispatcherNotifier {
				if notifiers[url] == nil {
					notifiers[url] = &fakeNotifier{}
				}
				notifiers[url].token = token
				return notifiers[url]
			},
			Now: func() time.Time { return now },
		}
		notifiers["http://10.0.9.2:5070/RPC"] = &fakeNotifier{stale: 1}
		res := reconcileProxy(r, ns)
		Expect(res.RequeueAfter).To(BeNumerically("~", 60*time.Second, time.Second), "the reload waits for the kubelet")
		now = now.Add(61 * time.Second)
		res = reconcileProxy(r, ns)
		Expect(notifiers).To(HaveLen(2), "the ready pods only")
		var secret corev1.Secret
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-jsonrpc"}, &secret)).To(Succeed())
		for url, n := range notifiers {
			Expect(n.token).To(Equal(string(secret.Data[kamailioTokenKey])), url)
		}
		Expect(res.RequeueAfter).To(Equal(10*time.Second), "one pod's kubelet is slow: reload again soon")
		kc := getProxy(ns)
		Expect(kc.Status.DispatcherReloadedHash).NotTo(Equal(kc.Status.DispatcherHash))

		reconcileProxy(r, ns)
		for url, n := range notifiers {
			Expect(n.loaded).To(HaveKeyWithValue(int32(1), ConsistOf("sip:10.0.1.10:5060", "sip:10.0.1.20:5060")), url)
		}
		kc = getProxy(ns)
		Expect(kc.Status.DispatcherReloadedHash).To(Equal(kc.Status.DispatcherHash))

		By("a pod that becomes ready later may have read a stale file at start: it is reloaded and checked too")
		var late corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-kamailio-2"}, &late)).To(Succeed())
		late.Status.PodIP = "10.0.9.3"
		setKamailioPodReady(ctx, &late)
		notifiers["http://10.0.9.3:5070/RPC"] = &fakeNotifier{stale: 1}
		res = reconcileProxy(r, ns)
		Expect(res.RequeueAfter).To(Equal(10*time.Second), "the late pod's file is stale: reload again soon")
		kc = getProxy(ns)
		Expect(kc.Status.DispatcherReloadedHash).NotTo(Equal(kc.Status.DispatcherHash))
		reconcileProxy(r, ns)
		Expect(notifiers["http://10.0.9.3:5070/RPC"].loaded).To(HaveKeyWithValue(int32(1), ConsistOf("sip:10.0.1.10:5060", "sip:10.0.1.20:5060")))
		kc = getProxy(ns)
		Expect(kc.Status.DispatcherReloadedHash).To(Equal(kc.Status.DispatcherHash))
		reloads := notifiers["http://10.0.9.3:5070/RPC"].reloads
		reconcileProxy(r, ns)
		Expect(notifiers["http://10.0.9.3:5070/RPC"].reloads).To(Equal(reloads), "once confirmed, no more reloads")

		By("a node leaving its fleet is marked inactive on every ready Kamailio at once")
		publishFleetList(ctx, edge, destination{URI: "sip:10.0.1.10:5060", Pod: "edge-0"})
		reconcileProxy(r, ns)
		for url, n := range notifiers {
			Expect(n.inactive).To(Equal([]string{"1 sip:10.0.1.20:5060"}), url)
		}
	})

	It("replaces one outdated Kamailio pod at a time", func() {
		ns := newNamespace(ctx)
		Expect(k8sClient.Create(ctx, newProxy(ns, nil))).To(Succeed())
		r := &KamailioClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileProxy(r, ns)
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "proxy-kamailio"}, &sts)).To(Succeed())
		sts.Status.Replicas = 2
		sts.Status.UpdateRevision = "new"
		Expect(k8sClient.Status().Update(ctx, &sts)).To(Succeed())
		for _, p := range []*corev1.Pod{kamailioPod(ns, "proxy-kamailio-0", "10.0.9.1", "old"), kamailioPod(ns, "proxy-kamailio-1", "10.0.9.2", "old")} {
			ip := p.Status.PodIP
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.PodIP = ip
			setKamailioPodReady(ctx, p)
		}
		reconcileProxy(r, ns)
		gone := func(name string) bool {
			var p corev1.Pod
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p)
			return apierrors.IsNotFound(err) || (err == nil && p.DeletionTimestamp != nil)
		}
		Expect(gone("proxy-kamailio-1")).To(BeTrue())
		reconcileProxy(r, ns)
		Expect(gone("proxy-kamailio-0")).To(BeFalse(), "never two at once")
		Expect(meta.IsStatusConditionTrue(getProxy(ns).Status.Conditions, conditionProgressing)).To(BeTrue())
	})

	It("refuses an RPC port equal to the SIP port", func() {
		ns := newNamespace(ctx)
		err := k8sClient.Create(ctx, newProxy(ns, func(kc *sipv1alpha1.KamailioCluster) { kc.Spec.RPC.Port = 5060 }))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(k8sClient.Create(ctx, newProxy(ns, nil))).To(Succeed())
		kc := getProxy(ns)
		Expect(kc.Spec.SIP.Port).To(Equal(int32(5060)))
		Expect(kc.Spec.RPC.Port).To(Equal(int32(5070)))
		Expect(kc.Spec.ReloadDelaySeconds).To(Equal(int32(75)))
	})
})
