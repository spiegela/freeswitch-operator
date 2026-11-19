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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	dispatchClusterField = "spec.dispatch.freeswitchCluster"
	conditionSetConflict = "SetConflict"
)

// KamailioClusterReconciler reconciles a KamailioCluster object.
type KamailioClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewNotifier builds the JSON-RPC notifier for one Kamailio pod. Nil disables notification.
	NewNotifier NotifierFactory
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=sip.instruments.com,resources=kamailioclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sip.instruments.com,resources=kamailioclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sip.instruments.com,resources=kamailioclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile moves a proxy fleet toward its spec: the JSON-RPC token, the rendered config and the objects around
// the pods, one planned replacement at a time, the combined dispatcher list and its live reload, and the status.
func (r *KamailioClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	fleet := fleetRef{kind: kindKamailio, namespace: req.Namespace, name: req.Name}
	var kc sipv1alpha1.KamailioCluster
	if err := r.Get(ctx, req.NamespacedName, &kc); err != nil {
		if apierrors.IsNotFound(err) {
			forgetFleet(fleet)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if kc.DeletionTimestamp != nil {
		forgetFleet(fleet)
		return ctrl.Result{}, nil
	}
	cfg := resolveKamailio(&kc)
	status := kc.Status.DeepCopy()
	status.ObservedGeneration = kc.Generation

	token, err := r.ensureToken(ctx, &kc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureScripts(ctx, &kc); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, &kc); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensurePDB(ctx, &kc); err != nil {
		return ctrl.Result{}, err
	}

	sets, problems, err := r.loadSets(ctx, &kc)
	if err != nil {
		return ctrl.Result{}, err
	}
	setIDs := make([]int32, len(sets))
	for i, s := range sets {
		setIDs[i] = s.setID
	}
	rendered, err := renderKamailioConfig(kamailioConfigData{
		Name:           kc.Name,
		SIPPort:        cfg.sipPort,
		RPCPort:        cfg.rpcPort,
		MetricsPort:    cfg.metricsPort,
		DispatcherList: kamailioDispatcher,
		Sets:           setIDs,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	configHash := contentHash(rendered)
	configName := kamailioConfigName(&kc, configHash)
	if err := r.ensureConfigSnapshot(ctx, &kc, configName, rendered); err != nil {
		return ctrl.Result{}, err
	}
	sts, err := r.ensureStatefulSet(ctx, &kc, configName, configHash)
	if err != nil {
		return ctrl.Result{}, err
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(kc.Namespace), client.MatchingLabels(kamailioSelectorLabels(&kc))); err != nil {
		return ctrl.Result{}, err
	}
	pods := podList.Items
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })

	awaiting, err := syncExternalAddresses(ctx, r.Client, cfg.addressSource, cfg.addressPool, pods)
	if err != nil {
		return ctrl.Result{}, err
	}
	fleetAwaitingAddressAgent.WithLabelValues(fleet.labels()...).Set(float64(awaiting))
	if err := r.pruneConfigSnapshots(ctx, &kc, configName, sts, pods); err != nil {
		return ctrl.Result{}, err
	}

	decision := nextPodToReplace(pods, sts.Status.UpdateRevision, cfg.replicas)
	if p := decision.Replace; p != nil {
		log.Info("replacing Kamailio pod for rolling update", "pod", p.Name, "revision", podRevision(p), "updateRevision", sts.Status.UpdateRevision)
		uid := p.UID
		if err := r.Delete(ctx, p, client.Preconditions{UID: &uid}); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else {
			fleetPodReplacements.WithLabelValues(fleet.labels()...).Inc()
		}
	} else if decision.Waiting != "" {
		log.V(1).Info("rolling update waiting", "reason", decision.Waiting, "outdated", decision.Outdated)
	}

	requeue, err := r.syncDispatcher(ctx, &kc, fleet, cfg, token, sets, pods, status)
	if err != nil {
		return ctrl.Result{}, err
	}

	members, ready, updated := fleetMembers(pods, sts.Status.UpdateRevision)
	status.Members = members
	status.Replicas = int32(len(pods))
	status.ReadyReplicas = ready
	status.UpdatedReplicas = updated
	status.CurrentRevision = sts.Status.CurrentRevision
	status.UpdateRevision = sts.Status.UpdateRevision
	setFleetConditions(&status.Conditions, kc.Generation, ready, cfg.replicas, decision)
	if len(problems) > 0 {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionSetConflict, Status: metav1.ConditionTrue, Reason: "FleetLeftOut", Message: strings.Join(problems, "; "), ObservedGeneration: kc.Generation})
	} else {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: conditionSetConflict, Status: metav1.ConditionFalse, Reason: "AllFleetsDispatched", Message: "every fleet in spec.dispatch is in the dispatcher list", ObservedGeneration: kc.Generation})
	}
	recordFleet(fleet, fleetObservation{
		desired: cfg.replicas, pods: pods, outdated: decision.Outdated, conditions: status.Conditions,
		maxDrainSeconds: cfg.maxDrainSeconds, now: r.now(),
	})

	if !equalJSON(&kc.Status, status) {
		kc.Status = *status
		if err := r.Status().Update(ctx, &kc); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *KamailioClusterReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// ensureToken creates the JSON-RPC token Secret once and returns the token. An existing token is kept, so pods
// and operator agree across operator restarts. Pods read it at start, so a replaced token reaches them as they
// roll; until then the operator's calls are refused and the reload retried.
func (r *KamailioClusterReconciler) ensureToken(ctx context.Context, kc *sipv1alpha1.KamailioCluster) (string, error) {
	key := types.NamespacedName{Namespace: kc.Namespace, Name: kamailioTokenName(kc)}
	var secret corev1.Secret
	err := r.Get(ctx, key, &secret)
	switch {
	case err == nil:
		if !metav1.IsControlledBy(&secret, kc) {
			return "", fmt.Errorf("secret %s exists and is not this KamailioCluster's", key)
		}
		if t := string(secret.Data[kamailioTokenKey]); t != "" {
			return t, nil
		}
	case !apierrors.IsNotFound(err):
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if apierrors.IsNotFound(err) {
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: kamailioLabels(kc)},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{kamailioTokenKey: []byte(token)},
		}
		if err := controllerutil.SetControllerReference(kc, &secret, r.Scheme); err != nil {
			return "", err
		}
		return token, r.Create(ctx, &secret)
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[kamailioTokenKey] = []byte(token)
	return token, r.Update(ctx, &secret)
}

func (r *KamailioClusterReconciler) ensureScripts(ctx context.Context, kc *sipv1alpha1.KamailioCluster) error {
	want := buildKamailioScriptsConfigMap(kc)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = want.Labels
		cm.Data = want.Data
		return controllerutil.SetControllerReference(kc, cm, r.Scheme)
	})
	return err
}

func (r *KamailioClusterReconciler) ensureService(ctx context.Context, kc *sipv1alpha1.KamailioCluster) error {
	want := buildKamailioService(kc)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = want.Labels
		if svc.CreationTimestamp.IsZero() {
			svc.Spec.ClusterIP = corev1.ClusterIPNone
		}
		svc.Spec.Selector = want.Spec.Selector
		svc.Spec.Ports = want.Spec.Ports
		return controllerutil.SetControllerReference(kc, svc, r.Scheme)
	})
	return err
}

func (r *KamailioClusterReconciler) ensurePDB(ctx context.Context, kc *sipv1alpha1.KamailioCluster) error {
	want := buildKamailioPDB(kc)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = want.Labels
		pdb.Spec.MaxUnavailable = want.Spec.MaxUnavailable
		if pdb.CreationTimestamp.IsZero() {
			pdb.Spec.Selector = want.Spec.Selector
		}
		return controllerutil.SetControllerReference(kc, pdb, r.Scheme)
	})
	return err
}

// ensureConfigSnapshot creates the immutable ConfigMap holding a rendered kamailio.cfg. Its name is the content's
// hash, so one that exists already holds the right file.
func (r *KamailioClusterReconciler) ensureConfigSnapshot(ctx context.Context, kc *sipv1alpha1.KamailioCluster, name, rendered string) error {
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: kc.Namespace, Name: name}, &existing)
	if err == nil {
		if existing.DeletionTimestamp != nil {
			return fmt.Errorf("config snapshot %s is being deleted; will recreate it", name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	labels := kamailioLabels(kc)
	labels[configSnapshotLabel] = configSnapshotValue
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kc.Namespace, Labels: labels},
		Immutable:  ptr.To(true),
		Data:       map[string]string{"kamailio.cfg": rendered},
	}
	if err := controllerutil.SetControllerReference(kc, cm, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// pruneConfigSnapshots deletes the config snapshots that neither the current template nor any pod mounts, once the
// StatefulSet controller has seen the current template.
func (r *KamailioClusterReconciler) pruneConfigSnapshots(ctx context.Context, kc *sipv1alpha1.KamailioCluster, current string, sts *appsv1.StatefulSet, pods []corev1.Pod) error {
	if sts.CreationTimestamp.IsZero() || sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	keep := map[string]bool{current: true}
	for i := range pods {
		for _, v := range pods[i].Spec.Volumes {
			if v.ConfigMap != nil {
				keep[v.ConfigMap.Name] = true
			}
		}
	}
	var list corev1.ConfigMapList
	if err := r.List(ctx, &list, client.InNamespace(kc.Namespace), client.MatchingLabels{sipv1alpha1.KamailioClusterLabel: kc.Name, configSnapshotLabel: configSnapshotValue}); err != nil {
		return err
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if keep[cm.Name] || cm.DeletionTimestamp != nil || !metav1.IsControlledBy(cm, kc) {
			continue
		}
		uid := cm.UID
		if err := r.Delete(ctx, cm, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		logf.FromContext(ctx).Info("deleted unused Kamailio config", "configMap", cm.Name)
	}
	return nil
}

// ensureStatefulSet writes the StatefulSet, replacing the pod template only when the operator's rendering of it
// changes (as for a FreeswitchCluster).
func (r *KamailioClusterReconciler) ensureStatefulSet(ctx context.Context, kc *sipv1alpha1.KamailioCluster, configName, configHash string) (*appsv1.StatefulSet, error) {
	want := buildKamailioStatefulSet(kc, configName, configHash)
	hash, err := templateHash(&want.Spec.Template)
	if err != nil {
		return nil, err
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = want.Labels
		if sts.CreationTimestamp.IsZero() {
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
		return controllerutil.SetControllerReference(kc, sts, r.Scheme)
	})
	return sts, err
}

// dispatchSet is one fleet's set, as its FreeswitchCluster publishes it.
type dispatchSet struct {
	cluster string
	setID   int32
	list    string // the fleet's own dispatcher.list
}

// loadSets reads each fleet in spec.dispatch, in order. A fleet that can't be used is left out and described in
// problems: missing, without spec.dispatcher, or with a setID an earlier fleet already has. One whose list isn't
// published yet is kept, with no destinations, so the config doesn't change when the list appears.
func (r *KamailioClusterReconciler) loadSets(ctx context.Context, kc *sipv1alpha1.KamailioCluster) (sets []dispatchSet, problems []string, err error) {
	byID := map[int32]string{}
	for _, d := range kc.Spec.Dispatch {
		var fsc sipv1alpha1.FreeswitchCluster
		if err := r.Get(ctx, types.NamespacedName{Namespace: kc.Namespace, Name: d.FreeswitchCluster}, &fsc); err != nil {
			if apierrors.IsNotFound(err) {
				problems = append(problems, fmt.Sprintf("FreeswitchCluster %s not found", d.FreeswitchCluster))
				continue
			}
			return nil, nil, err
		}
		if fsc.Spec.Dispatcher == nil {
			problems = append(problems, fmt.Sprintf("FreeswitchCluster %s has no spec.dispatcher", d.FreeswitchCluster))
			continue
		}
		id := resolve(&fsc).dispatcherSetID
		if other, taken := byID[id]; taken {
			problems = append(problems, fmt.Sprintf("FreeswitchCluster %s uses setID %d, as %s does; left out", d.FreeswitchCluster, id, other))
			continue
		}
		byID[id] = d.FreeswitchCluster
		s := dispatchSet{cluster: d.FreeswitchCluster, setID: id}
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Namespace: kc.Namespace, Name: fsc.Spec.Dispatcher.ConfigMapName}, &cm); err == nil {
			// Only the FreeswitchCluster's own list: a ConfigMap of that name it doesn't control isn't its.
			if metav1.IsControlledBy(&cm, &fsc) {
				s.list = cm.Data[dispatcherListKey]
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, nil, err
		}
		sets = append(sets, s)
	}
	return sets, problems, nil
}

// combineDispatcherLists renders the proxy fleet's dispatcher.list: each set's lines from its fleet's own list, in
// priority order, with every destination flagged for probing. It also returns the destinations in each set.
func combineDispatcherLists(sets []dispatchSet) (string, map[int32][]string) {
	var b strings.Builder
	b.WriteString("# Generated by freeswitch-operator from the FreeswitchClusters in spec.dispatch. Do not edit.\n")
	b.WriteString("# setid destination flags priority attributes\n")
	uris := map[int32][]string{}
	for _, s := range sets {
		uris[s.setID] = []string{}
		fmt.Fprintf(&b, "# set %d: FreeswitchCluster %s\n", s.setID, s.cluster)
		for line := range strings.SplitSeq(s.list, "\n") {
			f := strings.Fields(line)
			if len(f) < 2 || f[0] != strconv.Itoa(int(s.setID)) {
				continue
			}
			for len(f) < 5 {
				f = append(f, "0")
			}
			f[2] = kamailioProbingFlag
			b.WriteString(strings.Join(f, " ") + "\n")
			uris[s.setID] = append(uris[s.setID], f[1])
		}
	}
	return b.String(), uris
}

// syncDispatcher writes the combined dispatcher list when it changes, takes destinations that left out of every
// Kamailio pod at once, and has the pods reload once the kubelet has had time to update the mounted file, checking
// that each did. It returns how long to wait before the reload is due or retried, or zero.
func (r *KamailioClusterReconciler) syncDispatcher(ctx context.Context, kc *sipv1alpha1.KamailioCluster, fleet fleetRef, cfg kamailioSettings, token string, sets []dispatchSet, pods []corev1.Pod, status *sipv1alpha1.KamailioClusterStatus) (time.Duration, error) {
	log := logf.FromContext(ctx)
	content, want := combineDispatcherLists(sets)
	hash := contentHash(content)

	status.Sets = make([]sipv1alpha1.DispatchSet, 0, len(sets))
	bySet := map[int32]int{}
	for _, s := range sets {
		status.Sets = append(status.Sets, sipv1alpha1.DispatchSet{FreeswitchCluster: s.cluster, SetID: s.setID, Destinations: int32(len(want[s.setID]))})
		bySet[s.setID] = len(want[s.setID])
	}
	recordDestinations(fleet, bySet)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: kamailioDispatcherName(kc), Namespace: kc.Namespace}}
	var previous string
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err == nil {
		previous = cm.Data[dispatcherListKey]
	} else if !apierrors.IsNotFound(err) {
		return 0, err
	}
	notifier := r.podNotifier(kc, cfg, token, pods)

	// As for a FreeswitchCluster, the ConfigMap records what changed when; the status only mirrors it.
	if previous != content || cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation] == "" {
		changedAt := r.now().UTC().Format(time.RFC3339Nano)
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			if cm.Labels == nil {
				cm.Labels = map[string]string{}
			}
			maps.Copy(cm.Labels, kamailioLabels(kc))
			if cm.Annotations == nil {
				cm.Annotations = map[string]string{}
			}
			cm.Annotations[sipv1alpha1.DispatcherHashAnnotation] = hash
			cm.Annotations[sipv1alpha1.DispatcherUpdatedAtAnnotation] = changedAt
			cm.Data = map[string]string{dispatcherListKey: content}
			return controllerutil.SetControllerReference(kc, cm, r.Scheme)
		})
		if err != nil {
			return 0, err
		}
		log.Info("combined dispatcher list updated", "sets", len(sets), "hash", hash)
		dispatcherListUpdates.WithLabelValues(fleet.labels()...).Inc()
		if notifier != nil {
			for _, s := range sets {
				for _, uri := range removedURIs(parseDispatcherURIs(previous, s.setID), want[s.setID]) {
					err := notifier.SetInactive(ctx, s.setID, uri)
					recordSetInactive(fleet, err)
					if err != nil {
						// Kamailio's probing and the paused node's 503s still protect calls; keep going.
						log.Error(err, "could not mark destination inactive", "set", s.setID, "uri", uri)
					}
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

	// Kamailio reads the mounted list when it starts, and the kubelet may not have caught that file up yet: a pod
	// that has become ready since the last reconcile gets the reload and its check too, or it could keep a stale
	// list while the status says every pod holds the current one.
	if newlyActive(status.Members, pods) {
		status.DispatcherReloadedHash = ""
	}
	defer func() {
		recordPendingReload(fleet, notifier != nil && status.DispatcherReloadedHash != status.DispatcherHash)
	}()
	if notifier == nil || status.DispatcherReloadedHash == status.DispatcherHash {
		return 0, nil
	}
	delay := time.Duration(cfg.reloadDelaySecs) * time.Second
	if status.DispatcherUpdatedAt != nil {
		if wait := delay - r.now().Sub(status.DispatcherUpdatedAt.Time); wait > 0 {
			return wait, nil
		}
	}
	err := notifier.Reload(ctx, want)
	recordReload(fleet, err)
	if err != nil {
		if onlyListNotLoaded(err) {
			log.Info("a Kamailio pod has not loaded the new dispatcher list yet; will reload again", "detail", err.Error())
			return 10 * time.Second, nil
		}
		log.Error(err, "could not reload the Kamailio dispatcher list; will retry")
		return 30 * time.Second, nil
	}
	status.DispatcherReloadedHash = status.DispatcherHash
	return 0, nil
}

// newlyActive reports whether a pod is active now that wasn't in the members the status last recorded.
func newlyActive(before []sipv1alpha1.Member, pods []corev1.Pod) bool {
	was := map[string]bool{}
	for _, m := range before {
		if m.State == sipv1alpha1.MemberActive {
			was[m.Pod] = true
		}
	}
	for i := range pods {
		if memberState(&pods[i]) == sipv1alpha1.MemberActive && !was[pods[i].Name] {
			return true
		}
	}
	return false
}

// podNotifier makes each JSON-RPC call on every ready Kamailio pod, at its own address, with the token. A pod
// that isn't ready yet is left out; syncDispatcher reloads it once it is.
func (r *KamailioClusterReconciler) podNotifier(kc *sipv1alpha1.KamailioCluster, cfg kamailioSettings, token string, pods []corev1.Pod) DispatcherNotifier {
	if r.NewNotifier == nil {
		return nil
	}
	f := &fanoutNotifier{service: "KamailioCluster " + kc.Name}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || !podReady(p) || p.Status.PodIP == "" {
			continue
		}
		url := "http://" + net.JoinHostPort(p.Status.PodIP, strconv.Itoa(int(cfg.rpcPort))) + "/RPC"
		f.targets = append(f.targets, url)
		f.notifiers = append(f.notifiers, r.NewNotifier(url, token))
	}
	return f
}

// SetupWithManager sets up the controller with the Manager.
func (r *KamailioClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	if err := indexFleetPodsByNode(ctx, mgr); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &sipv1alpha1.KamailioCluster{}, dispatchClusterField, func(o client.Object) []string {
		kc := o.(*sipv1alpha1.KamailioCluster)
		names := make([]string, 0, len(kc.Spec.Dispatch))
		for _, d := range kc.Spec.Dispatch {
			names = append(names, d.FreeswitchCluster)
		}
		return names
	}); err != nil {
		return err
	}

	// The proxy fleets that dispatch to a FreeswitchCluster.
	dispatchers := func(ctx context.Context, namespace, fleet string) []reconcile.Request {
		var list sipv1alpha1.KamailioClusterList
		if err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingFields{dispatchClusterField: fleet}); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, kc := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: kc.Namespace, Name: kc.Name}})
		}
		return out
	}
	fleetToProxies := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		return dispatchers(ctx, o.GetNamespace(), o.GetName())
	})
	// A FreeswitchCluster's dispatcher list is a ConfigMap it controls, carrying the list's hash.
	fleetListToProxies := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		owner := metav1.GetControllerOf(o)
		if owner == nil || owner.Kind != "FreeswitchCluster" || o.GetAnnotations()[sipv1alpha1.DispatcherHashAnnotation] == "" {
			return nil
		}
		return dispatchers(ctx, o.GetNamespace(), owner.Name)
	})
	podToProxy := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		name := o.GetLabels()[sipv1alpha1.KamailioClusterLabel]
		if name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
	})
	nodeToProxies := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.MatchingFields{podNodeNameField: o.GetName()}); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, p := range pods.Items {
			if name := p.Labels[sipv1alpha1.KamailioClusterLabel]; name != "" {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: name}})
			}
		}
		return out
	})

	var fleets fleetLister = func(ctx context.Context) ([]client.Object, []sipv1alpha1.ExternalAddressSpec, error) {
		var list sipv1alpha1.KamailioClusterList
		if err := r.List(ctx, &list); err != nil {
			return nil, nil, err
		}
		objs := make([]client.Object, len(list.Items))
		specs := make([]sipv1alpha1.ExternalAddressSpec, len(list.Items))
		for i := range list.Items {
			objs[i], specs[i] = &list.Items[i], list.Items[i].Spec.ExternalAddress
		}
		return objs, specs, nil
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sipv1alpha1.KamailioCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&sipv1alpha1.FreeswitchCluster{}, fleetToProxies).
		Watches(&corev1.ConfigMap{}, fleetListToProxies).
		Watches(&corev1.Pod{}, podToProxy).
		Watches(&corev1.Node{}, nodeToProxies).
		Watches(&sipv1alpha1.SIPAddressPool{}, poolToFleets(fleets)).
		Watches(&corev1.Pod{}, agentToFleets(mgr.GetClient(), fleets), agentReports).
		Named("kamailiocluster").
		Complete(r)
}
