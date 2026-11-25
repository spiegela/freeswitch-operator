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
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
	"github.com/spiegela/freeswitch-operator/internal/addresspool"
)

const (
	// poolResync is how often a pool rereads its provider with nothing else happening, to see changes made
	// outside the operator. Each pass is one provider read (one DescribeAddresses on AWS).
	poolResync = 5 * time.Minute
	// poolConfirm is how soon a pool rereads its provider after binding or unbinding, to record the outcome.
	poolConfirm = 5 * time.Second
	// poolRetry is how soon a pool whose provider can't be built tries again.
	poolRetry = time.Minute
)

// SIPAddressPoolReconciler binds a pool's addresses to the nodes running pods that use it.
type SIPAddressPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewProvider builds each pool's provider. Nil means no provider is enabled.
	NewProvider addresspool.Factory

	// AgentNamespace is where the address agents run (the operator's namespace); empty reads them in every
	// namespace, as under make run.
	AgentNamespace string
}

// +kubebuilder:rbac:groups=sip.instruments.com,resources=sipaddresspools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sip.instruments.com,resources=sipaddresspools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sip.instruments.com,resources=sipaddresspools/finalizers,verbs=update
// +kubebuilder:rbac:groups=sip.instruments.com,resources=freeswitchclusters;kamailioclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods;nodes,verbs=get;list;watch

// Reconcile reads where the provider has each address, plans one pass, makes the provider calls, and records the
// outcome. Deleting a pool leaves its addresses where they are: they may be carrying calls.
func (r *SIPAddressPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pool sipv1alpha1.SIPAddressPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			forgetPool(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pool.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	status := pool.Status.DeepCopy()
	status.ObservedGeneration = pool.Generation
	gen := pool.Generation

	var provider addresspool.Provider
	err := fmt.Errorf("no address providers are enabled (--address-providers)")
	if r.NewProvider != nil {
		provider, err = r.NewProvider(ctx, &pool)
	}
	if err != nil {
		setPoolConditions(status, gen, metav1.ConditionFalse, sipv1alpha1.PoolReasonProviderUnavailable, err.Error())
		setProviderAvailable(status, gen, sipv1alpha1.PoolReasonProviderUnavailable, err)
		return ctrl.Result{RequeueAfter: poolRetry}, r.writePoolStatus(ctx, &pool, status)
	}
	caps := provider.Capabilities()
	status.Datapath, status.Fencing = caps.Datapath, caps.Fencing

	in, nodeNames, err := r.planInput(ctx, &pool, provider)
	if err != nil {
		return ctrl.Result{}, err
	}
	in.MayFence = caps.Fencing == sipv1alpha1.AddressFencingAuthoritative || pool.Spec.AllowAdvisoryFencing
	in.Observed, err = provider.Observe(ctx)
	recordPoolCall(pool.Name, "observe", err)
	if err != nil {
		setPoolConditions(status, gen, metav1.ConditionFalse, sipv1alpha1.PoolReasonProviderError, err.Error())
		setProviderAvailable(status, gen, sipv1alpha1.PoolReasonProviderError, err)
		if werr := r.writePoolStatus(ctx, &pool, status); werr != nil {
			return ctrl.Result{}, werr
		}
		return ctrl.Result{}, fmt.Errorf("reading the pool from its provider: %w", err)
	}
	poolLastObserved.WithLabelValues(pool.Name).SetToCurrentTime()
	setProviderAvailable(status, gen, "Observed", nil)
	if pr, ok := provider.(addresspool.Pruner); ok {
		// Bindings of addresses taken out of the spec. A failure costs nothing now: the next pass tries again.
		err := pr.Prune(ctx)
		recordPoolCall(pool.Name, "prune", err)
		if err != nil {
			log.Error(err, "taking back the bindings of addresses no longer in the pool")
		}
	}

	plan := addresspool.Decide(in)
	byID := map[string]int{}
	for i, a := range plan.Status {
		byID[a.ID] = i
	}
	for _, group := range plan.Steps {
		for _, s := range group {
			var err error
			if s.Unbind {
				log.Info("unbinding address", "address", s.Address.IP, "id", s.Address.ID, "from", s.Address.NodeID)
				err = provider.Unbind(ctx, s.Address)
				recordPoolCall(pool.Name, "unbind", err)
			} else {
				log.Info("binding address", "address", s.Address.IP, "id", s.Address.ID, "node", s.Node)
				err = provider.Bind(ctx, s.Address, s.NodeID)
				recordPoolCall(pool.Name, "bind", err)
			}
			if err != nil {
				log.Error(err, "address provider call failed", "id", s.Address.ID)
				// Record the address where it was observed, not where the plan meant it to go.
				st := &plan.Status[byID[s.Address.ID]]
				st.State, st.Message = sipv1alpha1.PoolAddressError, err.Error()
				st.NodeID, st.Node, st.ClaimedBy = s.Address.NodeID, nodeNames[s.Address.NodeID], nil
				if s.Unbind {
					st.Message = "unbinding: " + err.Error()
				} else {
					st.Message = "binding to " + s.Node + ": " + err.Error()
					plan.Waiting[s.Node] = "binding failed: " + err.Error()
				}
				break
			}
		}
	}
	status.Addresses = plan.Status
	recordPool(pool.Name, status.Addresses, len(plan.Waiting))

	var problems []string
	for _, n := range slices.Sorted(maps.Keys(plan.Waiting)) {
		problems = append(problems, fmt.Sprintf("node %s has no address: %s", n, plan.Waiting[n]))
	}
	for _, a := range status.Addresses {
		switch {
		case a.State == sipv1alpha1.PoolAddressConflict || a.State == sipv1alpha1.PoolAddressError:
			problems = append(problems, fmt.Sprintf("%s (%s) %s: %s", a.ID, a.Address, a.State, a.Message))
		case addresspool.AwaitingAgent(&a):
			problems = append(problems, fmt.Sprintf("node %s waits for its address agent to put %s on its interface", a.Node, a.BindAddress))
		}
	}
	if len(problems) == 0 {
		setPoolConditions(status, gen, metav1.ConditionTrue, "AddressesBound", "every node running a pod that uses the pool holds one of its addresses, ready to use")
	} else {
		setPoolConditions(status, gen, metav1.ConditionFalse, "AddressesMissing", strings.Join(problems, "; "))
	}
	if err := r.writePoolStatus(ctx, &pool, status); err != nil {
		return ctrl.Result{}, err
	}
	if len(plan.Steps) > 0 {
		return ctrl.Result{RequeueAfter: poolConfirm}, nil
	}
	return ctrl.Result{RequeueAfter: poolResync}, nil
}

// planInput is the planner's input from the cluster (all but the provider's report), and node names by the
// provider's ID for them, to record failures with.
func (r *SIPAddressPoolReconciler) planInput(
	ctx context.Context, pool *sipv1alpha1.SIPAddressPool, provider addresspool.Provider,
) (addresspool.Input, map[string]string, error) {
	log := logf.FromContext(ctx)
	in := addresspool.Input{Previous: pool.Status.Addresses}
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return in, nil, err
	}
	nodeNames := map[string]string{}
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		id, err := provider.NodeID(n)
		if err != nil {
			log.V(1).Info("provider can't name node", "node", n.Name, "reason", err.Error())
			id = ""
		}
		in.Nodes = append(in.Nodes, addresspool.Node{Name: n.Name, ID: id})
		if id != "" {
			nodeNames[id] = n.Name
		}
		if nodeFenced(n) {
			if in.Fenced == nil {
				in.Fenced = map[string]bool{}
			}
			in.Fenced[n.Name] = true
		}
	}
	var err error
	if in.Claimants, err = r.poolClaimants(ctx, pool.Name); err != nil {
		return in, nil, err
	}
	if in.NodeAddresses, err = r.agentReports(ctx); err != nil {
		return in, nil, err
	}
	if in.Refused, err = r.refusedNodes(ctx, pool); err != nil {
		return in, nil, err
	}
	return in, nodeNames, nil
}

// poolClaimants are the pods, scheduled to a node, of every fleet that draws its addresses from the pool.
func (r *SIPAddressPoolReconciler) poolClaimants(ctx context.Context, pool string) ([]addresspool.Claimant, error) {
	type fleet struct {
		kind, namespace, name string
		selector              map[string]string
	}
	var fleets []fleet
	var fscs sipv1alpha1.FreeswitchClusterList
	if err := r.List(ctx, &fscs); err != nil {
		return nil, err
	}
	for i := range fscs.Items {
		fsc := &fscs.Items[i]
		if addressPoolName(fsc.Spec.ExternalAddress) == pool {
			fleets = append(fleets, fleet{"FreeswitchCluster", fsc.Namespace, fsc.Name, selectorLabels(fsc)})
		}
	}
	var kcs sipv1alpha1.KamailioClusterList
	if err := r.List(ctx, &kcs); err != nil {
		return nil, err
	}
	for i := range kcs.Items {
		kc := &kcs.Items[i]
		if addressPoolName(kc.Spec.ExternalAddress) == pool {
			fleets = append(fleets, fleet{"KamailioCluster", kc.Namespace, kc.Name, kamailioSelectorLabels(kc)})
		}
	}
	var out []addresspool.Claimant
	for _, f := range fleets {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(f.namespace), client.MatchingLabels(f.selector)); err != nil {
			return nil, err
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName == "" {
				continue
			}
			out = append(out, addresspool.Claimant{
				Claim: sipv1alpha1.AddressClaim{Kind: f.kind, Namespace: f.namespace, Name: f.name, Pod: p.Name},
				Node:  p.Spec.NodeName,
			})
		}
	}
	return out, nil
}

// agentReports are the bind addresses each node's address agent reports on its interface, by node. A node whose
// agent hasn't reported (none runs there, or a new one before its first pass) is missing.
func (r *SIPAddressPoolReconciler) agentReports(ctx context.Context) (map[string][]string, error) {
	var pods corev1.PodList
	opts := []client.ListOption{client.MatchingLabels{sipv1alpha1.AppNameLabel: sipv1alpha1.AddressAgentName}}
	if r.AgentNamespace != "" {
		opts = append(opts, client.InNamespace(r.AgentNamespace))
	}
	if err := r.List(ctx, &pods, opts...); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" || p.DeletionTimestamp != nil {
			continue
		}
		if addrs, ok := addresspool.ReportedBindAddresses(p); ok {
			out[p.Spec.NodeName] = append(out[p.Spec.NodeName], addrs...)
		}
	}
	return out, nil
}

// vpcCNILabel is the label on the Amazon VPC CNI's node pods (the aws-node DaemonSet).
const vpcCNILabel = "k8s-app"

// refusedNodes are the nodes the pool must not bind a new address to, with why. An AWS pool with privateAddress
// Secondary assigns the node's primary interface a secondary private address, which the VPC CNI manages too: it
// can fill the interface's address limit, adopt the address for a pod, or release it as surplus (which disassociates
// the Elastic IP). So such a pool refuses nodes running the CNI's aws-node pod. A node's CNI pod runs before the
// node is Ready, and so before any fleet pod there claims an address.
func (r *SIPAddressPoolReconciler) refusedNodes(ctx context.Context, pool *sipv1alpha1.SIPAddressPool) (map[string]string, error) {
	spec := pool.Spec.AWSElasticIP
	if spec == nil || spec.PrivateAddress != sipv1alpha1.AWSPrivateAddressSecondary {
		return nil, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingLabels{vpcCNILabel: "aws-node"}); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range pods.Items {
		if p.Spec.NodeName != "" {
			out[p.Spec.NodeName] = "it runs the Amazon VPC CNI (" + p.Namespace + "/" + p.Name + "), which manages the " +
				"secondary addresses privateAddress Secondary assigns; use Primary, or nodes without the VPC CNI"
		}
	}
	return out, nil
}

func (r *SIPAddressPoolReconciler) writePoolStatus(ctx context.Context, pool *sipv1alpha1.SIPAddressPool, status *sipv1alpha1.SIPAddressPoolStatus) error {
	if equalJSON(&pool.Status, status) {
		return nil
	}
	pool.Status = *status
	if err := r.Status().Update(ctx, pool); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func setPoolConditions(status *sipv1alpha1.SIPAddressPoolStatus, gen int64, ready metav1.ConditionStatus, reason, msg string) {
	degraded := metav1.ConditionFalse
	if ready != metav1.ConditionTrue {
		degraded = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: sipv1alpha1.PoolConditionReady, Status: ready, Reason: reason, Message: msg, ObservedGeneration: gen})
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionDegraded, Status: degraded, Reason: reason, Message: msg, ObservedGeneration: gen})
}

// setProviderAvailable records whether the pass read the provider: err nil, or why not.
func setProviderAvailable(status *sipv1alpha1.SIPAddressPoolStatus, gen int64, reason string, err error) {
	c := metav1.Condition{Type: sipv1alpha1.PoolConditionProviderAvailable, Status: metav1.ConditionTrue, Reason: reason, Message: "the provider answered", ObservedGeneration: gen}
	if err != nil {
		c.Status, c.Message = metav1.ConditionFalse, err.Error()
	}
	meta.SetStatusCondition(&status.Conditions, c)
}

// addressPoolName is the pool a fleet draws from, or empty.
func addressPoolName(s sipv1alpha1.ExternalAddressSpec) string {
	if s.Source != sipv1alpha1.ExternalAddressAddressPool || s.AddressPoolRef == nil {
		return ""
	}
	return s.AddressPoolRef.Name
}

// SetupWithManager sets up the controller with the Manager. Pools are few, so a pod, node, fleet or address agent
// event requeues every pool; the planner finds nothing to do for those it doesn't concern.
func (r *SIPAddressPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	allPools := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list sipv1alpha1.SIPAddressPoolList
		if err := r.List(ctx, &list); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, p := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
		}
		return out
	})
	// Only what changes a pod's claim: a fleet pod appearing, being scheduled, or going. Readiness and status
	// churn would otherwise cost a provider read each.
	fleetPod := func(o client.Object) bool {
		l := o.GetLabels()
		return l[sipv1alpha1.ClusterLabel] != "" || l[sipv1alpha1.KamailioClusterLabel] != ""
	}
	podClaims := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return fleetPod(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return fleetPod(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, n := e.ObjectOld.(*corev1.Pod), e.ObjectNew.(*corev1.Pod)
			return fleetPod(n) && o.Spec.NodeName != n.Spec.NodeName
		},
	}
	// An address agent's report is an annotation on its own pod; creating and deleting one pass too.
	agentPod := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[sipv1alpha1.AppNameLabel] == sipv1alpha1.AddressAgentName
	})
	// Nodes matter when they come and go, when their provider ID appears, or when a failover fences one.
	nodeChanges := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, n := e.ObjectOld.(*corev1.Node), e.ObjectNew.(*corev1.Node)
			return o.Spec.ProviderID != n.Spec.ProviderID || nodeFenced(o) != nodeFenced(n)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sipv1alpha1.SIPAddressPool{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Pod{}, allPools, builder.WithPredicates(podClaims)).
		Watches(&corev1.Pod{}, allPools, builder.WithPredicates(agentPod, predicate.AnnotationChangedPredicate{})).
		Watches(&corev1.Node{}, allPools, builder.WithPredicates(nodeChanges)).
		Watches(&sipv1alpha1.FreeswitchCluster{}, allPools, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&sipv1alpha1.KamailioCluster{}, allPools, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("sipaddresspool").
		Complete(r)
}
