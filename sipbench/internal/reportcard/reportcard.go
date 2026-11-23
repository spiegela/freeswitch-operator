// Package reportcard turns bench reports and the conformance record into the sipbench report card: a Markdown
// page for the repository and CSVs with the same rows for anything that charts them.
package reportcard

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/runner"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
)

// Case is one entry of the conformance record, with the source it came from.
type Case struct {
	Source    string `yaml:"-"`
	Order     int    `yaml:"-"`
	ID        string `yaml:"id"`
	Title     string `yaml:"title"`
	Area      string `yaml:"area"`
	Component string `yaml:"component"`
	Status    string `yaml:"status"`
	Milestone string `yaml:"milestone"`
}

// sources names each record file on the card, in the order the card lists them. Files not named here follow,
// under their source id.
var sources = []struct{ file, label string }{
	{"sipbench.yaml", "sipbench scenarios"},
	{"i3forum.yaml", "i3 Forum interop plan R2"},
	{"etsi-ts-102-027-2.yaml", "ETSI TS 102 027-2"},
	{"rfc4475.yaml", "RFC 4475 torture messages"},
	{"impairment.yaml", "Network impairment"},
}

// LoadConformance reads every record file in dir.
func LoadConformance(dir string) ([]Case, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no conformance record in %s", dir)
	}
	order := func(f string) int {
		for i, s := range sources {
			if s.file == filepath.Base(f) {
				return i
			}
		}
		return len(sources)
	}
	slices.SortStableFunc(files, func(a, b string) int { return order(a) - order(b) })
	var out []Case
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var rec struct {
			Source struct {
				ID string `yaml:"id"`
			} `yaml:"source"`
			Cases []Case `yaml:"cases"`
		}
		if err := yaml.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		label := rec.Source.ID
		if o := order(f); o < len(sources) {
			label = sources[o].label
		}
		for _, c := range rec.Cases {
			c.Source, c.Order = label, order(f)+1
			out = append(out, c)
		}
	}
	return out, nil
}

// LoadReport reads a JSON report written by sipbench run -json.
func LoadReport(path string) (*runner.Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r runner.Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(r.Results) == 0 {
		return nil, fmt.Errorf("%s: no results", path)
	}
	return &r, nil
}

// Input is everything a card is made from.
type Input struct {
	Kind      *runner.Report // through Kamailio on kind; required
	Compose   *runner.Report // one FreeSWITCH in compose; optional
	Scenarios []*scenario.Scenario
	Cases     []Case
	Commit    string // the commit the runs were made from
}

// Counts are a record's cases by status.
type Counts struct {
	Covered, Partial, Planned, NotApplicable int
}

func (c *Counts) add(status string) {
	switch status {
	case "covered":
		c.Covered++
	case "partial":
		c.Partial++
	case "planned":
		c.Planned++
	default:
		c.NotApplicable++
	}
}

// Total is every case.
func (c Counts) Total() int { return c.Covered + c.Partial + c.Planned + c.NotApplicable }

// Applicable leaves out cases that don't apply to this edge.
func (c Counts) Applicable() int { return c.Total() - c.NotApplicable }

// Checked is what a scenario checks, fully or in part.
func (c Counts) Checked() int { return c.Covered + c.Partial }

// Coverage counts the record overall and per source, in the card's order.
func Coverage(cases []Case) (all Counts, bySource []string, per map[string]*Counts) {
	per = map[string]*Counts{}
	for _, c := range cases {
		all.add(c.Status)
		if per[c.Source] == nil {
			per[c.Source] = &Counts{}
			bySource = append(bySource, c.Source)
		}
		per[c.Source].add(c.Status)
	}
	return all, bySource, per
}

// Milestones counts planned cases by milestone, in milestone order.
func Milestones(cases []Case) (names []string, n map[string]int) {
	n = map[string]int{}
	for _, c := range cases {
		if c.Status != "planned" {
			continue
		}
		if n[c.Milestone] == 0 {
			names = append(names, c.Milestone)
		}
		n[c.Milestone]++
	}
	slices.Sort(names)
	return names, n
}

// Write writes README.md, scenarios.csv and conformance.csv to dir.
func Write(dir string, in Input) error {
	if in.Kind == nil {
		return fmt.Errorf("no kind report")
	}
	for name, write := range map[string]func(io.Writer, Input) error{
		"README.md":       Markdown,
		"scenarios.csv":   ScenariosCSV,
		"conformance.csv": ConformanceCSV,
	} {
		if err := writeFile(filepath.Join(dir, name), func(w io.Writer) error { return write(w, in) }); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// row is one scenario across both runs.
type row struct {
	order       int
	name, area  string
	description string
	kind, comp  *runner.Result
}

func rows(in Input) []row {
	byName := func(r *runner.Report) map[string]*runner.Result {
		m := map[string]*runner.Result{}
		if r == nil {
			return m
		}
		for i := range r.Results {
			m[r.Results[i].Name] = &r.Results[i]
		}
		return m
	}
	kind, comp := byName(in.Kind), byName(in.Compose)
	var out []row
	seen := map[string]bool{}
	for i, s := range in.Scenarios {
		seen[s.Name] = true
		out = append(out, row{order: i + 1, name: s.Name, area: s.Area, description: oneLine(s.Description),
			kind: kind[s.Name], comp: comp[s.Name]})
	}
	// A result for a scenario no longer in the directory still shows.
	for _, r := range in.Kind.Results {
		if !seen[r.Name] {
			out = append(out, row{order: len(out) + 1, name: r.Name, area: r.Area, kind: kind[r.Name], comp: comp[r.Name]})
		}
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func status(r *runner.Result) string {
	if r == nil {
		return "not run"
	}
	return r.Status
}

func note(r *runner.Result) string {
	if r == nil {
		return ""
	}
	parts := []string{}
	if r.Reason != "" {
		parts = append(parts, r.Reason)
	}
	parts = append(parts, r.Failures...)
	return oneLine(strings.Join(parts, "; "))
}

func seconds(r *runner.Result) string {
	if r == nil || r.Status == "skip" {
		return ""
	}
	return strconv.FormatFloat(r.Duration.Seconds(), 'f', 1, 64)
}

func mark(s string) string {
	switch s {
	case "pass":
		return "✅ pass"
	case "fail":
		return "❌ **fail**"
	case "skip":
		return "➖ skip"
	}
	return s
}

func cell(s string) string { return strings.ReplaceAll(oneLine(s), "|", `\|`) }

func pct(n, of int) int {
	if of == 0 {
		return 0
	}
	return int(float64(n)/float64(of)*100 + 0.5)
}

// Markdown writes the card as a page.
func Markdown(w io.Writer, in Input) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# sipbench report card\n\n")
	p("<!-- Generated by `make report-card` (sipbench report-card). Don't edit by hand. -->\n\n")
	p("How the edge did against [sipbench](../README.md)'s scenarios")
	if in.Commit != "" {
		p(" at commit `%s`", in.Commit)
	}
	p(", run %s. The release workflow (`.github/workflows/report-card.yml`) regenerates it after every release.\n\n",
		in.Kind.StartedAt.UTC().Format("2 January 2006 15:04 MST"))

	p("| Run | Passed | Failed | Skipped |\n|---|---:|---:|---:|\n")
	run := func(label string, r *runner.Report) {
		if r != nil {
			p("| %s | %d of %d | %d | %d |\n", label, r.Pass, len(r.Results), r.Fail, r.Skip)
		}
	}
	run("Edge on kind, through Kamailio to a 3-node `FreeswitchCluster`", in.Kind)
	run("Compose, one FreeSWITCH (M0)", in.Compose)

	all, bySource, per := Coverage(in.Cases)
	p("\nThe [conformance record](../conformance/README.md) has %d cases, %d of which apply to this edge. "+
		"Scenarios check %d of those (%d%%): %d fully and %d in part. %d are planned.\n\n",
		all.Total(), all.Applicable(), all.Checked(), pct(all.Checked(), all.Applicable()), all.Covered, all.Partial,
		all.Planned)

	p("## Scenarios\n\n")
	p("Time is the scenario's wall time on kind. Notes give a skip's reason or the checks that failed.\n\n")
	p("| Scenario | Area | Kind | Time (s) | Compose | What it checks |\n|---|---|---|---:|---|---|\n")
	for _, r := range rows(in) {
		var desc strings.Builder
		desc.WriteString(cell(r.description))
		for _, n := range []struct{ label, text string }{{"Kind", note(r.kind)}, {"Compose", note(r.comp)}} {
			if n.text != "" {
				desc.WriteString("<br>*" + n.label + ": " + cell(n.text) + "*")
			}
		}
		comp := "—"
		if in.Compose != nil {
			comp = mark(status(r.comp))
		}
		p("| `%s` | %s | %s | %s | %s | %s |\n", r.name, cell(r.area), mark(status(r.kind)), seconds(r.kind), comp, desc.String())
	}

	p("\n## Conformance record\n\n")
	p("Covered: a scenario checks the whole case. Partial: a scenario checks some of it.\n\n")
	p("| Source | Cases | Covered | Partial | Planned | Not applicable |\n|---|---:|---:|---:|---:|---:|\n")
	for _, s := range bySource {
		c := per[s]
		p("| %s | %d | %d | %d | %d | %d |\n", cell(s), c.Total(), c.Covered, c.Partial, c.Planned, c.NotApplicable)
	}
	p("| **All** | **%d** | **%d** | **%d** | **%d** | **%d** |\n", all.Total(), all.Covered, all.Partial, all.Planned,
		all.NotApplicable)

	names, n := Milestones(in.Cases)
	p("\n## Planned cases by milestone\n\n| Milestone | Cases |\n|---|---:|\n")
	for _, m := range names {
		p("| %s | %d |\n", m, n[m])
	}
	p("\nThe rows behind this page are in [`scenarios.csv`](scenarios.csv) and [`conformance.csv`](conformance.csv); " +
		"the runs' full reports are in `kind.json` and `compose.json`.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// ScenariosCSV writes one row per scenario.
func ScenariosCSV(w io.Writer, in Input) error {
	c := csv.NewWriter(w)
	_ = c.Write([]string{"order", "scenario", "area", "kind_status", "kind_seconds", "kind_note", "compose_status",
		"compose_note", "description"})
	for _, r := range rows(in) {
		comp := ""
		if in.Compose != nil {
			comp = status(r.comp)
		}
		_ = c.Write([]string{strconv.Itoa(r.order), r.name, r.area, status(r.kind), seconds(r.kind), note(r.kind), comp,
			note(r.comp), r.description})
	}
	c.Flush()
	return c.Error()
}

// ConformanceCSV writes one row per case.
func ConformanceCSV(w io.Writer, in Input) error {
	c := csv.NewWriter(w)
	_ = c.Write([]string{"source_order", "source", "id", "area", "component", "status", "milestone", "title"})
	for _, x := range in.Cases {
		_ = c.Write([]string{strconv.Itoa(x.Order), x.Source, x.ID, x.Area, x.Component, x.Status, x.Milestone,
			oneLine(x.Title)})
	}
	c.Flush()
	return c.Error()
}
