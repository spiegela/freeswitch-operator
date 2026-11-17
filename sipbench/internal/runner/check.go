package runner

import (
	"fmt"
	"slices"
	"strings"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/uas"
)

// check compares what a call saw against its expectations and returns each mismatch.
func check(c scenario.Call, cr CallResult, haveMock bool) []string {
	var out []string
	e := c.Expect
	if cr.Error != "" {
		out = append(out, "error: "+cr.Error)
	}
	if cr.Status != e.Status {
		out = append(out, fmt.Sprintf("status %d, want %d", cr.Status, e.Status))
	}
	for k, want := range e.ResponseHeaders {
		got, ok := cr.ResponseHeaders[strings.ToLower(k)]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("response header %s missing", k))
		case want != "" && got != want:
			out = append(out, fmt.Sprintf("response header %s = %q, want %q", k, got, want))
		}
	}
	out = append(out, tones("caller", cr.TonesHeard, e.TonesHeard, e.TonesNotHeard)...)
	if e.Digits != nil && cr.Digits != *e.Digits {
		out = append(out, fmt.Sprintf("caller digits %q, want %q", cr.Digits, *e.Digits))
	}
	if e.EndedBy != "" && cr.EndedBy != e.EndedBy {
		out = append(out, fmt.Sprintf("ended by %q, want %q", cr.EndedBy, e.EndedBy))
	}
	if e.Codec != "" && !strings.EqualFold(cr.Codec, e.Codec) {
		out = append(out, fmt.Sprintf("caller codec %s, want %s", cr.Codec, e.Codec))
	}
	if e.Backend != nil && haveMock {
		out = append(out, end("backend", cr.Backend, *e.Backend)...)
	}
	if e.Target != nil {
		out = append(out, end("target", cr.Target, *e.Target)...)
	}
	return out
}

func end(name string, calls []uas.Call, e scenario.EndExpect) []string {
	var out []string
	want := 1
	if e.Calls != nil {
		want = *e.Calls
	}
	if len(calls) != want {
		out = append(out, fmt.Sprintf("%s saw %d legs, want %d", name, len(calls), want))
	}
	if want == 0 || len(calls) == 0 {
		return out
	}
	c := calls[len(calls)-1]
	for k, v := range e.Headers {
		got, ok := lookup(c.Headers, k)
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s did not receive header %s", name, k))
		case v != "" && got != v:
			out = append(out, fmt.Sprintf("%s header %s = %q, want %q", name, k, got, v))
		}
	}
	out = append(out, tones(name, c.TonesHeard, e.TonesHeard, nil)...)
	if e.Digits != nil && c.Digits != *e.Digits {
		out = append(out, fmt.Sprintf("%s digits %q, want %q", name, c.Digits, *e.Digits))
	}
	if e.Codec != "" && !strings.EqualFold(c.Codec, e.Codec) {
		out = append(out, fmt.Sprintf("%s codec %s, want %s", name, c.Codec, e.Codec))
	}
	if e.Answered != nil && c.Answered != *e.Answered {
		out = append(out, fmt.Sprintf("%s answered=%v, want %v", name, c.Answered, *e.Answered))
	}
	if e.EndedBy != "" && c.EndedBy != e.EndedBy {
		out = append(out, fmt.Sprintf("%s ended by %q, want %q", name, c.EndedBy, e.EndedBy))
	}
	if e.ReferAccepted != nil {
		got := c.Refer != nil && c.Refer.Accepted
		if got != *e.ReferAccepted {
			detail := ""
			if c.Refer != nil && c.Refer.Error != "" {
				detail = ": " + c.Refer.Error
			}
			out = append(out, fmt.Sprintf("%s REFER accepted=%v, want %v%s", name, got, *e.ReferAccepted, detail))
		}
	}
	for _, err := range c.Errors {
		out = append(out, fmt.Sprintf("%s error: %s", name, err))
	}
	return out
}

func tones(name string, heard, want, notWant []float64) []string {
	var out []string
	for _, hz := range want {
		if !slices.Contains(heard, hz) {
			out = append(out, fmt.Sprintf("%s did not hear %g Hz (heard %v)", name, hz, heard))
		}
	}
	for _, hz := range notWant {
		if slices.Contains(heard, hz) {
			out = append(out, fmt.Sprintf("%s heard %g Hz, which it should not have", name, hz))
		}
	}
	return out
}

func lookup(h map[string]string, k string) (string, bool) {
	for name, v := range h {
		if strings.EqualFold(name, k) {
			return v, true
		}
	}
	return "", false
}
