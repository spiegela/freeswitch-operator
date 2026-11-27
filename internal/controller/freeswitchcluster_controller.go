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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
	"github.com/spiegela/freeswitch-operator/internal/addresspool"
)

const (
	templateHashAnnotation = "sip.instruments.com/template-hash"
	podNodeNameField       = "spec.nodeName"
	configMapRefField      = "spec.config.configMapRefs.name"

	conditionAvailable   = "Available"
	conditionProgressing = "Progressing"
	conditionDegraded    = "Degraded"
	conditionReady       = "Ready"
)

// FreeswitchClusterReconciler reconciles a FreeswitchCluster object.
type FreeswitchClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewNotifier builds the Kamailio notifier for spec.dispatcher.jsonrpcURL. Nil disables notification.
	NewNotifier NotifierFactory
	// APIReader reads straight from the API server, for the reads a stale cache would make fail (fencing a node);
	// nil means the Client.
	APIReader client.Reader
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// SIPProbes reads the SIP prober's results; nil means no SIP signal.
	SIPProbes SIPProbeResults
	// ProbeEvents, when set, delivers fleets whose SIP signal changed (the SIP prober's Events).
	ProbeEvents <-chan event.GenericEvent
	// NodeLeases reads when each node's lease was last seen renewed; nil means a node is lost only by its Ready
	// condition. Its Events go on ProbeEvents too.
	NodeLeases NodeLeaseResults
	// Recorder records events on the fleet; nil records none.
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=sip.instruments.com,resources=freeswitchclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sip.instruments.com,resources=freeswitchclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sip.instruments.com,resources=freeswitchclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=sip.instruments.com,resources=sipaddresspools,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// Reconcile moves a fleet toward its spec: the objects around the pods, each pod's external address, one planned
// replacement at a time, the dispatcher list, and the status.
func (r *FreeswitchClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	fleet := fleetRef{kind: kindFreeswitch, namespace: req.Namespace, name: req.Name}
	var fsc sipv1alpha1.FreeswitchCluster
	if err := r.Get(ctx, req.NamespacedName, &fsc); err != nil {
		if apierrors.IsNotFound(err) {
			forgetFleet(fleet)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if fsc.DeletionTimestamp != nil {
		// Owner references let the garbage collector remove everything; each pod still drains in preStop.
		forgetFleet(fleet)
		return ctrl.Result{}, nil
	}
	cfg := resolve(&fsc)
	status := fsc.Status.DeepCopy()
	status.ObservedGeneration = fsc.Generation

	if err := r.ensureScriptsConfigMap(ctx, &fsc); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, &fsc); err != nil {
		return ctrl.Result{}, err
	}
	if err := ensurePDB(ctx, r.Client, r.Scheme, &fsc, newPDB(pdbName(&fsc), fsc.Namespace, commonLabels(&fsc), selectorLabels(&fsc))); err != nil {
		return ctrl.Result{}, err
	}
	// A missing or conflicting ConfigMap is the user's to fix: report it, and keep the running fleet as it is,
	// with its template unchanged and any roll in progress paused.
	conf, confErr := r.loadConfig(ctx, &fsc)
	stsKey := types.NamespacedName{Namespace: fsc.Namespace, Name: statefulSetName(&fsc)}
	sts := &appsv1.StatefulSet{}
	// A StatefulSet the API server refuses (requests above limits, say) mustn't stop the fleet's health, failovers
	// and dispatcher list either: report it, carry on with the StatefulSet as it is, and retry with backoff.
	var stsErr error
	if confErr != nil {
		var ce *configError
		if !errors.As(confErr, &ce) {
			return ctrl.Result{}, confErr
		}
		log.Info("configuration not usable; leaving the StatefulSet as it is", "reason", ce.msg)
		if err := r.Get(ctx, stsKey, sts); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	} else {
		stsErr = r.ensureConfigSnapshots(ctx, &fsc, conf)
		if stsErr == nil {
			sts, stsErr = r.ensureStatefulSet(ctx, &fsc, conf)
		}
		if stsErr != nil {
			log.Error(stsErr, "could not write the StatefulSet; leaving it as it is")
			sts = &appsv1.StatefulSet{}
			if err := r.Get(ctx, stsKey, sts); err != nil {
				return ctrl.Result{}, errors.Join(stsErr, client.IgnoreNotFound(err))
			}
		}
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(fsc.Namespace), client.MatchingLabels(selectorLabels(&fsc))); err != nil {
		return ctrl.Result{}, err
	}
	pods := podList.Items
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })

	nodes, err := r.syncPods(ctx, cfg, pods)
	if err != nil {
		return ctrl.Result{}, err
	}

	if confErr == nil && stsErr == nil {
		keep := map[string]bool{}
		for _, snap := range conf.snapshots {
			keep[snap.name] = true
		}
		for i := range pods {
			podConfigSnapshots(&pods[i], keep)
		}
		if err := pruneConfigSnapshots(ctx, r.Client, &fsc, sts, client.MatchingLabels{sipv1alpha1.ClusterLabel: fsc.Name}, keep); err != nil {
			return ctrl.Result{}, err
		}
	}

	var requeue time.Duration
	decision := nextPodToReplace(pods, sts.Status.UpdateRevision, cfg.replicas)
	if confErr != nil && decision.Outdated > 0 {
		// The template being rolled to may mount what is now missing; don't trade a working node for one that
		// can't start.
		decision.Replace = nil
		decision.Waiting = "paused until the configuration is fixed"
	}
	if err := replacePod(ctx, r.Client, fleet, decision, sts.Status.UpdateRevision); err != nil {
		return ctrl.Result{}, err
	}

	if cfg.dispatcher != nil {
		after, err := r.syncDispatcher(ctx, &fsc, fleet, cfg, status, pods, nodes)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = after
	}

	r.fillStatus(&fsc, cfg, sts, pods, decision, status)
	// A failover that failed is retried, but the status is written first, so it goes on showing the fleet.
	healthAfter, failoverErr := r.syncHealth(ctx, &fsc, fleet, cfg, pods, nodes, status)
	requeue = minPositive(requeue, healthAfter)
	setWriteProblem(&status.Conditions, fsc.Generation, confErr, stsErr)
	retryErr := errors.Join(stsErr, failoverErr)
	recordFleet(fleet, fleetObservation{
		desired: cfg.replicas, pods: pods, outdated: decision.Outdated, conditions: status.Conditions,
		maxDrainSeconds: cfg.maxDrainSeconds, now: r.now(),
	})
	if !equalJSON(&fsc.Status, status) {
		fsc.Status = *status
		if err := r.Status().Update(ctx, &fsc); err != nil {
			if apierrors.IsConflict(err) {
				if retryErr != nil {
					// Retried on that error (and logged), which writes the status again too.
					return ctrl.Result{}, retryErr
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, errors.Join(err, retryErr)
		}
	}
	if retryErr != nil {
		return ctrl.Result{}, retryErr
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// setWriteProblem marks the fleet Degraded and not Ready when its configuration is unusable or the API server
// refused its StatefulSet; the configuration counts first.
func setWriteProblem(conds *[]metav1.Condition, gen int64, confErr, stsErr error) {
	reason, msg := "ConfigInvalid", ""
	switch {
	case confErr != nil:
		msg = confErr.Error()
	case stsErr != nil:
		reason, msg = "StatefulSetRejected", truncate(stsErr.Error(), 1024)
	default:
		return
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionTrue, Reason: reason, Message: msg, ObservedGeneration: gen})
	meta.SetStatusCondition(conds, metav1.Condition{Type: conditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: gen})
}

// syncPods reads the pods' nodes, and writes on each pod its external address and whether it has been ready. It
// returns the nodes (podNodes), for the fleet's health.
func (r *FreeswitchClusterReconciler) syncPods(ctx context.Context, cfg settings, pods []corev1.Pod) (map[string]*corev1.Node, error) {
	nodes, err := podNodes(ctx, r.Client, pods)
	if err != nil {
		return nil, err
	}
	if err := syncExternalAddresses(ctx, r.Client, cfg.addressSource, cfg.addressPool, pods, nodes); err != nil {
		return nil, err
	}
	return nodes, markFirstReady(ctx, r.Client, pods, r.now())
}

func (r *FreeswitchClusterReconciler) now() time.Time { return nowFrom(r.Now) }

// nowFrom is the time from a clock that may be nil, meaning time.Now.
func nowFrom(clock func() time.Time) time.Time {
	if clock != nil {
		return clock()
	}
	return time.Now()
}

func (r *FreeswitchClusterReconciler) ensureScriptsConfigMap(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster) error {
	want := buildScriptsConfigMap(fsc)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = want.Labels
		cm.Data = want.Data
		return controllerutil.SetControllerReference(fsc, cm, r.Scheme)
	})
	return err
}

func (r *FreeswitchClusterReconciler) ensureService(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster) error {
	want := buildService(fsc)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = want.Labels
		if svc.CreationTimestamp.IsZero() {
			svc.Spec.ClusterIP = corev1.ClusterIPNone // immutable after creation
		}
		svc.Spec.Selector = want.Spec.Selector
		svc.Spec.Ports = want.Spec.Ports
		svc.Spec.PublishNotReadyAddresses = want.Spec.PublishNotReadyAddresses
		return controllerutil.SetControllerReference(fsc, svc, r.Scheme)
	})
	return err
}

// ensureStatefulSet writes the StatefulSet. The pod template is replaced only when the operator's rendering of it
// changes (tracked by a hash annotation), so API-server defaulting never causes an update loop.
func (r *FreeswitchClusterReconciler) ensureStatefulSet(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, conf configLayout) (*appsv1.StatefulSet, error) {
	want := buildStatefulSet(fsc, conf)
	hash, err := templateHash(&want.Spec.Template)
	if err != nil {
		return nil, err
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = want.Labels
		if sts.CreationTimestamp.IsZero() {
			// Immutable after creation.
			sts.Spec.Selector = want.Spec.Selector
			sts.Spec.ServiceName = want.Spec.ServiceName
			sts.Spec.PodManagementPolicy = want.Spec.PodManagementPolicy
		}
		sts.Spec.Replicas = want.Spec.Replicas
		sts.Spec.UpdateStrategy = want.Spec.UpdateStrategy
		if sts.Annotations[templateHashAnnotation] != hash {
			sts.Spec.Template = want.Spec.Template
			if sts.Annotations == nil {
				sts.Annotations = map[string]string{}
			}
			sts.Annotations[templateHashAnnotation] = hash
		}
		return controllerutil.SetControllerReference(fsc, sts, r.Scheme)
	})
	return sts, err
}

// loadConfig reads the referenced ConfigMaps and lays them out (see layoutConfig). A missing ConfigMap is a
// *configError, not a failed reconcile; the ConfigMap watch brings the cluster back when it appears.
func (r *FreeswitchClusterReconciler) loadConfig(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster) (configLayout, error) {
	cms := map[string]*corev1.ConfigMap{}
	for _, ref := range fsc.Spec.Config.ConfigMapRefs {
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Namespace: fsc.Namespace, Name: ref.Name}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return configLayout{}, err
		}
		cms[ref.Name] = &cm
	}
	return layoutConfig(fsc, cms)
}

// ensureConfigSnapshots creates the immutable copies the template is about to mount. A snapshot's name is its
// contents' hash, so one that already exists already holds the right files.
func (r *FreeswitchClusterReconciler) ensureConfigSnapshots(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, conf configLayout) error {
	for _, snap := range conf.snapshots {
		var existing corev1.ConfigMap
		err := r.Get(ctx, types.NamespacedName{Namespace: fsc.Namespace, Name: snap.name}, &existing)
		if err == nil {
			if existing.DeletionTimestamp != nil {
				return fmt.Errorf("config snapshot %s is being deleted; will recreate it", snap.name)
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		labels := commonLabels(fsc)
		labels[configSnapshotLabel] = configSnapshotValue
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        snap.name,
				Namespace:   fsc.Namespace,
				Labels:      labels,
				Annotations: map[string]string{configSourceAnnotation: snap.source},
			},
			Immutable:  ptr.To(true),
			Data:       snap.data,
			BinaryData: snap.binaryData,
		}
		if err := controllerutil.SetControllerReference(fsc, cm, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func templateHash(t *corev1.PodTemplateSpec) (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// syncExternalAddresses writes each pod's external address annotation: its node's ExternalIP, or the address the
// fleet's SIPAddressPool has bound to its node, with the bind address when it has one. FreeSWITCH and Kamailio pods
// both read them from their downward-API volume. nodes are the pods' nodes by name (podNodes), read for NodeExternalIP.
func syncExternalAddresses(ctx context.Context, c client.Client, src sipv1alpha1.ExternalAddressSource, poolName string, pods []corev1.Pod, nodes map[string]*corev1.Node) error {
	var pool sipv1alpha1.SIPAddressPool
	if src == sipv1alpha1.ExternalAddressAddressPool {
		// A missing pool binds nothing, so every pod goes without.
		if err := c.Get(ctx, types.NamespacedName{Name: poolName}, &pool); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
			continue
		}
		want, wantBind := "", ""
		switch src {
		case sipv1alpha1.ExternalAddressNodeExternalIP:
			node := nodes[p.Spec.NodeName]
			if node == nil {
				continue
			}
			want = nodeExternalAddress(node)
		case sipv1alpha1.ExternalAddressAddressPool:
			// The pool hands out an address with a bind address only once the node has it on an interface.
			if b := addresspool.BindingForNode(&pool.Status, p.Spec.NodeName); b != nil {
				want, wantBind = b.Address, b.BindAddress
			}
		}
		if err := setAddressAnnotations(ctx, c, p, want, wantBind); err != nil {
			return err
		}
	}
	return nil
}

// setAddressAnnotations sets a pod's external and bind address annotations, removing those that are empty.
func setAddressAnnotations(ctx context.Context, c client.Client, p *corev1.Pod, external, bind string) error {
	same := func(key, want string) bool {
		have, present := p.Annotations[key]
		return have == want && (present || want == "")
	}
	if same(sipv1alpha1.ExternalAddressAnnotation, external) && same(sipv1alpha1.BindAddressAnnotation, bind) {
		return nil
	}
	patch := client.MergeFrom(p.DeepCopy())
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	for key, want := range map[string]string{sipv1alpha1.ExternalAddressAnnotation: external, sipv1alpha1.BindAddressAnnotation: bind} {
		if want == "" {
			delete(p.Annotations, key)
		} else {
			p.Annotations[key] = want
		}
	}
	if err := c.Patch(ctx, p, patch); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// fleetsUsingPool maps a SIPAddressPool to the fleets of one kind (listed into newList) that draw from it.
func fleetsUsingPool(c client.Reader, newList func() client.ObjectList) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		list := newList()
		if err := c.List(ctx, list); err != nil {
			return nil
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, item := range items {
			if f, ok := item.(client.Object); ok && addressPoolName(externalAddressOf(f)) == o.GetName() {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(f)})
			}
		}
		return out
	})
}

// externalAddressOf is a fleet's external address spec.
func externalAddressOf(o client.Object) sipv1alpha1.ExternalAddressSpec {
	switch f := o.(type) {
	case *sipv1alpha1.FreeswitchCluster:
		return f.Spec.ExternalAddress
	case *sipv1alpha1.KamailioCluster:
		return f.Spec.ExternalAddress
	}
	return sipv1alpha1.ExternalAddressSpec{}
}

// syncDispatcher renders the dispatcher list from the ready endpoints, writes it when it changes, takes leaving
// nodes out of a running Kamailio at once, and asks Kamailio to reload once the kubelet has had time to update
// the mounted file. It returns how long to wait before the reload is due, or zero.
func (r *FreeswitchClusterReconciler) syncDispatcher(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, fleet fleetRef, cfg settings, status *sipv1alpha1.FreeswitchClusterStatus, pods []corev1.Pod, nodes map[string]*corev1.Node) (time.Duration, error) {
	log := logf.FromContext(ctx)
	d := cfg.dispatcher

	var endpointSlices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &endpointSlices, client.InNamespace(fsc.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: serviceName(fsc)}); err != nil {
		return 0, err
	}
	dests := readyDestinations(endpointSlices.Items, cfg.sipPort, string(cfg.dispatcherProto))
	content := renderDispatcherList(cfg.dispatcherSetID, dests)
	hash := contentHash(content)
	recordDestinations(fleet, map[int32]int{cfg.dispatcherSetID: len(dests)})

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: d.ConfigMapName, Namespace: fsc.Namespace}}
	var previous string
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err == nil {
		previous = cm.Data[dispatcherListKey]
	} else if !apierrors.IsNotFound(err) {
		return 0, err
	}

	// One call per ready Kamailio pod when the URL names a Service; see dispatcherNotifier. Failing to find them is
	// handled like a failed call: logged, and the reload retried.
	notifier, notifierErr := r.dispatcherNotifier(ctx, fsc.Namespace, d.JSONRPCURL)
	if notifierErr != nil {
		log.Error(notifierErr, "could not find the Kamailio pods to notify", "jsonrpcURL", d.JSONRPCURL)
	}

	// A pod recovering its calls on start re-invites them before it is ready, so before the list names it, and
	// Kamailio's trusted-peer check would drop them. The starting pods' addresses go on the ConfigMap, for the
	// KamailioClusters dispatching to this fleet, and to the Kamailio notified here; see startingHosts.
	var starting []string
	if cfg.recovery {
		starting = startingHosts(pods, nodes)
	}
	startingList := strings.Join(starting, ",")

	// The ConfigMap is the record of what changed when: the list, its hash and the change time go in one update,
	// and the status only mirrors them. A status write lost to a conflict then can't hide a change that still
	// needs a reload (the next reconcile would see an up-to-date ConfigMap and nothing to do).
	listChanged := previous != content || cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation] == ""
	if listChanged || cm.Annotations[sipv1alpha1.StartingAddressesAnnotation] != startingList {
		changedAt := cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation]
		if listChanged {
			changedAt = r.now().UTC().Format(time.RFC3339Nano)
		}
		if err := r.writeDispatcherList(ctx, fsc, cm, content, hash, changedAt, startingList); err != nil {
			return 0, err
		}
	}
	if listChanged {
		log.Info("dispatcher list updated", "destinations", len(dests), "hash", hash)
		dispatcherListUpdates.WithLabelValues(fleet.labels()...).Inc()

		if notifier != nil {
			deactivateRemoved(ctx, notifier, fleet, cfg.dispatcherSetID, previous, content)
		}
	}
	if notifier != nil && len(starting) > 0 {
		// Admitted again on each look while the pods start; the entries outlive the start (kamailioAdmittedTable).
		if err := notifier.Admit(ctx, starting); err != nil {
			log.Error(err, "could not admit starting pods' addresses to Kamailio", "addresses", starting)
		}
	}
	status.DispatcherHash = hash
	status.DispatcherUpdatedAt = nil
	if t, err := time.Parse(time.RFC3339Nano, cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation]); err == nil {
		at := metav1.NewTime(t)
		status.DispatcherUpdatedAt = &at
	}

	// Without a Kamailio to notify, no reload is ever due. Every return below leaves the status saying whether
	// one still is, and a reload that fails is retried, which sets the gauge again.
	notifies := notifier != nil || notifierErr != nil
	defer func() { recordPendingReload(fleet, notifies && status.DispatcherReloadedHash != status.DispatcherHash) }()
	if !notifies || status.DispatcherReloadedHash == status.DispatcherHash {
		return 0, nil
	}
	if notifier != nil {
		// Best effort: the reload below covers these too, only later.
		if err := notifier.Admit(ctx, dispatcherHosts(parseDispatcherURIs(content, cfg.dispatcherSetID))); err != nil {
			log.Error(err, "could not admit the fleet's addresses to Kamailio ahead of the reload")
		}
	}
	delay := time.Duration(cfg.reloadDelaySecs) * time.Second
	if status.DispatcherUpdatedAt != nil {
		if wait := delay - r.now().Sub(status.DispatcherUpdatedAt.Time); wait > 0 {
			return wait, nil
		}
	}
	if notifierErr != nil {
		return 30 * time.Second, nil
	}
	// The reload is done only once every Kamailio holds the list just written: reloadDelaySeconds is a guess at
	// how long the kubelet takes to update the mounted file, and a reload before that re-reads the old one.
	err := notifier.Reload(ctx, map[int32][]string{cfg.dispatcherSetID: parseDispatcherURIs(content, cfg.dispatcherSetID)})
	recordReload(fleet, err)
	if err != nil {
		if onlyListNotLoaded(err) {
			log.Info("Kamailio has not loaded the new dispatcher list yet; will reload again", "detail", err.Error())
			return 10 * time.Second, nil
		}
		log.Error(err, "could not reload Kamailio dispatcher list; will retry")
		return 30 * time.Second, nil
	}
	status.DispatcherReloadedHash = status.DispatcherHash
	return 0, nil
}

// writeDispatcherList writes the list, its hash, when it changed and the starting pods' addresses to the
// dispatcher ConfigMap, in one update.
func (r *FreeswitchClusterReconciler) writeDispatcherList(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, cm *corev1.ConfigMap, content, hash, changedAt, startingList string) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		maps.Copy(cm.Labels, commonLabels(fsc))
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations[sipv1alpha1.DispatcherHashAnnotation] = hash
		cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation] = changedAt
		if startingList != "" {
			cm.Annotations[sipv1alpha1.StartingAddressesAnnotation] = startingList
		} else {
			delete(cm.Annotations, sipv1alpha1.StartingAddressesAnnotation)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[dispatcherListKey] = content
		return controllerutil.SetControllerReference(fsc, cm, r.Scheme)
	})
	return err
}

// deactivateRemoved takes the destinations that left the list out of the running Kamailio at once.
func deactivateRemoved(ctx context.Context, notifier DispatcherNotifier, fleet fleetRef, setID int32, previous, content string) {
	for _, uri := range removedURIs(parseDispatcherURIs(previous, setID), parseDispatcherURIs(content, setID)) {
		err := notifier.SetInactive(ctx, setID, uri)
		recordSetInactive(fleet, err)
		if err != nil {
			// Kamailio's own probing and the paused node's 503s still protect calls; keep going.
			logf.FromContext(ctx).Error(err, "could not mark destination inactive", "uri", uri)
		}
	}
}

// onlyListNotLoaded reports whether err, or every error joined in it, is errListNotLoaded: Kamailio answered, but
// its file was not updated yet.
func onlyListNotLoaded(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !onlyListNotLoaded(e) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, errListNotLoaded)
}

func (r *FreeswitchClusterReconciler) fillStatus(fsc *sipv1alpha1.FreeswitchCluster, cfg settings, sts *appsv1.StatefulSet, pods []corev1.Pod, decision rolloutDecision, status *sipv1alpha1.FreeswitchClusterStatus) {
	members, ready, updated := fleetMembers(pods, sts.Status.UpdateRevision)
	status.Members = make([]sipv1alpha1.FreeswitchMember, len(members))
	for i, m := range members {
		status.Members[i] = sipv1alpha1.FreeswitchMember{Member: m}
	}
	status.Replicas = int32(len(pods))
	status.ReadyReplicas = ready
	status.UpdatedReplicas = updated
	status.CurrentRevision = sts.Status.CurrentRevision
	status.UpdateRevision = sts.Status.UpdateRevision
	setFleetConditions(&status.Conditions, fsc.Generation, ready, cfg.replicas, decision)
}

func equalJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// indexFleetPodsByNode indexes FreeSWITCH and Kamailio pods by node, so a node's change reaches the fleets on it.
// Both controllers use it; it is registered once.
func indexFleetPodsByNode(ctx context.Context, mgr ctrl.Manager) error {
	podIndexOnce.Do(func() {
		podIndexErr = mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, podNodeNameField, func(o client.Object) []string {
			p := o.(*corev1.Pod)
			if p.Spec.NodeName == "" || p.Labels[sipv1alpha1.ClusterLabel] == "" && p.Labels[sipv1alpha1.KamailioClusterLabel] == "" {
				return nil
			}
			return []string{p.Spec.NodeName}
		})
	})
	return podIndexErr
}

var (
	podIndexOnce sync.Once
	podIndexErr  error
)

// SetupWithManager sets up the controller with the Manager.
func (r *FreeswitchClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	if err := indexFleetPodsByNode(ctx, mgr); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(ctx, &sipv1alpha1.FreeswitchCluster{}, configMapRefField, func(o client.Object) []string {
		fsc := o.(*sipv1alpha1.FreeswitchCluster)
		names := make([]string, 0, len(fsc.Spec.Config.ConfigMapRefs))
		for _, ref := range fsc.Spec.Config.ConfigMapRefs {
			names = append(names, ref.Name)
		}
		return names
	}); err != nil {
		return err
	}

	// A referenced ConfigMap's change (or its creation, when a cluster waits for it) reaches every cluster using it.
	configToClusters := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var list sipv1alpha1.FreeswitchClusterList
		if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{configMapRefField: o.GetName()}); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, fsc := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: fsc.Namespace, Name: fsc.Name}})
		}
		return out
	})

	podToCluster := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		name := o.GetLabels()[sipv1alpha1.ClusterLabel]
		if name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
	})

	sliceToCluster := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		svc := o.GetLabels()[discoveryv1.LabelServiceName]
		if svc == "" {
			return nil
		}
		key := types.NamespacedName{Namespace: o.GetNamespace(), Name: svc}
		var fsc sipv1alpha1.FreeswitchCluster
		if err := r.Get(ctx, key, &fsc); err != nil {
			return nil
		}
		return []reconcile.Request{{NamespacedName: key}}
	})

	nodeToClusters := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.MatchingFields{podNodeNameField: o.GetName()}); err != nil {
			return nil
		}
		seen := map[types.NamespacedName]bool{}
		var out []reconcile.Request
		for _, p := range pods.Items {
			key := types.NamespacedName{Namespace: p.Namespace, Name: p.Labels[sipv1alpha1.ClusterLabel]}
			if key.Name == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, reconcile.Request{NamespacedName: key})
		}
		return out
	})

	b := ctrl.NewControllerManagedBy(mgr)
	if r.ProbeEvents != nil {
		b = b.WatchesRawSource(source.Channel(r.ProbeEvents, &handler.EnqueueRequestForObject{}))
	}
	return b.
		For(&sipv1alpha1.FreeswitchCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Pod{}, podToCluster).
		Watches(&discoveryv1.EndpointSlice{}, sliceToCluster).
		Watches(&corev1.Node{}, nodeToClusters).
		Watches(&corev1.ConfigMap{}, configToClusters).
		Watches(&sipv1alpha1.SIPAddressPool{}, fleetsUsingPool(mgr.GetClient(), func() client.ObjectList { return &sipv1alpha1.FreeswitchClusterList{} })).
		Named("freeswitchcluster").
		Complete(r)
}
