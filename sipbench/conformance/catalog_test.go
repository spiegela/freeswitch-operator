// Package conformance holds the conformance record (see README.md). This test keeps it consistent with itself
// and with the scenarios: every scenario is recorded, and every covered case names scenarios that exist.
package conformance

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type record struct {
	Source struct {
		ID        string `yaml:"id"`
		Title     string `yaml:"title"`
		Reference string `yaml:"reference"`
		Citation  string `yaml:"citation"`
	} `yaml:"source"`
	Cases []entry `yaml:"cases"`
}

type entry struct {
	ID         string         `yaml:"id"`
	Ref        string         `yaml:"ref"`
	Title      string         `yaml:"title"`
	Area       string         `yaml:"area"`
	Component  string         `yaml:"component"`
	Expect     string         `yaml:"expect"`
	Status     string         `yaml:"status"`
	Scenarios  []string       `yaml:"scenarios"`
	Gap        string         `yaml:"gap"`
	Milestone  string         `yaml:"milestone"`
	Impairment map[string]any `yaml:"impairment"`
	Reason     string         `yaml:"reason"`
}

var (
	areas = set("call-setup", "call-release", "error-handling", "transfer", "dtmf", "codec", "media", "fax",
		"headers", "identity", "keepalive", "transport", "timers", "parsing", "security", "impairment", "capacity",
		"registration", "resilience")
	components = set("edge", "proxy", "freeswitch", "operator")
	milestones = set("M2", "M3", "M4", "M5", "M6")
)

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// scenarioNames reads the name: of every scenario file.
func scenarioNames(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../scenarios/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios found: %v", err)
	}
	names := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Name string `yaml:"name"`
		}
		if err := yaml.Unmarshal(raw, &s); err != nil || s.Name == "" {
			t.Fatalf("%s: no name (%v)", f, err)
		}
		names[s.Name] = true
	}
	return names
}

func TestRecord(t *testing.T) {
	scenarios := scenarioNames(t)
	files, err := filepath.Glob("*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no record files: %v", err)
	}
	ids := map[string]string{}
	sources := map[string]string{}
	recorded := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r record
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if r.Source.ID == "" || r.Source.Title == "" || r.Source.Reference == "" || r.Source.Citation == "" {
			t.Errorf("%s: source needs id, title, reference and citation", f)
		}
		if other, dup := sources[r.Source.ID]; dup {
			t.Errorf("%s: source id %s also used by %s", f, r.Source.ID, other)
		}
		sources[r.Source.ID] = f
		for _, c := range r.Cases {
			where := f + ": " + c.ID
			if c.ID == "" || c.Ref == "" || c.Title == "" || c.Expect == "" {
				t.Errorf("%s: id, ref, title and expect are required", where)
			}
			if other, dup := ids[c.ID]; dup {
				t.Errorf("%s: id also in %s", where, other)
			}
			ids[c.ID] = f
			if !areas[c.Area] {
				t.Errorf("%s: unknown area %q", where, c.Area)
			}
			if !components[c.Component] {
				t.Errorf("%s: unknown component %q", where, c.Component)
			}
			for _, s := range c.Scenarios {
				if !scenarios[s] {
					t.Errorf("%s: no scenario named %s", where, s)
				}
				recorded[s] = true
			}
			switch c.Status {
			case "covered":
				if len(c.Scenarios) == 0 {
					t.Errorf("%s: covered needs scenarios", where)
				}
			case "partial":
				if len(c.Scenarios) == 0 || c.Gap == "" {
					t.Errorf("%s: partial needs scenarios and a gap", where)
				}
			case "planned":
				if !milestones[c.Milestone] {
					t.Errorf("%s: planned needs a milestone (M2-M6), got %q", where, c.Milestone)
				}
			case "not-applicable":
				if c.Reason == "" {
					t.Errorf("%s: not-applicable needs a reason", where)
				}
			default:
				t.Errorf("%s: unknown status %q", where, c.Status)
			}
		}
	}
	var unrecorded []string
	for s := range scenarios {
		if !recorded[s] {
			unrecorded = append(unrecorded, s)
		}
	}
	sort.Strings(unrecorded)
	if len(unrecorded) > 0 {
		t.Errorf("scenarios no case names (add or extend an entry): %v", unrecorded)
	}
}
