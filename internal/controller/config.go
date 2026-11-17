package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

// configHashAnnotation on the pod template carries a hash of every referenced ConfigMap's contents, so a config
// change changes the template and rolls the fleet one drained node at a time.
const configHashAnnotation = "sip.instruments.com/config-hash"

// configLayout is the referenced ConfigMaps laid out as one directory tree: a projected volume whose items put each
// ConfigMap's keys in that reference's directory.
type configLayout struct {
	mountPath string
	sources   []corev1.VolumeProjection
	hash      string
}

// configError is a problem with the referenced configuration the user has to fix: a missing ConfigMap or two
// references writing the same file. The operator reports it and leaves the running fleet alone.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

func configMountPath(fsc *sipv1alpha1.FreeswitchCluster) string {
	if p := fsc.Spec.Config.MountPath; p != "" {
		return path.Clean(p)
	}
	return configDir
}

func configRefDir(ref sipv1alpha1.ConfigMapRef) string {
	if ref.Path == "" {
		return ref.Name
	}
	return ref.Path
}

// layoutConfig lays out the cluster's referenced ConfigMaps, which cms holds by name. Keys come in sorted order and
// the hash covers names, directories, keys and values, so the same inputs always render the same template.
func layoutConfig(fsc *sipv1alpha1.FreeswitchCluster, cms map[string]*corev1.ConfigMap) (configLayout, error) {
	out := configLayout{mountPath: configMountPath(fsc)}
	for _, reserved := range []string{scriptsDir, podInfoDir} {
		if out.mountPath == reserved || strings.HasPrefix(reserved+"/", strings.TrimSuffix(out.mountPath, "/")+"/") {
			return out, &configError{fmt.Sprintf("config.mountPath %s would hide the operator's %s", out.mountPath, reserved)}
		}
	}
	refs := fsc.Spec.Config.ConfigMapRefs
	if len(refs) == 0 {
		return out, nil
	}

	h := sha256.New()
	owner := map[string]string{} // file path under mountPath -> ConfigMap that writes it
	for _, ref := range refs {
		cm, ok := cms[ref.Name]
		if !ok || cm == nil {
			return out, &configError{fmt.Sprintf("ConfigMap %s not found", ref.Name)}
		}
		dir := configRefDir(ref)
		keys := make([]string, 0, len(cm.Data)+len(cm.BinaryData))
		for k := range cm.Data {
			keys = append(keys, k)
		}
		for k := range cm.BinaryData {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		items := make([]corev1.KeyToPath, 0, len(keys))
		fmt.Fprintf(h, "ref\x00%s\x00%s\x00", ref.Name, dir)
		for _, k := range keys {
			p := path.Join(dir, k)
			if other, dup := owner[p]; dup {
				return out, &configError{fmt.Sprintf("ConfigMaps %s and %s both write %s", other, ref.Name, p)}
			}
			owner[p] = ref.Name
			items = append(items, corev1.KeyToPath{Key: k, Path: p})
			if v, ok := cm.Data[k]; ok {
				fmt.Fprintf(h, "k\x00%s\x00%d\x00%s", k, len(v), v)
			} else {
				fmt.Fprintf(h, "b\x00%s\x00%d\x00", k, len(cm.BinaryData[k]))
				h.Write(cm.BinaryData[k])
			}
		}
		out.sources = append(out.sources, corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
			Items:                items,
		}})
	}
	out.hash = hex.EncodeToString(h.Sum(nil)[:8])
	return out, nil
}
