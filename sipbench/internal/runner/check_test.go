package runner

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/lkagent"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/uas"
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

func TestInfos(t *testing.T) {
	const ct = "application/x-barge-in+json"
	got := []uas.Info{
		{ContentType: ct, Body: `{"event":"speech-started","party":"caller","probability":0.97}`},
		{ContentType: ct, Body: `{"event":"speech-stopped","party":"caller"}`},
		{ContentType: "text/plain", Body: "hello"},
	}
	ok := []scenario.InfoExpect{
		{ContentType: ct, Fields: map[string]string{"party": "caller", "event": "speech-started"}},
		{ContentType: "APPLICATION/X-BARGE-IN+JSON", Min: ptr(2), Max: ptr(2)},
		{Fields: map[string]string{"probability": "0.97"}},
		{ContentType: ct, Fields: map[string]string{"party": "agent"}, Max: ptr(0)},
		{}, // any INFO at all
	}
	if f := infos("backend", got, ok); len(f) != 0 {
		t.Errorf("expected no failures, got %v", f)
	}
	bad := []scenario.InfoExpect{
		{ContentType: ct, Fields: map[string]string{"party": "agent"}},
		{ContentType: ct, Max: ptr(1)},
		{Fields: map[string]string{"event": "speech-started"}, Min: ptr(2)},
	}
	if f := infos("backend", got, bad); len(f) != 3 {
		t.Errorf("want 3 failures, got %v", f)
	}
	if f := infos("backend", nil, ok[:1]); len(f) != 1 || !strings.Contains(f[0], "received none") {
		t.Errorf("no INFOs at all: got %v", f)
	}
}

func TestLiveKit(t *testing.T) {
	got := []lkagent.Participant{{
		Identity:   "sip_1001",
		Attributes: map[string]string{lkagent.BenchCallAttribute: "b1", lkagent.BargeInAttribute: "freeswitch"},
		Data: []lkagent.Data{
			{Topic: "barge-in", Payload: `{"event":"speech-started","party":"caller"}`},
			{Topic: "barge-in", Payload: `{"event":"speech-stopped","party":"caller"}`},
			{Topic: "other", Payload: `{"event":"speech-started"}`},
		},
	}}
	ok := scenario.LiveKitExpect{
		Attributes: map[string]string{lkagent.BargeInAttribute: "freeswitch"},
		Data: []scenario.DataExpect{
			{Topic: "barge-in", Fields: map[string]string{"party": "caller", "event": "speech-started"}},
			{Topic: "barge-in", Min: ptr(2), Max: ptr(2)},
			{Fields: map[string]string{"event": "speech-started"}, Min: ptr(2)},
			{Topic: "barge-in", Fields: map[string]string{"party": "agent"}, Max: ptr(0)},
		},
	}
	if f := livekit(got, ok); len(f) != 0 {
		t.Errorf("expected no failures, got %v", f)
	}
	bad := scenario.LiveKitExpect{
		Attributes: map[string]string{lkagent.BargeInAttribute: "livekit", "missing": "x"},
		Data: []scenario.DataExpect{
			{Topic: "barge-in", Fields: map[string]string{"party": "agent"}},
			{Topic: "barge-in", Max: ptr(1)},
		},
	}
	if f := livekit(got, bad); len(f) != 4 {
		t.Errorf("want 4 failures, got %v", f)
	}
	if f := livekit(nil, ok); len(f) != 1 || !strings.Contains(f[0], "no participant") {
		t.Errorf("no participant: got %v", f)
	}
	no := false
	if f := livekit(got, scenario.LiveKitExpect{Joined: &no}); len(f) != 1 {
		t.Errorf("joined when it should not have: got %v", f)
	}
	if f := livekit(nil, scenario.LiveKitExpect{Joined: &no}); len(f) != 0 {
		t.Errorf("none joined, as wanted: got %v", f)
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
