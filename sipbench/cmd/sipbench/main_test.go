package main

import (
	"slices"
	"testing"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
)

func TestPick(t *testing.T) {
	ss := []*scenario.Scenario{
		{Name: "basic-two-way"},
		{Name: "node-lost-mid-call", Long: true},
		{Name: "netem-media-baseline"},
		{Name: "netem-media-lte", Long: true},
	}
	names := func(ss []*scenario.Scenario) []string {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			out = append(out, s.Name)
		}
		return out
	}
	for _, c := range []struct {
		only  string
		short bool
		want  []string
	}{
		{"", false, []string{"basic-two-way", "node-lost-mid-call", "netem-media-baseline", "netem-media-lte"}},
		{"", true, []string{"basic-two-way", "netem-media-baseline"}},
		{"^netem-", false, []string{"netem-media-baseline", "netem-media-lte"}},
		{"^netem-", true, []string{"netem-media-baseline"}},
	} {
		got, err := pick(ss, c.only, c.short)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(names(got), c.want) {
			t.Errorf("pick(%q, short=%v) = %v, want %v", c.only, c.short, names(got), c.want)
		}
	}
	if _, err := pick(ss, "(", false); err == nil {
		t.Error("took a bad regexp")
	}
}

func TestShareable(t *testing.T) {
	ss, err := scenario.LoadDir("../../scenarios")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*scenario.Scenario{}
	n := 0
	for _, s := range ss {
		byName[s.Name] = s
		if shareable(s) {
			n++
		}
	}
	for name, want := range map[string]bool{
		"basic-two-way":            true,
		"raw-unknown-version":      true,
		"blind-transfer":           true,
		"drain-mid-call":           false, // fleet actions
		"netem-media-baseline":     false, // impairs the runner's own traffic
		"rolling-update-live-call": false,
		"raw-malformed-requests":   false, // checks Kamailio's counters
	} {
		if s := byName[name]; s == nil {
			t.Errorf("no scenario %s", name)
		} else if got := shareable(s); got != want {
			t.Errorf("shareable(%s) = %v, want %v", name, got, want)
		}
	}
	if n == 0 || n == len(ss) {
		t.Errorf("%d of %d scenarios shareable", n, len(ss))
	}
}
