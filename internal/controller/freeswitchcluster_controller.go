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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
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
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	configHashes configHasher
}

// +kubebuilder:rbac:groups=sip.instruments.com,resources=freeswitchclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sip.instruments.com,resources=freeswitchclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sip.instruments.com,resources=freeswitchclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// Reconcile moves a fleet toward its spec: the objects around the pods, each pod's external address, one planned
// replacement at a time, the dispatcher list, and the status.
func (r *FreeswitchClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fsc sipv1alpha1.FreeswitchCluster
	if err := r.Get(ctx, req.NamespacedName, &fsc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if fsc.DeletionTimestamp != nil {
		// Owner references let the garbage collector remove everything; each pod still drains in preStop.
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
	if err := r.ensurePDB(ctx, &fsc); err != nil {
		return ctrl.Result{}, err
	}
	// A missing or conflicting ConfigMap is the user's to fix: report it, and keep the running fleet as it is,
	// with its template unchanged and any roll in progress paused.
	conf, confErr := r.loadConfig(ctx, &fsc)
	sts := &appsv1.StatefulSet{}
	if confErr != nil {
		var ce *configError
		if !errors.As(confErr, &ce) {
			return ctrl.Result{}, confErr
		}
		log.Info("configuration not usable; leaving the StatefulSet as it is", "reason", ce.msg)
		if err := r.Get(ctx, types.NamespacedName{Namespace: fsc.Namespace, Name: statefulSetName(&fsc)}, sts); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	} else {
		if err := r.ensureConfigSnapshots(ctx, &fsc, conf); err != nil {
			return ctrl.Result{}, err
		}
		var err error
		if sts, err = r.ensureStatefulSet(ctx, &fsc, conf); err != nil {
			return ctrl.Result{}, err
		}
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(fsc.Namespace), client.MatchingLabels(selectorLabels(&fsc))); err != nil {
		return ctrl.Result{}, err
	}
	pods := podList.Items
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })

	if err := r.syncExternalAddresses(ctx, cfg, pods); err != nil {
		return ctrl.Result{}, err
	}

	if confErr == nil {
		if err := r.pruneConfigSnapshots(ctx, &fsc, conf, sts, pods); err != nil {
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
	if decision.Replace != nil {
		p := decision.Replace
		log.Info("replacing pod for rolling update", "pod", p.Name, "revision", podRevision(p), "updateRevision", sts.Status.UpdateRevision)
		uid := p.UID
		if err := r.Delete(ctx, p, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if decision.Waiting != "" {
		log.V(1).Info("rolling update waiting", "reason", decision.Waiting, "outdated", decision.Outdated)
	}

	if cfg.dispatcher != nil {
		after, err := r.syncDispatcher(ctx, &fsc, cfg, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = after
	}

	r.fillStatus(&fsc, cfg, sts, pods, decision, status)
	if confErr != nil {
		gen := fsc.Generation
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionTrue, Reason: "ConfigInvalid", Message: confErr.Error(), ObservedGeneration: gen})
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionReady, Status: metav1.ConditionFalse, Reason: "ConfigInvalid", Message: confErr.Error(), ObservedGeneration: gen})
	}
	if !equalStatus(&fsc.Status, status) {
		fsc.Status = *status
		if err := r.Status().Update(ctx, &fsc); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *FreeswitchClusterReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
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

func (r *FreeswitchClusterReconciler) ensurePDB(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster) error {
	want := buildPDB(fsc)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = want.Labels
		pdb.Spec.MaxUnavailable = want.Spec.MaxUnavailable
		if pdb.CreationTimestamp.IsZero() {
			pdb.Spec.Selector = want.Spec.Selector
		}
		return controllerutil.SetControllerReference(fsc, pdb, r.Scheme)
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
	return layoutConfig(fsc, cms, r.configHashes.hash)
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
		labels[configSnapshotLabel] = "true"
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

// pruneConfigSnapshots deletes the snapshots that neither the current template nor any pod mounts. It waits until
// the StatefulSet controller has seen the current template, so no pod can still be created from an older one.
func (r *FreeswitchClusterReconciler) pruneConfigSnapshots(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, conf configLayout, sts *appsv1.StatefulSet, pods []corev1.Pod) error {
	if sts.CreationTimestamp.IsZero() || sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	keep := map[string]bool{}
	for _, snap := range conf.snapshots {
		keep[snap.name] = true
	}
	for i := range pods {
		podConfigSnapshots(&pods[i], keep)
	}
	var list corev1.ConfigMapList
	if err := r.List(ctx, &list, client.InNamespace(fsc.Namespace), client.MatchingLabels{sipv1alpha1.ClusterLabel: fsc.Name, configSnapshotLabel: "true"}); err != nil {
		return err
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if keep[cm.Name] || cm.DeletionTimestamp != nil || !metav1.IsControlledBy(cm, fsc) {
			continue
		}
		uid := cm.UID
		if err := r.Delete(ctx, cm, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		logf.FromContext(ctx).Info("deleted unused config snapshot", "configMap", cm.Name)
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

// syncExternalAddresses writes each pod's external address annotation from its node.
func (r *FreeswitchClusterReconciler) syncExternalAddresses(ctx context.Context, cfg settings, pods []corev1.Pod) error {
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		want := ""
		if cfg.addressSource == sipv1alpha1.ExternalAddressNodeExternalIP {
			if p.Spec.NodeName == "" {
				continue
			}
			var node corev1.Node
			if err := r.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, &node); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return err
			}
			want = nodeExternalAddress(&node)
		}
		have, present := p.Annotations[sipv1alpha1.ExternalAddressAnnotation]
		if have == want && (present || want == "") {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		if want == "" {
			delete(p.Annotations, sipv1alpha1.ExternalAddressAnnotation)
		} else {
			p.Annotations[sipv1alpha1.ExternalAddressAnnotation] = want
		}
		if err := r.Patch(ctx, p, patch); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// syncDispatcher renders the dispatcher list from the ready endpoints, writes it when it changes, takes leaving
// nodes out of a running Kamailio at once, and asks Kamailio to reload once the kubelet has had time to update
// the mounted file. It returns how long to wait before the reload is due, or zero.
func (r *FreeswitchClusterReconciler) syncDispatcher(ctx context.Context, fsc *sipv1alpha1.FreeswitchCluster, cfg settings, status *sipv1alpha1.FreeswitchClusterStatus) (time.Duration, error) {
	log := logf.FromContext(ctx)
	d := cfg.dispatcher

	var endpointSlices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &endpointSlices, client.InNamespace(fsc.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: serviceName(fsc)}); err != nil {
		return 0, err
	}
	dests := readyDestinations(endpointSlices.Items, cfg.sipPort, string(cfg.dispatcherProto))
	content := renderDispatcherList(cfg.dispatcherSetID, dests)
	hash := contentHash(content)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: d.ConfigMapName, Namespace: fsc.Namespace}}
	var previous string
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err == nil {
		previous = cm.Data[dispatcherListKey]
	} else if !apierrors.IsNotFound(err) {
		return 0, err
	}

	var notifier DispatcherNotifier
	if d.JSONRPCURL != "" && r.NewNotifier != nil {
		notifier = r.NewNotifier(d.JSONRPCURL)
	}

	// The ConfigMap is the record of what changed when: the list, its hash and the change time go in one update,
	// and the status only mirrors them. A status write lost to a conflict then can't hide a change that still
	// needs a reload (the next reconcile would see an up-to-date ConfigMap and nothing to do).
	if previous != content || cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation] == "" {
		changedAt := r.now().UTC().Format(time.RFC3339Nano)
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
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[dispatcherListKey] = content
			return controllerutil.SetControllerReference(fsc, cm, r.Scheme)
		})
		if err != nil {
			return 0, err
		}
		log.Info("dispatcher list updated", "destinations", len(dests), "hash", hash)

		if notifier != nil {
			for _, uri := range removedURIs(parseDispatcherURIs(previous, cfg.dispatcherSetID), parseDispatcherURIs(content, cfg.dispatcherSetID)) {
				if err := notifier.SetInactive(ctx, cfg.dispatcherSetID, uri); err != nil {
					// Kamailio's own probing and the paused node's 503s still protect calls; keep going.
					log.Error(err, "could not mark destination inactive", "uri", uri)
				}
			}
		}
	}
	status.DispatcherHash = hash
	status.DispatcherUpdatedAt = nil
	if t, err := time.Parse(time.RFC3339Nano, cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation]); err == nil {
		at := metav1.NewTime(t)
		status.DispatcherUpdatedAt = &at
	}

	if notifier == nil || status.DispatcherReloadedHash == status.DispatcherHash {
		return 0, nil
	}
	delay := time.Duration(cfg.reloadDelaySecs) * time.Second
	if status.DispatcherUpdatedAt != nil {
		if wait := delay - r.now().Sub(status.DispatcherUpdatedAt.Time); wait > 0 {
			return wait, nil
		}
	}
	if err := notifier.Reload(ctx); err != nil {
		log.Error(err, "could not reload Kamailio dispatcher list; will retry")
		return 30 * time.Second, nil
	}
	status.DispatcherReloadedHash = status.DispatcherHash
	return 0, nil
}

func (r *FreeswitchClusterReconciler) fillStatus(fsc *sipv1alpha1.FreeswitchCluster, cfg settings, sts *appsv1.StatefulSet, pods []corev1.Pod, decision rolloutDecision, status *sipv1alpha1.FreeswitchClusterStatus) {
	members := make([]sipv1alpha1.Member, 0, len(pods))
	var ready, updated int32
	for i := range pods {
		p := &pods[i]
		m := sipv1alpha1.Member{
			Pod:             p.Name,
			Node:            p.Spec.NodeName,
			Address:         p.Status.PodIP,
			ExternalAddress: p.Annotations[sipv1alpha1.ExternalAddressAnnotation],
			State:           memberState(p),
			Revision:        podRevision(p),
		}
		if m.State == sipv1alpha1.MemberActive {
			ready++
		}
		if sts.Status.UpdateRevision != "" && m.Revision == sts.Status.UpdateRevision {
			updated++
		}
		members = append(members, m)
	}
	status.Members = members
	status.Replicas = int32(len(pods))
	status.ReadyReplicas = ready
	status.UpdatedReplicas = updated
	status.CurrentRevision = sts.Status.CurrentRevision
	status.UpdateRevision = sts.Status.UpdateRevision

	gen := fsc.Generation
	if ready > 0 {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionAvailable, Status: metav1.ConditionTrue, Reason: "MembersReady", Message: fmt.Sprintf("%d of %d members take calls", ready, cfg.replicas), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionAvailable, Status: metav1.ConditionFalse, Reason: "NoMembersReady", Message: "no member takes calls", ObservedGeneration: gen})
	}
	if decision.Outdated > 0 {
		msg := fmt.Sprintf("%d members on an older revision", decision.Outdated)
		if decision.Waiting != "" {
			msg += "; " + decision.Waiting
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionProgressing, Status: metav1.ConditionTrue, Reason: "RollingUpdate", Message: msg, ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionProgressing, Status: metav1.ConditionFalse, Reason: "UpToDate", Message: "every member runs the latest revision", ObservedGeneration: gen})
	}
	if ready < cfg.replicas {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionTrue, Reason: "MembersNotReady", Message: fmt.Sprintf("%d of %d members ready", ready, cfg.replicas), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionFalse, Reason: "AllMembersReady", Message: "every member is ready", ObservedGeneration: gen})
	}
	// Ready is the summary GitOps tools (Flux health checks, kstatus) wait on: the whole fleet ready and current.
	if ready >= cfg.replicas && decision.Outdated == 0 {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionReady, Status: metav1.ConditionTrue, Reason: "FleetReady", Message: "every member is ready and current", ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionReady, Status: metav1.ConditionFalse, Reason: "FleetNotReady", Message: fmt.Sprintf("%d of %d members ready, %d outdated", ready, cfg.replicas, decision.Outdated), ObservedGeneration: gen})
	}
}

func equalStatus(a, b *sipv1alpha1.FreeswitchClusterStatus) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// SetupWithManager sets up the controller with the Manager.
func (r *FreeswitchClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, podNodeNameField, func(o client.Object) []string {
		p := o.(*corev1.Pod)
		if p.Spec.NodeName == "" || p.Labels[sipv1alpha1.ClusterLabel] == "" {
			return nil
		}
		return []string{p.Spec.NodeName}
	}); err != nil {
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&sipv1alpha1.FreeswitchCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&corev1.Pod{}, podToCluster).
		Watches(&discoveryv1.EndpointSlice{}, sliceToCluster).
		Watches(&corev1.Node{}, nodeToClusters).
		Watches(&corev1.ConfigMap{}, configToClusters).
		Named("freeswitchcluster").
		Complete(r)
}
