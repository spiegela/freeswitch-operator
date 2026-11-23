package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/fleet"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
)

func TestSDPConnectionAddress(t *testing.T) {
	cases := map[string]struct{ sdp, want string }{
		"session only": {"v=0\r\no=- 1 1 IN IP4 10.0.0.9\r\ns=-\r\nc=IN IP4 203.0.113.5\r\nt=0 0\r\n" +
			"m=audio 4000 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\n", "203.0.113.5"},
		"media over session": {"v=0\r\nc=IN IP4 10.0.0.9\r\nt=0 0\r\nm=audio 4000 RTP/AVP 0\r\n" +
			"c=IN IP4 198.51.100.7\r\n", "198.51.100.7"},
		"audio section, not video": {"v=0\r\nc=IN IP4 10.0.0.9\r\nm=video 5000 RTP/AVP 96\r\nc=IN IP4 192.0.2.1\r\n" +
			"m=audio 4000 RTP/AVP 0\r\nc=IN IP4 198.51.100.7\r\n", "198.51.100.7"},
		"first audio section": {"v=0\r\nm=audio 4000 RTP/AVP 0\r\nc=IN IP4 198.51.100.7\r\n" +
			"m=audio 4002 RTP/AVP 0\r\nc=IN IP4 192.0.2.1\r\n", "198.51.100.7"},
		"LF only and a ttl": {"v=0\nc=IN IP4 233.252.0.1/127\nm=audio 4000 RTP/AVP 0\n", "233.252.0.1"},
		"no c= at all":      {"v=0\r\nm=audio 4000 RTP/AVP 0\r\n", ""},
		"malformed c=":      {"v=0\r\nc=IN IP4\r\nm=audio 4000 RTP/AVP 0\r\n", ""},
		"no body":           {"", ""},
	}
	for name, c := range cases {
		if got := sdpConnectionAddress([]byte(c.sdp)); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestMatchMediaAddress(t *testing.T) {
	pods := map[string]string{"edge-0": "203.0.113.5", "edge-1": "", "edge-2": "198.51.100.7"}
	pass := []struct{ want, got string }{
		{"203.0.113.5", "203.0.113.5"},
		{"::ffff:203.0.113.5", "203.0.113.5"}, // one address, written two ways
		{"fleet", "198.51.100.7"},
	}
	for _, c := range pass {
		if f := matchMediaAddress(c.want, c.got, pods); len(f) != 0 {
			t.Errorf("%s against %s: %v", c.got, c.want, f)
		}
	}
	fail := []struct{ want, got, says string }{
		{"203.0.113.5", "10.0.0.9", "media address 10.0.0.9, want 203.0.113.5"},
		{"203.0.113.5", "", "no connection address"},
		{"fleet", "", "no connection address"},
		{"fleet", "10.0.0.9", "(edge-0 203.0.113.5, edge-1 none, edge-2 198.51.100.7)"},
	}
	for _, c := range fail {
		f := matchMediaAddress(c.want, c.got, pods)
		if len(f) != 1 || !strings.Contains(f[0], c.says) {
			t.Errorf("%q against %s: %v, want one failure saying %q", c.got, c.want, f, c.says)
		}
	}
	if f := matchMediaAddress("fleet", "10.0.0.9", nil); len(f) != 1 || !strings.Contains(f[0], "no pods") {
		t.Errorf("an empty fleet: %v", f)
	}
}

// errFleet is a fleet whose addresses can't be read.
type errFleet struct{ *fakeFleet }

func (errFleet) ExternalAddresses(context.Context) (map[string]string, error) {
	return nil, errors.New("pods is forbidden")
}

func TestFleetAddressesCheck(t *testing.T) {
	ctx := context.Background()
	answered := CallResult{Status: 200, MediaAddress: "203.0.113.5"}

	none := &fleetAddresses{}
	if f := none.check(ctx, "", answered); len(f) != 0 {
		t.Errorf("no expectation: %v", f)
	}
	if f := none.check(ctx, "fleet", CallResult{Status: 486}); len(f) != 0 {
		t.Errorf("an unanswered call has no answer to check: %v", f)
	}
	if f := none.check(ctx, "203.0.113.5", answered); len(f) != 0 {
		t.Errorf("a literal needs no fleet: %v", f)
	}
	if f := none.check(ctx, "fleet", answered); len(f) != 1 || !strings.Contains(f[0], "-fleet") {
		t.Errorf("fleet without one: %v", f)
	}

	ff := newFakeFleet()
	ff.external = map[string]string{"edge-0": "203.0.113.5"}
	a := &fleetAddresses{fleet: ff}
	if f := a.check(ctx, "fleet", answered); len(f) != 0 {
		t.Errorf("the fleet's address: %v", f)
	}
	// Read once per scenario: a later change isn't seen by the same check.
	ff.external["edge-0"] = "192.0.2.1"
	if f := a.check(ctx, "fleet", answered); len(f) != 0 {
		t.Errorf("addresses should be read once per scenario: %v", f)
	}

	var bad fleet.Fleet = errFleet{newFakeFleet()}
	a = &fleetAddresses{fleet: bad}
	if f := a.check(ctx, "fleet", answered); len(f) != 1 || !strings.Contains(f[0], "pods is forbidden") {
		t.Errorf("an unreadable fleet: %v", f)
	}
}

// The runner records the answer's media address on every answered call, and checks it against the fleet's pods
// when asked. The mock answers from 127.0.0.1, edge-0's address in the fake fleet.
func TestMediaAddressAgainstFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("places real calls over loopback")
	}
	f := newFakeFleet()
	f.external = map[string]string{"edge-0": "127.0.0.1", "edge-1": "10.0.0.1"}
	r := withFleet(startMock(t, testRoutes), f)
	res := runScenario(t, r, `
name: media-address
requires: [mock, fleet]
calls:
  - id: a
    to: agent
    steps: [{tone: {hz: 600, ms: 600}}, {hangup: {}}]
    expect: {status: 200, tonesHeard: [1000], mediaAddress: fleet}
  - id: b
    to: agent
    steps: [{hangup: {}}]
    expect: {status: 200}
`)
	for _, c := range res.Calls {
		if c.MediaAddress != "127.0.0.1" {
			t.Errorf("call %s: media address %q recorded, want 127.0.0.1", c.ID, c.MediaAddress)
		}
	}

	f.external = map[string]string{"edge-0": "203.0.113.5"}
	s, err := scenario.Parse([]byte(`
name: media-address-elsewhere
requires: [mock, fleet]
calls:
  - {id: a, to: agent, steps: [{hangup: {}}], expect: {status: 200, mediaAddress: fleet}}
`))
	if err != nil {
		t.Fatal(err)
	}
	res = r.Run(context.Background(), s)
	if res.Status != statusFail || !strings.Contains(strings.Join(res.Failures, "\n"), "edge-0 203.0.113.5") {
		t.Errorf("an address the fleet doesn't have should fail: %s %v", res.Status, res.Failures)
	}
}
