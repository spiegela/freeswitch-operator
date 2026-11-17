package scenario

import (
	"strings"
	"testing"
)

func TestShippedScenariosLoad(t *testing.T) {
	ss, err := LoadDir("../../scenarios")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) < 10 {
		t.Fatalf("loaded %d scenarios, expected the full set", len(ss))
	}
	names := map[string]bool{}
	for _, s := range ss {
		if names[s.Name] {
			t.Errorf("duplicate scenario name %q", s.Name)
		}
		names[s.Name] = true
	}
}

func TestParse(t *testing.T) {
	s, err := Parse([]byte(`
name: x
requires: [mock, codec:g722]
calls:
  - id: a
    to: agent
    codecs: [pcma]
    headers: {X-A: "1"}
    steps:
      - tone: {hz: 600, ms: 100}
      - digits: {digits: "12#"}
      - hangup: {}
    expect:
      status: 200
      digits: ""
      backend: {calls: 0}
`))
	if err != nil {
		t.Fatal(err)
	}
	c := s.Calls[0]
	if c.Expect.Digits == nil || *c.Expect.Digits != "" {
		t.Errorf("an explicit empty digits expectation must be kept")
	}
	if c.Expect.Backend.Calls == nil || *c.Expect.Backend.Calls != 0 {
		t.Errorf("calls: 0 must be kept")
	}
	if got := s.Missing(map[string]bool{"mock": true}); len(got) != 1 || got[0] != "codec:g722" {
		t.Errorf("Missing = %v", got)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":      "name: x\nbogus: 1\noptions: {expectStatus: 200}",
		"no name":            "options: {expectStatus: 200}",
		"nothing to do":      "name: x",
		"missing status":     "name: x\ncalls: [{id: a, to: b}]",
		"duplicate id":       "name: x\ncalls: [{id: a, to: b, expect: {status: 200}}, {id: a, to: b, expect: {status: 200}}]",
		"unknown codec":      "name: x\ncalls: [{id: a, to: b, codecs: [opus], expect: {status: 200}}]",
		"answer from caller": "name: x\ncalls: [{id: a, to: b, steps: [{answer: {}}], expect: {status: 200}}]",
		"bad endedBy":        "name: x\ncalls: [{id: a, to: b, expect: {status: 200, endedBy: both}}]",
		"empty route":        "name: x\noptions: {expectStatus: 200}\nbackend: {routes: {r: {steps: []}}}",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: empty error", name)
		}
	}
}
