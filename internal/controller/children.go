package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// What a FreeswitchCluster and a KamailioCluster both keep of their children, alike apart from names and labels.

// newPDB lets at most one of the pods selector matches be disrupted at a time.
func newPDB(name, namespace string, labels, selector map[string]string) *policyv1.PodDisruptionBudget {
	maxUnavailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: selector},
		},
	}
}

// ensurePDB writes want, controlled by owner. The selector is set only when the PDB is created.
func ensurePDB(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, want *policyv1.PodDisruptionBudget) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, pdb, func() error {
		pdb.Labels = want.Labels
		pdb.Spec.MaxUnavailable = want.Spec.MaxUnavailable
		if pdb.CreationTimestamp.IsZero() {
			pdb.Spec.Selector = want.Spec.Selector
		}
		return controllerutil.SetControllerReference(owner, pdb, scheme)
	})
	return err
}

// pruneConfigSnapshots deletes owner's config snapshots, among those matching sel, that keep doesn't name: keep holds
// the current template's and every pod's. It waits until the StatefulSet controller has seen the current template,
// so no pod can still be created from an older one.
func pruneConfigSnapshots(ctx context.Context, c client.Client, owner client.Object, sts *appsv1.StatefulSet, sel client.MatchingLabels, keep map[string]bool) error {
	if sts.CreationTimestamp.IsZero() || sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	sel[configSnapshotLabel] = configSnapshotValue
	var list corev1.ConfigMapList
	if err := c.List(ctx, &list, client.InNamespace(owner.GetNamespace()), sel); err != nil {
		return err
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if keep[cm.Name] || cm.DeletionTimestamp != nil || !metav1.IsControlledBy(cm, owner) {
			continue
		}
		uid := cm.UID
		if err := c.Delete(ctx, cm, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		logf.FromContext(ctx).Info("deleted unused config snapshot", "configMap", cm.Name)
	}
	return nil
}

// replacePod deletes the pod a rolling update replaces next, if any; one already gone counts as replaced.
func replacePod(ctx context.Context, c client.Client, fleet fleetRef, decision rolloutDecision, updateRevision string) error {
	log := logf.FromContext(ctx)
	p := decision.Replace
	if p == nil {
		if decision.Waiting != "" {
			log.V(1).Info("rolling update waiting", "reason", decision.Waiting, "outdated", decision.Outdated)
		}
		return nil
	}
	log.Info("replacing pod for rolling update", "pod", p.Name, "revision", podRevision(p), "updateRevision", updateRevision)
	uid := p.UID
	if err := c.Delete(ctx, p, client.Preconditions{UID: &uid}); err != nil {
		return client.IgnoreNotFound(err)
	}
	fleetPodReplacements.WithLabelValues(fleet.labels()...).Inc()
	return nil
}
