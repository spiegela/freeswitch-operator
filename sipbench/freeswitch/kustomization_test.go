// Package freeswitch holds the bench's FreeSWITCH configuration. Compose mounts conf/ whole; the cluster bench
// gets it through the ConfigMaps kustomization.yaml generates, which list each file by hand. This test keeps the
// two the same, so a file added under conf/ can't be silently missing on the cluster.
package freeswitch

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKustomizationListsEveryConfFile(t *testing.T) {
	raw, err := os.ReadFile("kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		ConfigMapGenerator []struct {
			Name  string   `yaml:"name"`
			Files []string `yaml:"files"`
		} `yaml:"configMapGenerator"`
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatal(err)
	}

	listed := map[string]string{} // file -> generator
	for _, g := range k.ConfigMapGenerator {
		dirs := map[string]bool{}
		for _, f := range g.Files {
			if other, dup := listed[f]; dup {
				t.Errorf("%s is listed by both %s and %s", f, other, g.Name)
			}
			listed[f] = g.Name
			dirs[path.Dir(f)] = true
		}
		// A ConfigMap becomes one directory in the FreeswitchCluster, so its files must share one.
		if len(dirs) > 1 {
			t.Errorf("ConfigMap %s mixes files from several directories: %v", g.Name, keys(dirs))
		}
	}

	var onDisk []string
	err = filepath.WalkDir("conf", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		onDisk = append(onDisk, filepath.ToSlash(p))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range onDisk {
		if _, ok := listed[f]; !ok {
			t.Errorf("%s is not in any configMapGenerator in kustomization.yaml (the cluster bench would not see it)", f)
		}
		delete(listed, f)
	}
	for f := range listed {
		t.Errorf("kustomization.yaml lists %s, which does not exist", f)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
