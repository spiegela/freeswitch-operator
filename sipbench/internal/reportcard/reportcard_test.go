package reportcard

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/runner"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

func report(results ...runner.Result) *runner.Report {
	r := &runner.Report{StartedAt: time.Date(2026, 11, 17, 18, 44, 0, 0, time.UTC)}
	for _, x := range results {
		r.Add(x)
	}
	return r
}

func testInput() Input {
	return Input{
		Commit: "abc1234",
		Kind: report(
			runner.Result{Name: "basic", Status: "pass", Duration: 3200 * time.Millisecond},
			runner.Result{Name: "g722", Status: "skip", Reason: "needs codec:g722"},
			runner.Result{Name: "transfer", Status: "fail", Failures: []string{"call 1: heard 0 Hz | want 600"}},
		),
		Compose: report(
			runner.Result{Name: "basic", Status: "pass", Duration: time.Second},
			runner.Result{Name: "g722", Status: "skip", Reason: "needs codec:g722"},
			runner.Result{Name: "transfer", Status: "pass", Duration: time.Second},
		),
		Scenarios: []*scenario.Scenario{
			{Name: "basic", Area: "basic", Description: "Two-way\naudio."},
			{Name: "g722", Area: "codecs"},
			{Name: "transfer", Area: "transfers"},
		},
		Cases: []Case{
			{Source: "A", Order: 1, ID: "a1", Status: "covered"},
			{Source: "A", Order: 1, ID: "a2", Status: "planned", Milestone: "M3"},
			{Source: "B", Order: 2, ID: "b1", Status: "partial"},
			{Source: "B", Order: 2, ID: "b2", Status: "planned", Milestone: "M2"},
			{Source: "B", Order: 2, ID: "b3", Status: "not-applicable"},
		},
	}
}

func TestMarkdown(t *testing.T) {
	var b bytes.Buffer
	if err := Markdown(&b, testInput()); err != nil {
		t.Fatal(err)
	}
	md := b.String()
	for _, want := range []string{
		"at commit `abc1234`, run 17 November 2026 18:44 UTC",
		"| Edge on kind, through Kamailio to a 3-node `FreeswitchCluster` | 1 of 3 | 1 | 1 |",
		"| Compose, one FreeSWITCH (M0) | 2 of 3 | 0 | 1 |",
		"5 cases, 4 of which apply to this edge. Scenarios check 2 of those (50%): 1 fully and 1 in part. 2 are planned.",
		"| `basic` | basic | ✅ pass | 3.2 | ✅ pass | Two-way audio. |",
		"| `g722` | codecs | ➖ skip |  | ➖ skip | <br>*Kind: needs codec:g722*<br>*Compose: needs codec:g722* |",
		`| ❌ **fail** | 0.0 | ✅ pass | <br>*Kind: call 1: heard 0 Hz \| want 600* |`,
		"| B | 3 | 0 | 1 | 1 | 1 |",
		"| **All** | **5** | **1** | **1** | **2** | **1** |",
		"| M2 | 1 |\n| M3 | 1 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
}

func TestMarkdownWithoutCompose(t *testing.T) {
	in := testInput()
	in.Compose = nil
	var b bytes.Buffer
	if err := Markdown(&b, in); err != nil {
		t.Fatal(err)
	}
	if md := b.String(); strings.Contains(md, "Compose, one FreeSWITCH") || !strings.Contains(md, "| ✅ pass | 3.2 | — |") {
		t.Errorf("a card without a compose run shows no compose row and dashes in its column:\n%s", md)
	}
}

func TestScenariosCSV(t *testing.T) {
	var b bytes.Buffer
	if err := ScenariosCSV(&b, testInput()); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || strings.Join(rows[1], ",") != "1,basic,basic,pass,3.2,,pass,,Two-way audio." {
		t.Fatalf("got %q", rows)
	}
}

func TestLoadConformanceReadsTheRecord(t *testing.T) {
	cases, err := LoadConformance("../../conformance")
	if err != nil {
		t.Fatal(err)
	}
	all, bySource, _ := Coverage(cases)
	if bySource[0] != "sipbench scenarios" || all.Covered == 0 || all.Applicable() == 0 {
		t.Fatalf("sources %v, counts %+v", bySource, all)
	}
}
