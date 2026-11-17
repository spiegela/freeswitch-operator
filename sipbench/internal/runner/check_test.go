package runner

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/uas"
)

func ptr[T any](v T) *T { return &v }

func TestCheck(t *testing.T) {
	call := scenario.Call{Expect: scenario.Expect{
		Status:          200,
		ResponseHeaders: map[string]string{"X-Fallback-Reason": "USER_BUSY", "X-Any": ""},
		TonesHeard:      []float64{1400},
		TonesNotHeard:   []float64{1000},
		Digits:          ptr("12"),
		EndedBy:         "remote",
		Codec:           "pcmu",
		Backend: &scenario.EndExpect{
			Headers:       map[string]string{"x-conversation-id": "abc"},
			Answered:      ptr(false),
			ReferAccepted: ptr(true),
		},
		Target: &scenario.EndExpect{Calls: ptr(0)},
	}}
	good := CallResult{
		Status:          200,
		ResponseHeaders: map[string]string{"x-fallback-reason": "USER_BUSY", "x-any": "whatever"},
		TonesHeard:      []float64{1400},
		Digits:          "12",
		EndedBy:         "remote",
		Codec:           "PCMU",
		Backend: []uas.Call{{
			Headers: map[string]string{"X-Conversation-Id": "abc"},
			Refer:   &uas.ReferResult{Accepted: true},
		}},
	}
	if f := check(call, good, true); len(f) != 0 {
		t.Fatalf("expected no failures, got %v", f)
	}

	bad := good
	bad.Status = 486
	bad.ResponseHeaders = map[string]string{"x-fallback-reason": "NO_ANSWER"}
	bad.TonesHeard = []float64{1000}
	bad.Digits = "1"
	bad.Codec = "PCMA"
	bad.Backend = []uas.Call{{Answered: true, Headers: map[string]string{}}}
	bad.Target = []uas.Call{{}}
	f := check(call, bad, true)
	for _, want := range []string{"status 486", "X-Fallback-Reason", "X-Any missing", "did not hear 1400", "heard 1000", "digits", "codec", "header x-conversation-id", "answered=true", "REFER", "target saw 1 legs"} {
		found := false
		for _, s := range f {
			if strings.Contains(s, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no failure mentioning %q in %v", want, f)
		}
	}

	// Without the mock, backend expectations are not checked.
	nb := good
	nb.Backend = nil
	if f := check(call, nb, false); len(f) != 0 {
		t.Errorf("backend checked without a mock: %v", f)
	}
}

func TestReports(t *testing.T) {
	rep := &Report{}
	rep.Add(Result{Name: "a", Status: "pass"})
	rep.Add(Result{Name: "b", Status: "fail", Failures: []string{"call a: status 486, want 200"}})
	rep.Add(Result{Name: "c", Status: "skip", Reason: "needs codec:g722"})
	if rep.Pass != 1 || rep.Fail != 1 || rep.Skip != 1 {
		t.Fatalf("counts %d/%d/%d", rep.Pass, rep.Fail, rep.Skip)
	}
	var sum bytes.Buffer
	rep.WriteSummary(&sum)
	if !strings.Contains(sum.String(), "1 passed, 1 failed, 1 skipped") || !strings.Contains(sum.String(), "status 486") {
		t.Errorf("summary:\n%s", sum.String())
	}
	var j bytes.Buffer
	if err := rep.WriteJUnit(&j); err != nil {
		t.Fatal(err)
	}
	var suite junitSuite
	if err := xml.Unmarshal(j.Bytes(), &suite); err != nil {
		t.Fatalf("junit is not valid XML: %v", err)
	}
	if suite.Tests != 3 || suite.Failures != 1 || suite.Skipped != 1 || suite.Cases[1].Failure == nil || suite.Cases[2].Skipped == nil {
		t.Errorf("junit: %+v", suite)
	}
}
