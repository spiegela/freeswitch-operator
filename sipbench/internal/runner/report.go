package runner

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"
)

// Report is a whole run.
type Report struct {
	StartedAt time.Time `json:"startedAt"`
	Edge      string    `json:"edge"`
	Caps      []string  `json:"capabilities"`
	Results   []Result  `json:"results"`
	Pass      int       `json:"pass"`
	Fail      int       `json:"fail"`
	Skip      int       `json:"skip"`
}

// Add records a result and updates the counts.
func (r *Report) Add(res Result) {
	r.Results = append(r.Results, res)
	switch res.Status {
	case statusPass:
		r.Pass++
	case statusSkip:
		r.Skip++
	default:
		r.Fail++
	}
}

// WriteSummary writes a human summary.
func (r *Report) WriteSummary(w io.Writer) {
	for _, res := range r.Results {
		_, _ = fmt.Fprintf(w, "%-4s  %-32s %6.1fs", strings.ToUpper(res.Status), res.Name, res.Duration.Seconds())
		if res.Reason != "" {
			_, _ = fmt.Fprintf(w, "  (%s)", res.Reason)
		}
		_, _ = fmt.Fprintln(w)
		for _, f := range res.Failures {
			_, _ = fmt.Fprintf(w, "        - %s\n", f)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped\n", r.Pass, r.Fail, r.Skip)
}

// WriteJSON writes the full report.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     float64     `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// WriteJUnit writes a JUnit XML report for CI systems.
func (r *Report) WriteJUnit(w io.Writer) error {
	s := junitSuite{Name: "sipbench", Tests: len(r.Results), Failures: r.Fail, Skipped: r.Skip}
	for _, res := range r.Results {
		area := res.Area
		if area == "" {
			area = "scenarios"
		}
		c := junitCase{Name: res.Name, Classname: "sipbench." + area, Time: res.Duration.Seconds()}
		s.Time += c.Time
		switch res.Status {
		case statusSkip:
			c.Skipped = &junitMessage{Message: res.Reason}
		case statusPass:
		default:
			msg := res.Reason
			if msg == "" && len(res.Failures) > 0 {
				msg = res.Failures[0]
			}
			c.Failure = &junitMessage{Message: msg, Body: strings.Join(res.Failures, "\n")}
		}
		s.Cases = append(s.Cases, c)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
