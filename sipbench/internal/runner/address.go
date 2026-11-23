package runner

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/fleet"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
)

// sdpConnectionAddress returns the address an SDP body says to send audio to: the c= line of its first audio
// section, else the session's c= line. "" when it has neither.
//
// Not diago's media/sdp, nor the remote address of its media session (built from it): that parser flattens the
// sections and prefers a session-level c= over the audio section's, where RFC 4566 says the media level wins.
// An edge that puts its pool address on the audio section only would be checked against the wrong address.
func sdpConnectionAddress(body []byte) string {
	var session, media string
	section := "" // "" is the session level; otherwise the m= line's media type
	audioDone := false
	for line := range strings.Lines(string(body)) {
		line = strings.TrimRight(line, "\r\n")
		if m, ok := strings.CutPrefix(line, "m="); ok {
			if section == "audio" {
				audioDone = true
			}
			section, _, _ = strings.Cut(m, " ")
			continue
		}
		c, ok := strings.CutPrefix(line, "c=")
		if !ok {
			continue
		}
		f := strings.Fields(c) // nettype addrtype address[/ttl[/count]]
		if len(f) < 3 {
			continue
		}
		addr, _, _ := strings.Cut(f[2], "/")
		switch {
		case section == "":
			session = addr
		case section == "audio" && !audioDone && media == "":
			media = addr
		}
	}
	if media != "" {
		return media
	}
	return session
}

// fleetAddresses reads the fleet's external addresses once, the first time a call's check needs them: after the
// calls, so a pod replaced during the scenario counts with its replacement's address.
type fleetAddresses struct {
	fleet fleet.Fleet
	read  bool
	byPod map[string]string
	err   error
}

// check compares the media address a call's answer gave against want (an IP address, scenario.MediaAddressFleet,
// or "" for no check). Only an answered call has an answer to check; the status check reports the others.
func (a *fleetAddresses) check(ctx context.Context, want string, cr CallResult) []string {
	if want == "" || cr.Status != 200 {
		return nil
	}
	if want == scenario.MediaAddressFleet {
		if a.fleet == nil {
			return []string{"mediaAddress: fleet needs the runner's fleet (-fleet)"}
		}
		if !a.read {
			a.read = true
			a.byPod, a.err = a.fleet.ExternalAddresses(ctx)
		}
		if a.err != nil {
			return []string{fmt.Sprintf("reading %s's external addresses: %v", a.fleet.Name(), a.err)}
		}
	}
	return matchMediaAddress(want, cr.MediaAddress, a.byPod)
}

// matchMediaAddress checks got, the answer's media address, against want: an IP address, or
// scenario.MediaAddressFleet, any of byPod's addresses.
func matchMediaAddress(want, got string, byPod map[string]string) []string {
	if got == "" {
		return []string{"the answer's SDP gave no connection address"}
	}
	if want != scenario.MediaAddressFleet {
		if !sameIP(got, want) {
			return []string{fmt.Sprintf("media address %s, want %s", got, want)}
		}
		return nil
	}
	var have []string
	for pod, addr := range byPod {
		if addr != "" && sameIP(got, addr) {
			return nil
		}
		if addr == "" {
			addr = "none"
		}
		have = append(have, pod+" "+addr)
	}
	if len(have) == 0 {
		return []string{fmt.Sprintf("media address %s, and the fleet has no pods to compare it with", got)}
	}
	slices.Sort(have)
	return []string{fmt.Sprintf("media address %s is no fleet pod's external address (%s)", got, strings.Join(have, ", "))}
}

// sameIP compares two addresses as IPs, so differently written forms of one address match.
func sameIP(a, b string) bool {
	x, y := net.ParseIP(a), net.ParseIP(b)
	if x == nil || y == nil {
		return a == b
	}
	return x.Equal(y)
}
