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
	conf, err := layoutConfig(fsc, cms)
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

func TestLayoutConfigTree(t *testing.T) {
	fsc, cms := treeCluster()
	conf, err := layoutConfig(fsc, cms)
	if err != nil {
		t.Fatal(err)
	}
	if conf.mountPath != "/usr/local/freeswitch/conf" {
		t.Fatalf("mountPath %q", conf.mountPath)
	}
	var got []string
	for _, src := range conf.sources {
		for _, it := range src.ConfigMap.Items {
			got = append(got, src.ConfigMap.Name+":"+it.Key+"->"+it.Path)
		}
	}
	want := []string{
		"fs-root:freeswitch.xml->freeswitch.xml",
		"fs-root:vars.xml->vars.xml",
		"fs-autoload:acl.conf.xml->autoload_configs/acl.conf.xml",
		"fs-autoload:sofia.conf.xml->autoload_configs/sofia.conf.xml",
		"fs-extra:prompt.wav->fs-extra/prompt.wav",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items:\n got %v\nwant %v", got, want)
	}
	if conf.hash == "" {
		t.Fatal("no hash")
	}
}

func TestLayoutConfigHash(t *testing.T) {
	fsc, cms := treeCluster()
	a, _ := layoutConfig(fsc, cms)
	again, _ := layoutConfig(fsc, cms)
	if a.hash != again.hash {
		t.Fatal("hash must be stable")
	}
	cms["fs-autoload"].Data["sofia.conf.xml"] = "changed"
	b, _ := layoutConfig(fsc, cms)
	if a.hash == b.hash {
		t.Fatal("a content change must change the hash")
	}
	cms["fs-extra"].BinaryData["prompt.wav"] = []byte{9}
	c, _ := layoutConfig(fsc, cms)
	if b.hash == c.hash {
		t.Fatal("a binary change must change the hash")
	}
	fsc.Spec.Config.ConfigMapRefs[2].Path = "sounds"
	d, _ := layoutConfig(fsc, cms)
	if c.hash == d.hash {
		t.Fatal("moving a ConfigMap must change the hash")
	}
}

func TestLayoutConfigErrors(t *testing.T) {
	var ce *configError

	fsc, cms := treeCluster()
	delete(cms, "fs-autoload")
	if _, err := layoutConfig(fsc, cms); !errors.As(err, &ce) || !strings.Contains(err.Error(), "fs-autoload not found") {
		t.Fatalf("missing: %v", err)
	}

	fsc, cms = treeCluster()
	cms["fs-extra"] = cm("fs-extra", map[string]string{"vars.xml": "dup"})
	fsc.Spec.Config.ConfigMapRefs[2].Path = "."
	if _, err := layoutConfig(fsc, cms); !errors.As(err, &ce) || !strings.Contains(err.Error(), "both write vars.xml") {
		t.Fatalf("duplicate: %v", err)
	}

	for _, mp := range []string{"/etc/freeswitch-operator", "/etc/freeswitch-operator/scripts", "/etc", "/"} {
		fsc, cms = treeCluster()
		fsc.Spec.Config.MountPath = mp
		if _, err := layoutConfig(fsc, cms); !errors.As(err, &ce) {
			t.Fatalf("mountPath %s must be refused: %v", mp, err)
		}
	}
	fsc, cms = treeCluster()
	fsc.Spec.Config.MountPath = "/etc/freeswitch-operator/config"
	if _, err := layoutConfig(fsc, cms); err != nil {
		t.Fatalf("the default mountPath must be allowed: %v", err)
	}
}

func TestBuildStatefulSetConfigVolume(t *testing.T) {
	fsc, cms := treeCluster()
	conf, err := layoutConfig(fsc, cms)
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
