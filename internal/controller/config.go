package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

const (
	// configHashAnnotation on the pod template summarises the configuration the template mounts, for people
	// reading it; the snapshot names in the template are what actually change it.
	configHashAnnotation = "sip.instruments.com/config-hash"
	// configSnapshotLabel marks the operator's immutable copies of referenced ConfigMaps.
	configSnapshotLabel = "sip.instruments.com/config-snapshot"
	// configSourceAnnotation on a snapshot names the ConfigMap it was copied from.
	configSourceAnnotation = "sip.instruments.com/config-source"
)

// Pods never mount the user's ConfigMaps. The kubelet pushes a mounted ConfigMap's edits into every running pod
// within a minute, so an edit would reach the whole fleet at once, ahead of the drained one-node-at-a-time roll.
// Instead the operator copies each referenced ConfigMap into an immutable snapshot named by a hash of its
// contents, and each revision of the pod template mounts its own snapshots. An edit makes new snapshots, and so
// a new template, which the rollout loop works through; pods on older revisions keep the files they started with.

// configSnapshot is one immutable copy of a referenced ConfigMap.
type configSnapshot struct {
	name       string
	source     string
	data       map[string]string
	binaryData map[string][]byte
}

// configLayout is the referenced ConfigMaps laid out as one directory tree: a projected volume whose items put
// each snapshot's keys in its reference's directory.
type configLayout struct {
	mountPath string
	sources   []corev1.VolumeProjection
	snapshots []configSnapshot
	hash      string
}

// configError is a problem with the referenced configuration the user has to fix: a missing ConfigMap or two
// references claiming the same path. The operator reports it and leaves the running fleet alone.
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

// within reports whether p is dir or lies under it. Both are clean absolute paths.
func within(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// checkMountPath refuses a mount that would hide, or sit inside, the operator's own volumes, or hide the drain
// marker in the container's /tmp (readiness would then never drop during a drain).
func checkMountPath(mp string) error {
	for _, dir := range []string{scriptsDir, podInfoDir} {
		if within(dir, mp) {
			return &configError{fmt.Sprintf("config.mountPath %s would hide the operator's %s", mp, dir)}
		}
		if within(mp, dir) {
			return &configError{fmt.Sprintf("config.mountPath %s is inside the operator's %s", mp, dir)}
		}
	}
	if markerDir := path.Dir(drainMarker); within(markerDir, mp) {
		return &configError{fmt.Sprintf("config.mountPath %s would hide %s, where the drain marker lives", mp, markerDir)}
	}
	return nil
}

func configMapKeys(cm *corev1.ConfigMap) []string {
	keys := make([]string, 0, len(cm.Data)+len(cm.BinaryData))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	for k := range cm.BinaryData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// configMapHash hashes a ConfigMap's keys and values, and nothing else: its name, labels and version don't
// change the files a pod sees.
func configMapHash(cm *corev1.ConfigMap) string {
	h := sha256.New()
	for _, k := range configMapKeys(cm) {
		if v, ok := cm.Data[k]; ok {
			fmt.Fprintf(h, "k\x00%s\x00%d\x00%s", k, len(v), v)
		} else {
			fmt.Fprintf(h, "b\x00%s\x00%d\x00", k, len(cm.BinaryData[k]))
			h.Write(cm.BinaryData[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// configSnapshotName is the snapshot of contents with this hash. Content addressing makes it safe to share
// between references and revisions, and to create again after a lost write.
func configSnapshotName(fsc *sipv1alpha1.FreeswitchCluster, hash string) string {
	prefix := fsc.Name
	if len(prefix) > 200 {
		prefix = prefix[:200]
	}
	return prefix + "-config-" + hash
}

// configHasher remembers each ConfigMap's hash by resourceVersion, so reconciles that the config didn't trigger
// (pods, endpoints, nodes) don't re-hash every referenced ConfigMap in full.
type configHasher struct {
	mu   sync.Mutex
	seen map[types.NamespacedName]hashedVersion
}

type hashedVersion struct {
	uid             types.UID
	resourceVersion string
	hash            string
}

func (c *configHasher) hash(cm *corev1.ConfigMap) string {
	if cm.ResourceVersion == "" {
		return configMapHash(cm)
	}
	key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.seen[key]; ok && v.uid == cm.UID && v.resourceVersion == cm.ResourceVersion {
		return v.hash
	}
	h := configMapHash(cm)
	if c.seen == nil {
		c.seen = map[types.NamespacedName]hashedVersion{}
	}
	c.seen[key] = hashedVersion{uid: cm.UID, resourceVersion: cm.ResourceVersion, hash: h}
	return h
}

// layoutConfig lays out the cluster's referenced ConfigMaps, which cms holds by name, as snapshots. hashOf hashes
// a ConfigMap (nil means configMapHash). References are taken in name order and keys in sorted order, so the same
// contents always render the same template, however the list is ordered.
func layoutConfig(fsc *sipv1alpha1.FreeswitchCluster, cms map[string]*corev1.ConfigMap, hashOf func(*corev1.ConfigMap) string) (configLayout, error) {
	out := configLayout{mountPath: configMountPath(fsc)}
	if err := checkMountPath(out.mountPath); err != nil {
		return out, err
	}
	refs := append([]sipv1alpha1.ConfigMapRef(nil), fsc.Spec.Config.ConfigMapRefs...)
	if len(refs) == 0 {
		return out, nil
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	if hashOf == nil {
		hashOf = configMapHash
	}

	h := sha256.New()
	files := map[string]string{} // file path under mountPath -> ConfigMap that writes it
	dirs := map[string]string{}  // directory under mountPath -> a ConfigMap with files in it
	snapshotted := map[string]bool{}
	for _, ref := range refs {
		cm, ok := cms[ref.Name]
		if !ok || cm == nil {
			return out, &configError{fmt.Sprintf("ConfigMap %s not found", ref.Name)}
		}
		dir := configRefDir(ref)
		keys := configMapKeys(cm)
		contentHash := hashOf(cm)
		fmt.Fprintf(h, "ref\x00%s\x00%s\x00", dir, contentHash)
		if len(keys) == 0 {
			// Nothing to mount. (A projection without items would mean every key at the volume's root.)
			continue
		}

		items := make([]corev1.KeyToPath, 0, len(keys))
		for _, k := range keys {
			p := path.Join(dir, k)
			if other, dup := files[p]; dup {
				return out, &configError{fmt.Sprintf("ConfigMaps %s and %s both write %s", other, ref.Name, p)}
			}
			if other, dup := dirs[p]; dup {
				return out, &configError{fmt.Sprintf("ConfigMap %s writes the file %s where %s has a directory", ref.Name, p, other)}
			}
			files[p] = ref.Name
			for d := path.Dir(p); d != "."; d = path.Dir(d) {
				if other, dup := files[d]; dup {
					return out, &configError{fmt.Sprintf("ConfigMap %s writes the file %s where %s has a directory", other, d, ref.Name)}
				}
				dirs[d] = ref.Name
			}
			items = append(items, corev1.KeyToPath{Key: k, Path: p})
		}

		name := configSnapshotName(fsc, contentHash)
		if !snapshotted[name] {
			snapshotted[name] = true
			out.snapshots = append(out.snapshots, configSnapshot{name: name, source: ref.Name, data: cm.Data, binaryData: cm.BinaryData})
		}
		out.sources = append(out.sources, corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Items:                items,
		}})
	}
	out.hash = hex.EncodeToString(h.Sum(nil)[:8])
	return out, nil
}

// podConfigSnapshots adds the names of the snapshots a pod mounts to into.
func podConfigSnapshots(p *corev1.Pod, into map[string]bool) {
	for _, v := range p.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ConfigMap != nil {
				into[src.ConfigMap.Name] = true
			}
		}
	}
}
