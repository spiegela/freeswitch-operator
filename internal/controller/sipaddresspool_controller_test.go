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
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
	"github.com/spiegela/freeswitch-operator/internal/addresspool"
)

var poolCounter int

var _ = Describe("SIPAddressPool controller", func() {
	ctx := context.Background()

	// Each test gets its own pool, namespace and node names, since pools and nodes are cluster-scoped.
	type world struct {
		pool, ns string
		fake     *addresspool.Fake
		r        *SIPAddressPoolReconciler
	}
	setup := func(ips ...string) world {
		poolCounter++
		w := world{pool: fmt.Sprintf("pool-%d", poolCounter), ns: newNamespace(ctx)}
		ids := make([]string, len(ips))
		for i := range ips {
			ids[i] = fmt.Sprintf("eipalloc-%d%d", poolCounter, i)
		}
		w.fake = addresspool.NATFake(ids, ips)
		Expect(k8sClient.Create(ctx, &sipv1alpha1.SIPAddressPool{
			ObjectMeta: metav1.ObjectMeta{Name: w.pool},
			Spec:       sipv1alpha1.SIPAddressPoolSpec{AWSElasticIP: &sipv1alpha1.AWSElasticIPPool{Region: "us-east-1", AllocationIDs: ids}},
		})).To(Succeed())
		w.r = &SIPAddressPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewProvider: func(context.Context, *sipv1alpha1.SIPAddressPool) (addresspool.Provider, error) { return w.fake, nil }}
		fsc := newCluster(w.ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.ExternalAddress = sipv1alpha1.ExternalAddressSpec{
				Source:         sipv1alpha1.ExternalAddressAddressPool,
				AddressPoolRef: &sipv1alpha1.AddressPoolReference{Name: w.pool},
			}
		})
		Expect(k8sClient.Create(ctx, fsc)).To(Succeed())
		return w
	}
	node := func(w world, name string) string {
		full := fmt.Sprintf("%s-%s", w.pool, name)
		Expect(k8sClient.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: full}})).To(Succeed())
		return full
	}
	reconcilePool := func(w world) (reconcile.Result, *sipv1alpha1.SIPAddressPool) {
		res, err := w.r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: w.pool}})
		Expect(err).NotTo(HaveOccurred())
		var pool sipv1alpha1.SIPAddressPool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: w.pool}, &pool)).To(Succeed())
		return res, &pool
	}
	annotation := func(ns, pod string) string {
		var p corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pod}, &p)).To(Succeed())
		return p.Annotations[sipv1alpha1.ExternalAddressAnnotation]
	}
	syncFleet := func(w world) {
		var pods corev1.PodList
		Expect(k8sClient.List(ctx, &pods, client.InNamespace(w.ns))).To(Succeed())
		Expect(syncExternalAddresses(ctx, k8sClient, sipv1alpha1.ExternalAddressAddressPool, w.pool, pods.Items)).To(Succeed())
	}

	It("binds an address to each node running a pod of a fleet that uses the pool, and the pods advertise it", func() {
		w := setup("203.0.113.10", "203.0.113.11", "203.0.113.12")
		a, b := node(w, "a"), node(w, "b")
		node(w, "spare")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", a, "r1"))).To(Succeed())
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-1", b, "r1"))).To(Succeed())

		res, pool := reconcilePool(w)
		Expect(res.RequeueAfter).To(Equal(poolConfirm))
		Expect(w.fake.CallLog()).To(ConsistOf("bind eipalloc-"+fmt.Sprint(poolCounter)+"0 "+a, "bind eipalloc-"+fmt.Sprint(poolCounter)+"1 "+b))
		Expect(pool.Status.Datapath).To(Equal(sipv1alpha1.AddressDatapathNAT))
		Expect(pool.Status.Fencing).To(Equal(sipv1alpha1.AddressFencingAuthoritative))
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, conditionReady)).To(BeTrue())
		states := map[string]sipv1alpha1.PoolAddressState{}
		for _, ad := range pool.Status.Addresses {
			states[ad.Address] = ad.State
		}
		Expect(states).To(Equal(map[string]sipv1alpha1.PoolAddressState{
			"203.0.113.10": sipv1alpha1.PoolAddressBound, "203.0.113.11": sipv1alpha1.PoolAddressBound, "203.0.113.12": sipv1alpha1.PoolAddressFree,
		}))

		By("the pods get the addresses only once a read confirms them")
		syncFleet(w)
		Expect(annotation(w.ns, "edge-0")).To(BeEmpty())

		By("a second pass confirms them, and finds nothing to do")
		w.fake.ResetCalls()
		res, _ = reconcilePool(w)
		Expect(w.fake.CallLog()).To(BeEmpty())
		Expect(res.RequeueAfter).To(Equal(poolResync))
		syncFleet(w)
		Expect(annotation(w.ns, "edge-0")).To(Equal("203.0.113.10"))
		Expect(annotation(w.ns, "edge-1")).To(Equal("203.0.113.11"))
	})

	It("moves a replaced node's address to the node its pod comes back on", func() {
		w := setup("203.0.113.20", "203.0.113.21", "203.0.113.22")
		a, b := node(w, "a"), node(w, "b")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", a, "r1"))).To(Succeed())
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-1", b, "r1"))).To(Succeed())
		reconcilePool(w)
		id1 := fmt.Sprintf("eipalloc-%d1", poolCounter)
		Expect(w.fake.BoundTo(id1)).To(Equal(b))

		By("node b goes away with its pod, and edge-1 comes back on a new node")
		var p corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: w.ns, Name: "edge-1"}, &p)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &p, client.GracePeriodSeconds(0))).To(Succeed())
		Expect(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: b}})).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: w.ns, Name: "edge-1"}, &corev1.Pod{}))
		}).Should(BeTrue())
		c := node(w, "c")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-1", c, "r1"))).To(Succeed())

		w.fake.ResetCalls()
		reconcilePool(w)
		Expect(w.fake.CallLog()).To(Equal([]string{"unbind " + id1, "bind " + id1 + " " + c}))
		_, pool := reconcilePool(w)
		Expect(addresspool.BindingForNode(&pool.Status, c)).To(HaveField("Address", "203.0.113.21"))
		syncFleet(w)
		Expect(annotation(w.ns, "edge-1")).To(Equal("203.0.113.21"))
	})

	It("takes its address off a node a failover fenced, and the failed-over pod comes back with one on the spare", func() {
		w := setup("203.0.113.30", "203.0.113.31", "203.0.113.32")
		a, b := node(w, "a"), node(w, "b")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", a, "r1"))).To(Succeed())
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-1", b, "r1"))).To(Succeed())
		reconcilePool(w)
		id1 := fmt.Sprintf("eipalloc-%d1", poolCounter)
		Expect(w.fake.BoundTo(id1)).To(Equal(b))

		By("the failover fences b and deletes edge-1, which the StatefulSet starts on the spare")
		Expect(fenceNode(ctx, k8sClient, k8sClient, b, "test")).To(Succeed())
		var p corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: w.ns, Name: "edge-1"}, &p)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &p, client.GracePeriodSeconds(0))).To(Succeed())
		spare := node(w, "spare")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-1", spare, "r1"))).To(Succeed())

		w.fake.ResetCalls()
		reconcilePool(w)
		Expect(w.fake.CallLog()).To(Equal([]string{"unbind " + id1, "bind " + id1 + " " + spare}), "b's address follows its pod")
		_, pool := reconcilePool(w)
		Expect(addresspool.BindingForNode(&pool.Status, spare)).To(HaveField("Address", "203.0.113.31"))
		Expect(addresspool.BindingForNode(&pool.Status, b)).To(BeNil())

		By("a pod scheduled to the fenced node anyway gets no address there")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-2", b, "r1"))).To(Succeed())
		w.fake.ResetCalls()
		reconcilePool(w)
		Expect(w.fake.CallLog()).To(BeEmpty())
	})

	It("serves a Kamailio fleet and a FreeSWITCH fleet from one pool", func() {
		w := setup("203.0.113.60", "203.0.113.61")
		proxyNode, mediaNode := node(w, "proxy"), node(w, "media")
		kc := newProxy(w.ns, func(k *sipv1alpha1.KamailioCluster) {
			k.Spec.ExternalAddress = sipv1alpha1.ExternalAddressSpec{
				Source:         sipv1alpha1.ExternalAddressAddressPool,
				AddressPoolRef: &sipv1alpha1.AddressPoolReference{Name: w.pool},
			}
		})
		Expect(k8sClient.Create(ctx, kc)).To(Succeed())
		kp := kamailioPod(w.ns, "proxy-0", "10.0.0.5", "r1")
		kp.Spec.NodeName = proxyNode
		Expect(k8sClient.Create(ctx, kp)).To(Succeed())
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", mediaNode, "r1"))).To(Succeed())

		reconcilePool(w)
		_, pool := reconcilePool(w)
		claims := map[string]string{}
		for _, a := range pool.Status.Addresses {
			Expect(a.State).To(Equal(sipv1alpha1.PoolAddressBound))
			claims[a.ClaimedBy[0].Kind] = a.Node
		}
		Expect(claims).To(Equal(map[string]string{"KamailioCluster": proxyNode, "FreeswitchCluster": mediaNode}))

		var pods corev1.PodList
		Expect(k8sClient.List(ctx, &pods, client.InNamespace(w.ns), client.MatchingLabels{sipv1alpha1.KamailioClusterLabel: "proxy"})).To(Succeed())
		Expect(syncExternalAddresses(ctx, k8sClient, sipv1alpha1.ExternalAddressAddressPool, w.pool, pods.Items)).To(Succeed())
		Expect(addresspool.BindingForNode(&pool.Status, proxyNode)).NotTo(BeNil())
		Expect(annotation(w.ns, "proxy-0")).To(Equal(addresspool.BindingForNode(&pool.Status, proxyNode).Address))
		Expect(annotation(w.ns, "proxy-0")).NotTo(BeEmpty())
	})

	It("leaves an address bound outside the cluster alone and reports it", func() {
		w := setup("203.0.113.30", "203.0.113.31")
		a := node(w, "a")
		w.fake.SetBound(fmt.Sprintf("eipalloc-%d0", poolCounter), "i-not-ours")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", a, "r1"))).To(Succeed())

		_, pool := reconcilePool(w)
		Expect(w.fake.CallLog()).To(Equal([]string{fmt.Sprintf("bind eipalloc-%d1 %s", poolCounter, a)}))
		Expect(pool.Status.Addresses[0].State).To(Equal(sipv1alpha1.PoolAddressConflict))
		Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, conditionDegraded)).To(BeTrue())

		By("the next pass leaves it alone too")
		w.fake.ResetCalls()
		_, pool = reconcilePool(w)
		Expect(w.fake.CallLog()).To(BeEmpty())
		Expect(pool.Status.Addresses[0].State).To(Equal(sipv1alpha1.PoolAddressConflict))
	})

	It("records a failed bind, keeps the pod without an address, and binds on the next pass", func() {
		w := setup("203.0.113.40")
		a := node(w, "a")
		id := fmt.Sprintf("eipalloc-%d0", poolCounter)
		w.fake.FailBind[id] = errors.New("UnauthorizedOperation")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", a, "r1"))).To(Succeed())

		_, pool := reconcilePool(w)
		Expect(pool.Status.Addresses[0].State).To(Equal(sipv1alpha1.PoolAddressError))
		Expect(pool.Status.Addresses[0].Message).To(ContainSubstring("UnauthorizedOperation"))
		Expect(meta.FindStatusCondition(pool.Status.Conditions, conditionReady).Message).To(ContainSubstring("has no address"))
		syncFleet(w)
		Expect(annotation(w.ns, "edge-0")).To(BeEmpty())

		_, pool = reconcilePool(w)
		Expect(pool.Status.Addresses[0].State).To(Equal(sipv1alpha1.PoolAddressBound))
		reconcilePool(w)
		syncFleet(w)
		Expect(annotation(w.ns, "edge-0")).To(Equal("203.0.113.40"))
	})

	It("gives pods a secondary bind address only once the node's address agent has it on the interface", func() {
		w := setup("203.0.113.70")
		var pool sipv1alpha1.SIPAddressPool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: w.pool}, &pool)).To(Succeed())
		pool.Spec.AWSElasticIP.PrivateAddress = sipv1alpha1.AWSPrivateAddressSecondary
		Expect(k8sClient.Update(ctx, &pool)).To(Succeed())
		id := fmt.Sprintf("eipalloc-%d0", poolCounter)
		w.fake.BindIPs = map[string]string{id: "10.0.9.70"}
		a := node(w, "a")
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", a, "r1"))).To(Succeed())
		bindAnnotation := func() string {
			var p corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: w.ns, Name: "edge-0"}, &p)).To(Succeed())
			return p.Annotations[sipv1alpha1.BindAddressAnnotation]
		}

		By("the bind pass doesn't know the bind address yet; the confirming pass reads it")
		_, st := reconcilePool(w)
		Expect(st.Status.Addresses[0].BindAddress).To(BeEmpty())
		_, st = reconcilePool(w)
		Expect(st.Status.Addresses[0].BindAddress).To(Equal("10.0.9.70"))

		Expect(st.Status.Addresses[0].BindAddressReady).To(BeFalse())
		Expect(meta.FindStatusCondition(st.Status.Conditions, conditionReady).Message).To(ContainSubstring("waits for its address agent"))

		By("no agent has reported it, so the pod gets neither address")
		syncFleet(w)
		Expect(annotation(w.ns, "edge-0")).To(BeEmpty())
		Expect(bindAnnotation()).To(BeEmpty())

		By("the agent on the node reports it, and the pool hands it out")
		agentNS := newNamespace(ctx)
		w.r.AgentNamespace = agentNS
		agent := func(name string, annotations map[string]string) *corev1.Pod {
			return &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentNS, Annotations: annotations,
					Labels: map[string]string{sipv1alpha1.AppNameLabel: sipv1alpha1.AddressAgentName}},
				Spec: corev1.PodSpec{NodeName: a, Containers: []corev1.Container{{Name: "agent", Image: "agent"}}},
			}
		}
		first := agent("agent-1", map[string]string{sipv1alpha1.AgentBindAddressesAnnotation: "10.0.9.70"})
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		_, st = reconcilePool(w)
		Expect(st.Status.Addresses[0].BindAddressReady).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(st.Status.Conditions, conditionReady)).To(BeTrue())
		syncFleet(w)
		Expect(annotation(w.ns, "edge-0")).To(Equal("203.0.113.70"))
		Expect(bindAnnotation()).To(Equal("10.0.9.70"))

		By("a replacement agent that hasn't reported yet leaves it ready")
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		Expect(k8sClient.Create(ctx, agent("agent-2", nil))).To(Succeed())
		_, st = reconcilePool(w)
		Expect(st.Status.Addresses[0].BindAddressReady).To(BeTrue())

		By("an agent that reports it gone takes it back from the pods")
		var second corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: agentNS, Name: "agent-2"}, &second)).To(Succeed())
		second.Annotations = map[string]string{sipv1alpha1.AgentBindAddressesAnnotation: ""}
		Expect(k8sClient.Update(ctx, &second)).To(Succeed())
		_, st = reconcilePool(w)
		Expect(st.Status.Addresses[0].BindAddressReady).To(BeFalse())
		syncFleet(w)
		Expect(annotation(w.ns, "edge-0")).To(BeEmpty())
	})

	It("refuses a Secondary pool a node running the VPC CNI, and keeps an address already there", func() {
		w := setup("203.0.113.80", "203.0.113.81")
		var pool sipv1alpha1.SIPAddressPool
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: w.pool}, &pool)).To(Succeed())
		pool.Spec.AWSElasticIP.PrivateAddress = sipv1alpha1.AWSPrivateAddressSecondary
		Expect(k8sClient.Update(ctx, &pool)).To(Succeed())
		a, b := node(w, "a"), node(w, "b")
		w.fake.SetBound(fmt.Sprintf("eipalloc-%d0", poolCounter), a)
		cniNS := newNamespace(ctx)
		for _, n := range []string{a, b} {
			Expect(k8sClient.Create(ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "aws-node-" + n, Namespace: cniNS, Labels: map[string]string{vpcCNILabel: "aws-node"}},
				Spec:       corev1.PodSpec{NodeName: n, Containers: []corev1.Container{{Name: "aws-node", Image: "aws-node"}}},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-0", a, "r1"))).To(Succeed())
		Expect(k8sClient.Create(ctx, fsPod(w.ns, "edge-1", b, "r1"))).To(Succeed())

		_, st := reconcilePool(w)
		Expect(w.fake.CallLog()).To(BeEmpty())
		Expect(st.Status.Addresses[0].State).To(Equal(sipv1alpha1.PoolAddressBound))
		Expect(st.Status.Addresses[1].State).To(Equal(sipv1alpha1.PoolAddressFree))
		Expect(meta.IsStatusConditionTrue(st.Status.Conditions, conditionDegraded)).To(BeTrue())
		Expect(meta.FindStatusCondition(st.Status.Conditions, conditionReady).Message).To(
			ContainSubstring("node " + b + " has no address: it runs the Amazon VPC CNI"))
	})

	It("reports a provider that isn't enabled", func() {
		w := setup("203.0.113.50")
		w.r.NewProvider = addresspool.Registry{}.Factory()
		res, pool := reconcilePool(w)
		Expect(res.RequeueAfter).To(Equal(poolRetry))
		cond := meta.FindStatusCondition(pool.Status.Conditions, conditionReady)
		Expect(cond.Reason).To(Equal("ProviderUnavailable"))
		Expect(cond.Message).To(ContainSubstring(`provider "aws" is not enabled`))
		Expect(meta.FindStatusCondition(pool.Status.Conditions, sipv1alpha1.PoolConditionProviderAvailable)).To(HaveField("Status", metav1.ConditionFalse))
	})

	It("refuses a pool without a provider and a fleet source without its pool", func() {
		Expect(k8sClient.Create(ctx, &sipv1alpha1.SIPAddressPool{ObjectMeta: metav1.ObjectMeta{Name: "no-provider"}})).
			To(MatchError(ContainSubstring("exactly one provider must be set")))
		ns := newNamespace(ctx)
		fsc := newCluster(ns, func(f *sipv1alpha1.FreeswitchCluster) {
			f.Spec.ExternalAddress.Source = sipv1alpha1.ExternalAddressAddressPool
		})
		Expect(k8sClient.Create(ctx, fsc)).To(MatchError(ContainSubstring("addressPoolRef is set exactly when source is AddressPool")))
	})
})
