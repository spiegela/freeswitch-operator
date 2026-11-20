package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

// Only a fleet that draws from a pool gets the bind-address file and its variable: other fleets' templates, and so
// their pods, don't change.
func TestBindAddressOnlyForPoolFleets(t *testing.T) {
	files := func(sts []corev1.Volume) []string {
		for _, v := range sts {
			if v.Name == volumePodInfo {
				var out []string
				for _, it := range v.DownwardAPI.Items {
					out = append(out, it.Path)
				}
				return out
			}
		}
		return nil
	}
	hasEnv := func(env []corev1.EnvVar) bool {
		for _, e := range env {
			if e.Name == "FS_BIND_ADDRESS_FILE" {
				return true
			}
		}
		return false
	}

	plain := newCluster("ns", nil)
	sts := buildStatefulSet(plain, configLayout{})
	if got := files(sts.Spec.Template.Spec.Volumes); len(got) != 1 || hasEnv(sts.Spec.Template.Spec.Containers[0].Env) {
		t.Errorf("a fleet on its node's address: files %v", got)
	}

	pooled := newCluster("ns", func(f *sipv1alpha1.FreeswitchCluster) {
		f.Spec.ExternalAddress = sipv1alpha1.ExternalAddressSpec{Source: sipv1alpha1.ExternalAddressAddressPool, AddressPoolRef: &sipv1alpha1.AddressPoolReference{Name: "p"}}
	})
	sts = buildStatefulSet(pooled, configLayout{})
	if got := files(sts.Spec.Template.Spec.Volumes); len(got) != 2 || got[1] != "bind-address" || !hasEnv(sts.Spec.Template.Spec.Containers[0].Env) {
		t.Errorf("a pool fleet: files %v", got)
	}

	proxy := newProxy("ns", func(k *sipv1alpha1.KamailioCluster) {
		k.Spec.ExternalAddress = sipv1alpha1.ExternalAddressSpec{Source: sipv1alpha1.ExternalAddressAddressPool, AddressPoolRef: &sipv1alpha1.AddressPoolReference{Name: "p"}}
	})
	ksts := buildKamailioStatefulSet(proxy, "cfg", "hash")
	if got := files(ksts.Spec.Template.Spec.Volumes); len(got) != 2 {
		t.Errorf("a pool proxy: files %v", got)
	}
}
