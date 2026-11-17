package controller

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

func cm(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: data}
}

// testLayout lays out testCluster-style references against ConfigMaps that exist with one file each.
func testLayout(t *testing.T, fsc *sipv1alpha1.FreeswitchCluster) configLayout {
	t.Helper()
	cms := map[string]*corev1.ConfigMap{}
	for _, ref := range fsc.Spec.Config.ConfigMapRefs {
		cms[ref.Name] = cm(ref.Name, map[string]string{"file.xml": "<x/>"})
	}
	conf, err := layoutConfig(fsc, cms, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conf
}

func treeCluster() (*sipv1alpha1.FreeswitchCluster, map[string]*corev1.ConfigMap) {
	fsc := testCluster()
	fsc.Spec.Config = sipv1alpha1.ConfigSpec{
		MountPath: "/usr/local/freeswitch/conf/",
		ConfigMapRefs: []sipv1alpha1.ConfigMapRef{
			{Name: "fs-root", Path: "."},
			{Name: "fs-autoload", Path: "autoload_configs"},
			{Name: "fs-extra"}, // defaults to its name
		},
	}
	cms := map[string]*corev1.ConfigMap{
		"fs-root":     cm("fs-root", map[string]string{"vars.xml": "v", "freeswitch.xml": "f"}),
		"fs-autoload": cm("fs-autoload", map[string]string{"sofia.conf.xml": "s", "acl.conf.xml": "a"}),
		"fs-extra":    {ObjectMeta: metav1.ObjectMeta{Name: "fs-extra"}, BinaryData: map[string][]byte{"prompt.wav": {1, 2, 3}}},
	}
	return fsc, cms
}

// items lists source:key->path, with each snapshot named by the ConfigMap it copies.
func items(conf configLayout) []string {
	source := map[string]string{}
	for _, s := range conf.snapshots {
		source[s.name] = s.source
	}
	var got []string
	for _, src := range conf.sources {
		for _, it := range src.ConfigMap.Items {
			got = append(got, source[src.ConfigMap.Name]+":"+it.Key+"->"+it.Path)
		}
	}
	return got
}

func TestLayoutConfigTree(t *testing.T) {
	fsc, cms := treeCluster()
	conf, err := layoutConfig(fsc, cms, nil)
	if err != nil {
		t.Fatal(err)
	}
	if conf.mountPath != "/usr/local/freeswitch/conf" {
		t.Fatalf("mountPath %q", conf.mountPath)
	}
	want := []string{ // references in name order
		"fs-autoload:acl.conf.xml->autoload_configs/acl.conf.xml",
		"fs-autoload:sofia.conf.xml->autoload_configs/sofia.conf.xml",
		"fs-extra:prompt.wav->fs-extra/prompt.wav",
		"fs-root:freeswitch.xml->freeswitch.xml",
		"fs-root:vars.xml->vars.xml",
	}
	if got := items(conf); !reflect.DeepEqual(got, want) {
		t.Fatalf("items:\n got %v\nwant %v", got, want)
	}
	if conf.hash == "" {
		t.Fatal("no hash")
	}
	for _, s := range conf.snapshots {
		if s.name != configSnapshotName(fsc, configMapHash(cms[s.source])) {
			t.Fatalf("snapshot %s of %s is not named by its contents", s.name, s.source)
		}
		if !reflect.DeepEqual(s.data, cms[s.source].Data) || !reflect.DeepEqual(s.binaryData, cms[s.source].BinaryData) {
			t.Fatalf("snapshot %s does not copy %s", s.name, s.source)
		}
	}
}

func TestLayoutConfigSnapshots(t *testing.T) {
	fsc, cms := treeCluster()
	a, _ := layoutConfig(fsc, cms, nil)

	// The pods never mount the user's ConfigMaps, only the operator's copies.
	for _, src := range a.sources {
		if _, user := cms[src.ConfigMap.Name]; user {
			t.Fatalf("template mounts the user's ConfigMap %s", src.ConfigMap.Name)
		}
	}

	// An edit makes a new snapshot for that ConfigMap only.
	cms["fs-autoload"].Data["sofia.conf.xml"] = "changed"
	b, _ := layoutConfig(fsc, cms, nil)
	if a.sources[0].ConfigMap.Name == b.sources[0].ConfigMap.Name {
		t.Fatal("an edited ConfigMap needs a new snapshot")
	}
	if a.sources[2].ConfigMap.Name != b.sources[2].ConfigMap.Name {
		t.Fatal("an unchanged ConfigMap keeps its snapshot")
	}

	// Two references with the same contents share one snapshot.
	cms["fs-extra"] = cm("fs-extra", map[string]string{"vars.xml": "v", "freeswitch.xml": "f"})
	c, err := layoutConfig(fsc, cms, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.sources) != 3 || len(c.snapshots) != 2 {
		t.Fatalf("got %d sources and %d snapshots, want 3 and 2", len(c.sources), len(c.snapshots))
	}

}

func TestLayoutConfigHash(t *testing.T) {
	fsc, cms := treeCluster()
	a, _ := layoutConfig(fsc, cms, nil)
	again, _ := layoutConfig(fsc, cms, nil)
	if a.hash != again.hash {
		t.Fatal("hash must be stable")
	}

	// Reordering the references changes nothing a pod sees, so it must not change the template.
	refs := fsc.Spec.Config.ConfigMapRefs
	refs[0], refs[2] = refs[2], refs[0]
	reordered, _ := layoutConfig(fsc, cms, nil)
	if a.hash != reordered.hash || !reflect.DeepEqual(a.sources, reordered.sources) {
		t.Fatal("reordering configMapRefs must not change the layout")
	}
	ha, _ := templateHash(&buildStatefulSet(fsc, a).Spec.Template)
	hr, _ := templateHash(&buildStatefulSet(fsc, reordered).Spec.Template)
	if ha != hr {
		t.Fatal("reordering configMapRefs must not change the template")
	}

	cms["fs-autoload"].Data["sofia.conf.xml"] = "changed"
	b, _ := layoutConfig(fsc, cms, nil)
	if a.hash == b.hash {
		t.Fatal("a content change must change the hash")
	}
	cms["fs-extra"].BinaryData["prompt.wav"] = []byte{9}
	c, _ := layoutConfig(fsc, cms, nil)
	if b.hash == c.hash {
		t.Fatal("a binary change must change the hash")
	}
	for i := range fsc.Spec.Config.ConfigMapRefs {
		if fsc.Spec.Config.ConfigMapRefs[i].Name == "fs-extra" {
			fsc.Spec.Config.ConfigMapRefs[i].Path = "sounds"
		}
	}
	d, _ := layoutConfig(fsc, cms, nil)
	if c.hash == d.hash {
		t.Fatal("moving a ConfigMap must change the hash")
	}
}

func TestConfigHasherCachesByVersion(t *testing.T) {
	var hasher configHasher
	c := cm("fs-root", map[string]string{"vars.xml": "v"})
	c.Namespace, c.UID, c.ResourceVersion = "ns", "uid-1", "10"
	first := hasher.hash(c)
	if first != configMapHash(c) {
		t.Fatal("hasher must agree with configMapHash")
	}
	// Same version: the remembered hash, even though (impossibly) the data differs.
	c.Data["vars.xml"] = "edited"
	if hasher.hash(c) != first {
		t.Fatal("an unchanged resourceVersion must reuse the hash")
	}
	c.ResourceVersion = "11"
	if hasher.hash(c) == first {
		t.Fatal("a new resourceVersion must be hashed again")
	}
}

func TestLayoutConfigErrors(t *testing.T) {
	var ce *configError

	fsc, cms := treeCluster()
	delete(cms, "fs-autoload")
	if _, err := layoutConfig(fsc, cms, nil); !errors.As(err, &ce) || !strings.Contains(err.Error(), "fs-autoload not found") {
		t.Fatalf("missing: %v", err)
	}

	fsc, cms = treeCluster()
	cms["fs-extra"] = cm("fs-extra", map[string]string{"vars.xml": "dup"})
	fsc.Spec.Config.ConfigMapRefs[2].Path = "."
	if _, err := layoutConfig(fsc, cms, nil); !errors.As(err, &ce) || !strings.Contains(err.Error(), "both write vars.xml") {
		t.Fatalf("duplicate: %v", err)
	}

	// An empty ConfigMap is refused, not rolled out: it could remove a whole directory, such as every profile.
	fsc, cms = treeCluster()
	cms["fs-extra"] = cm("fs-extra", nil)
	if _, err := layoutConfig(fsc, cms, nil); !errors.As(err, &ce) || !strings.Contains(err.Error(), "fs-extra has no keys") {
		t.Fatalf("empty: %v", err)
	}

	// A file in one ConfigMap where another needs a directory, in either order.
	for _, file := range []string{"aaa", "zzz"} {
		fsc, cms = treeCluster()
		fsc.Spec.Config.ConfigMapRefs = []sipv1alpha1.ConfigMapRef{{Name: file, Path: "."}, {Name: "mmm", Path: "bin"}}
		cms = map[string]*corev1.ConfigMap{
			file:  cm(file, map[string]string{"bin": "#!/bin/sh"}),
			"mmm": cm("mmm", map[string]string{"env.sh": "#!/bin/sh"}),
		}
		if _, err := layoutConfig(fsc, cms, nil); !errors.As(err, &ce) || !strings.Contains(err.Error(), "writes the file bin where") {
			t.Fatalf("file/directory collision (%s): %v", file, err)
		}
	}

	for _, mp := range []string{
		"/etc/freeswitch-operator", "/etc/freeswitch-operator/scripts", "/etc", "/", // hide the operator's volumes
		"/etc/freeswitch-operator/scripts/conf", "/etc/freeswitch-operator/podinfo/x", // inside them
		"/tmp", // hides the drain marker
	} {
		fsc, cms = treeCluster()
		fsc.Spec.Config.MountPath = mp
		if _, err := layoutConfig(fsc, cms, nil); !errors.As(err, &ce) {
			t.Fatalf("mountPath %s must be refused: %v", mp, err)
		}
	}
	for _, mp := range []string{"/etc/freeswitch-operator/config", "/tmp/conf", "/usr/local/freeswitch/conf"} {
		fsc, cms = treeCluster()
		fsc.Spec.Config.MountPath = mp
		if _, err := layoutConfig(fsc, cms, nil); err != nil {
			t.Fatalf("mountPath %s must be allowed: %v", mp, err)
		}
	}
}

func TestBuildStatefulSetConfigVolume(t *testing.T) {
	fsc, cms := treeCluster()
	conf, err := layoutConfig(fsc, cms, nil)
	if err != nil {
		t.Fatal(err)
	}
	sts := buildStatefulSet(fsc, conf)
	if sts.Spec.Template.Annotations[configHashAnnotation] != conf.hash {
		t.Fatal("the pod template must carry the config hash")
	}
	var found bool
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == "config" && v.Projected != nil && len(v.Projected.Sources) == 3 {
			found = true
		}
	}
	if !found {
		t.Fatal("missing projected config volume")
	}
	var mounted bool
	for _, m := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "config" && m.MountPath == "/usr/local/freeswitch/conf" && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Fatal("config not mounted at mountPath")
	}

	fsc.Spec.Config.ConfigMapRefs = nil
	bare := buildStatefulSet(fsc, configLayout{mountPath: configDir})
	if _, ok := bare.Spec.Template.Annotations[configHashAnnotation]; ok {
		t.Fatal("no config, no hash annotation")
	}
	for _, v := range bare.Spec.Template.Spec.Volumes {
		if v.Name == "config" {
			t.Fatal("no config, no volume")
		}
	}
}
