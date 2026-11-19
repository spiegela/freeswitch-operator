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

const fleetCalls = "name: x\nrequires: [fleet]\ncalls: [{id: a, to: b, expect: {status: 200}}]\n"

func TestParseActions(t *testing.T) {
	s, err := Parse([]byte(`
name: x
requires: [mock, fleet]
calls:
  - {id: a, to: agent, expect: {status: 200}}
  - {id: b, to: agent, startAfter: drained, expect: {status: 200, backend: {otherNodeThan: a}}}
actions:
  - {name: drained, afterAnswered: a, afterMs: 100, drainNodeOf: a}
  - {waitMember: {of: a, state: Draining}}
  - {hangup: a}
  - {uncordon: {}}
  - {rollFleet: {}}
  - {breakConfig: {configMap: c, key: k, replace: x, with: y}}
  - {expectRollStuck: {forMs: 10}}
  - {restoreConfig: {}}
  - {afterEnded: a, waitFleetCurrent: {timeoutMs: 5}}
`))
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(s.Actions))
	for i := range s.Actions {
		kinds = append(kinds, s.Actions[i].Kind())
	}
	want := "drainNodeOf waitMember hangup uncordon rollFleet breakConfig expectRollStuck restoreConfig waitFleetCurrent"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("kinds = %s", got)
	}
	if !s.NeedsFleet() {
		t.Errorf("NeedsFleet = false")
	}
	hangupOnly, err := Parse([]byte("name: y\ncalls: [{id: a, to: b, expect: {status: 200}}]\nactions: [{afterMs: 5, hangup: a}]"))
	if err != nil {
		t.Fatal(err)
	}
	if hangupOnly.NeedsFleet() {
		t.Errorf("hanging up a call doesn't need the fleet")
	}
}

func TestParseMediaAddress(t *testing.T) {
	for _, addr := range []string{"203.0.113.5", "2001:db8::5"} {
		if _, err := Parse([]byte("name: x\ncalls: [{id: a, to: b, expect: {status: 200, mediaAddress: '" + addr + "'}}]")); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	s, err := Parse([]byte(fleetCalls))
	if err != nil {
		t.Fatal(err)
	}
	if s.NeedsFleet() {
		t.Errorf("requiring fleet alone doesn't need it")
	}
	s, err = Parse([]byte("name: x\nrequires: [fleet]\ncalls: [{id: a, to: b, expect: {status: 200, mediaAddress: fleet}}]"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.NeedsFleet() {
		t.Errorf("checking media addresses against the fleet needs it")
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
		"two kinds":          fleetCalls + "actions: [{rollFleet: {}, hangup: a}]",
		"no kind":            fleetCalls + "actions: [{afterMs: 5}]",
		"unknown call":       fleetCalls + "actions: [{drainNodeOf: b}]",
		"unknown trigger":    fleetCalls + "actions: [{afterAnswered: b, rollFleet: {}}]",
		"bad member state":   fleetCalls + "actions: [{waitMember: {of: a, state: Sleepy}}]",
		"no stuck time":      fleetCalls + "actions: [{expectRollStuck: {}}]",
		"break nothing":      fleetCalls + "actions: [{breakConfig: {configMap: c, key: k, replace: x, with: x}}]",
		"duplicate action":   fleetCalls + "actions: [{name: n, rollFleet: {}}, {name: n, uncordon: {}}]",
		"start after none":   "name: x\nrequires: [fleet]\ncalls: [{id: a, to: b, startAfter: n, expect: {status: 200}}]",
		"fleet unrequired":   "name: x\ncalls: [{id: a, to: b, expect: {status: 200}}]\nactions: [{rollFleet: {}}]",
		"other node of self": "name: x\ncalls: [{id: a, to: b, expect: {status: 200, backend: {otherNodeThan: a}}}]",
		"other node target":  "name: x\ncalls: [{id: a, to: b, expect: {status: 200, target: {otherNodeThan: a}}}]",
		"raw no expect":      "name: x\nraw: [{id: r, message: OPTIONS}]",
		"raw no message":     "name: x\nraw: [{id: r, expect: {status: 200}}]",
		"raw no id":          "name: x\nraw: [{message: OPTIONS, expect: {status: 200}}]",
		"raw duplicate":      "name: x\nraw: [{id: r, message: A, expect: {status: 200}}, {id: r, message: B, expect: {status: 200}}]",
		"raw bad transport":  "name: x\nraw: [{id: r, transport: sctp, message: A, expect: {status: 200}}]",
		"raw closed on udp":  "name: x\nraw: [{id: r, message: A, expect: {closed: true}}]",
		"raw none and code":  "name: x\nraw: [{id: r, message: A, expect: {noResponse: true, status: 200}}]",
		"netem unrequired": "name: x\ncalls: [{id: a, to: b, expect: {status: 200}}]\n" +
			"impairments: [{link: carrier-edge, direction: both, traffic: rtp, loss: 5%}]",
		"drop unrequired": "name: x\nrequires: [netem]\ncalls: [{id: a, to: b, expect: {status: 200}}]\n" +
			"impairments: [{link: carrier-edge, direction: to-edge, traffic: sip, drop: first INVITE}]",
		"sip unrequired":           "name: x\ncalls: [{id: a, to: b, expect: {status: 200, sip: [{out: INVITE, min: 2}]}}]",
		"media address name":       "name: x\ncalls: [{id: a, to: b, expect: {status: 200, mediaAddress: edge.example}}]",
		"media address pool":       "name: x\nrequires: [fleet]\ncalls: [{id: a, to: b, expect: {status: 200, mediaAddress: pool}}]",
		"media address unrequired": "name: x\ncalls: [{id: a, to: b, expect: {status: 200, mediaAddress: fleet}}]",
		"reorder alone": "name: x\nrequires: [netem]\ncalls: [{id: a, to: b, expect: {status: 200}}]\n" +
			"impairments: [{link: carrier-edge, direction: both, traffic: rtp, reorder: 5%}]",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: empty error", name)
		}
	}
}
