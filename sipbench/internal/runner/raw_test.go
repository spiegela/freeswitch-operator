package runner

import (
	"errors"
	"strings"
	"testing"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/rawsip"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

func msg(t *testing.T, s string) rawsip.Message {
	t.Helper()
	m, _, err := rawsip.Parse([]byte(strings.ReplaceAll(s, "\n", "\r\n") + "\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRawChecks(t *testing.T) {
	req := msg(t, "INVITE sip:a@e SIP/2.0\nVia: SIP/2.0/UDP 10.0.0.1:5;branch=z9hG4bKx\nFrom: <sip:s@h>;tag=1\nTo: <sip:a@e>\nCall-ID: c\nCSeq: 1 INVITE")
	trying := msg(t, "SIP/2.0 100 Trying\nVia: SIP/2.0/UDP 10.0.0.1:5;branch=z9hG4bKx;received=172.18.0.4;rport=5\nFrom: <sip:s@h>;tag=1\nTo: <sip:a@e>\nCall-ID: c\nCSeq: 1 INVITE")
	trying.At = 5 * time.Millisecond
	refused := msg(t, "SIP/2.0 420 Bad Extension\nUnsupported: x-frob, y\nTo: <sip:a@e>;tag=9")
	refused.At = 6 * time.Millisecond
	got := []rawsip.Message{trying, refused}

	good := scenario.RawExpect{
		Status: 420, Provisional: []int{100}, WithinMs: 50,
		Headers: map[string]string{"unsupported": "X-FROB"},
		Copied:  []string{"Via", "From", "To", "Call-ID", "CSeq"},
	}
	if out := rawChecks(good, &req, nil, got, finalOf(got), false); len(out) != 0 {
		t.Errorf("good: %v", out)
	}

	bad := scenario.RawExpect{
		Status: 483, Provisional: []int{180}, WithinMs: 1,
		Headers: map[string]string{"Unsupported": "z", "Allow": ""},
		Copied:  []string{"Call-ID"},
	}
	trying.Headers[2].Value = "<sip:a@e>;tag=added" // a To tag on the 100 isn't a copy
	out := rawChecks(bad, &req, nil, []rawsip.Message{trying, refused}, &refused, false)
	if len(out) != 5 {
		t.Errorf("want 5 failures (status, provisional, timing, two headers), got %d: %v", len(out), out)
	}
	if out := rawChecks(scenario.RawExpect{Copied: []string{"To"}}, &req, nil, []rawsip.Message{trying}, nil, false); len(out) != 1 {
		t.Errorf("a To tag added to the 100 must fail copied: %v", out)
	}

	if out := rawChecks(scenario.RawExpect{NoResponse: true}, &req, nil, got, nil, false); len(out) != 1 {
		t.Errorf("noResponse with a response: %v", out)
	}
	if out := rawChecks(scenario.RawExpect{NoResponse: true}, &req, nil, nil, nil, false); len(out) != 0 {
		t.Errorf("noResponse with none: %v", out)
	}
	if out := rawChecks(scenario.RawExpect{Status: 400, Closed: true}, &req, nil, nil, nil, false); len(out) != 2 {
		t.Errorf("no response and still open: %v", out)
	}
	if out := rawChecks(scenario.RawExpect{Copied: []string{"Via"}}, nil, errors.New("bad"), got, nil, false); len(out) != 1 {
		t.Errorf("copied from an unparsable request: %v", out)
	}
}

func TestURIOf(t *testing.T) {
	for in, want := range map[string]string{
		`"FS" <sip:mod_sofia@10.0.0.1:5060;transport=udp>;expires=60`: "sip:mod_sofia@10.0.0.1:5060;transport=udp",
		"sip:a@b;lr": "sip:a@b",
	} {
		if got := uriOf(in); got != want {
			t.Errorf("uriOf(%q) = %q, want %q", in, got, want)
		}
	}
}
