package netem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RTP ports: the runner's media uses only these (it sets diago's range to them), so tc tells media from signalling
// by the runner's own port. The range is aligned to its size so one u32 mask matches it.
const (
	RTPPortBase = 0x5000 // 20480
	RTPPortMask = 0xf000 // 4096 ports: 20480-24575
	RTPPortLast = RTPPortBase + (^RTPPortMask & 0xffff)
)

// Interface names the shaper creates for impairing what arrives: tc can only delay what leaves an interface, so
// matching arrivals are redirected out of ifbIn, a veth, and come back in through its peer ifbPeer. (The ifb device
// does this job usually; the Docker Desktop kernel has no ifb.)
const (
	ifbIn   = "sbnetem0"
	ifbPeer = "sbnetem1"
)

// Words the tc and ip commands repeat.
const (
	tcAdd         = "add"
	tcDel         = "del"
	tcParent      = "parent"
	ingressHandle = "ffff:" // the ingress qdisc's handle
)

// Shaper applies tc impairments to the runner's own traffic on one interface: the carrier-edge link, with the
// runner as the carrier. Out is what the interface sends (to the edge), in is what it receives.
//
// Traffic classes, by port: sip is UDP or TCP to or from the edge's SIP port; rtp is UDP to or from the runner's
// RTP ports (RTPPortBase..RTPPortLast); all is both. Nothing else (DNS, the mock's API, the Kubernetes API) is
// touched.
type Shaper struct {
	// Iface is the interface toward the edge (see IfaceToward).
	Iface string
	// SIPPort is the edge's SIP port.
	SIPPort int
	// Exec runs a command (tc or ip); nil runs it for real.
	Exec func(ctx context.Context, args ...string) error
}

// Plan is the commands that put a set of impairments in place: Setup builds the qdiscs and filters (with every
// impairment off), On[i] and Off[i] turn impairment i on and off, and Teardown removes it all.
type Plan struct {
	Setup    [][]string
	On, Off  [][][]string
	Teardown [][]string
}

// Plan returns the commands for ims, which must all be tc impairments on the carrier-edge link.
//
// Each direction gets a prio qdisc with a band per impairment (band 1 carries everything unmatched): u32 filters
// pick each impairment's traffic into its band, and turning an impairment on hangs a netem qdisc off its band.
func (s *Shaper) Plan(ims []*Impairment) (Plan, error) {
	var p Plan
	if len(ims) == 0 {
		return p, nil
	}
	if len(ims) > 15 {
		return p, fmt.Errorf("at most 15 tc impairments at once, got %d", len(ims))
	}
	in := false
	for _, im := range ims {
		if im.Link != LinkCarrierEdge || im.SIPLayer() {
			return p, fmt.Errorf("%s: not a tc impairment on the carrier-edge link", im)
		}
		in = in || im.In
	}
	p.Teardown = s.teardown()
	priomap := slices.Repeat([]string{"0"}, 16) // everything unmatched: band 1
	bands := strconv.Itoa(len(ims) + 1)

	out := qdisc(tcAdd, s.Iface, "root", "handle", "1:", "prio", "bands", bands, "priomap")
	p.Setup = append(p.Setup, append(out, priomap...))
	if in {
		p.Setup = append(p.Setup,
			ipLink(tcAdd, ifbIn, "type", "veth", "peer", "name", ifbPeer),
			ipLink("set", ifbIn, "up"),
			ipLink("set", ifbPeer, "up"),
			// What comes back in through the peer is addressed to Iface's MAC: deliver it to this host anyway.
			qdisc(tcAdd, ifbPeer, "handle", ingressHandle, "ingress"),
			tcFilter(ifbPeer, tcParent, ingressHandle, "matchall", "action", "skbedit", "ptype", "host"),
			append(qdisc(tcAdd, ifbIn, "root", "handle", "1:", "prio", "bands", bands, "priomap"), priomap...),
			qdisc(tcAdd, s.Iface, "handle", ingressHandle, "ingress"),
		)
	}
	for i, im := range ims {
		band := strconv.Itoa(i + 2)
		prio := strconv.Itoa(i + 1)
		var on, off [][]string
		if im.Out {
			for _, m := range s.matches(im.Traffic, true) {
				p.Setup = append(p.Setup, filter(s.Iface, "1:", prio, m, "flowid", "1:"+band))
			}
			on = append(on, netemCmd(tcAdd, s.Iface, band, im))
			off = append(off, qdisc(tcDel, s.Iface, tcParent, "1:"+band))
		}
		if im.In {
			for _, m := range s.matches(im.Traffic, false) {
				p.Setup = append(p.Setup,
					filter(s.Iface, ingressHandle, prio, m, "action", "mirred", "egress", "redirect", "dev", ifbIn),
					filter(ifbIn, "1:", prio, m, "flowid", "1:"+band))
			}
			on = append(on, netemCmd(tcAdd, ifbIn, band, im))
			off = append(off, qdisc(tcDel, ifbIn, tcParent, "1:"+band))
		}
		p.On = append(p.On, on)
		p.Off = append(p.Off, off)
	}
	return p, nil
}

func (s *Shaper) teardown() [][]string {
	return [][]string{
		qdisc(tcDel, s.Iface, "root"),
		qdisc(tcDel, s.Iface, "ingress"),
		ipLink(tcDel, ifbIn),
	}
}

// matches returns the u32 match sets (one filter each) for a traffic class, out of or into the interface.
func (s *Shaper) matches(traffic string, out bool) [][]string {
	sipField, rtpField := "dport", "sport" // out: to the edge's SIP port, from our RTP ports
	if !out {
		sipField, rtpField = "sport", "dport"
	}
	sipPort := strconv.Itoa(s.SIPPort)
	sip := [][]string{
		u32Port("17", sipField, sipPort, "0xffff"),
		u32Port("6", sipField, sipPort, "0xffff"),
	}
	rtp := [][]string{
		u32Port("17", rtpField, strconv.Itoa(RTPPortBase), fmt.Sprintf("0x%x", RTPPortMask)),
	}
	switch traffic {
	case TrafficSIP:
		return sip
	case TrafficRTP:
		return rtp
	}
	return slices.Concat(sip, rtp)
}

// u32Port matches an IP protocol (17 UDP, 6 TCP) and a port field (sport, dport) under a mask.
func u32Port(proto, field, port, mask string) []string {
	return []string{"match", "ip", "protocol", proto, "0xff", "match", "ip", field, port, mask}
}

func ipLink(args ...string) []string { return append([]string{"ip", "link"}, args...) }

func qdisc(verb, dev string, args ...string) []string {
	return append([]string{"tc", "qdisc", verb, "dev", dev}, args...)
}

func tcFilter(dev string, args ...string) []string {
	return append([]string{"tc", "filter", tcAdd, "dev", dev}, args...)
}

func filter(dev, parent, prio string, match []string, then ...string) []string {
	cmd := tcFilter(dev, tcParent, parent, "protocol", "ip", "prio", prio, "u32")
	cmd = append(cmd, match...)
	return append(cmd, then...)
}

// netemCmd hangs a netem qdisc with im's settings off a band.
func netemCmd(verb, dev, band string, im *Impairment) []string {
	return append(qdisc(verb, dev, tcParent, "1:"+band, "netem"), NetemArgs(im)...)
}

// NetemArgs is what netem is told for im.
func NetemArgs(im *Impairment) []string {
	var a []string
	if im.Delay > 0 || im.Jitter > 0 {
		d := im.Delay
		if d == 0 {
			d = im.Jitter // jitter alone: centre it there, so nothing goes out early
		}
		a = append(a, "delay", us(d))
		if im.Jitter > 0 {
			a = append(a, us(im.Jitter))
		}
	}
	if im.LossPct > 0 {
		if im.Burst >= 1 {
			// Gilbert-Elliott, every packet in the bad state lost: a mean run of Burst packets leaves the bad
			// state with r = 1/Burst, and a loss rate L needs p/(p+r) = L, so p = L*r/(1-L).
			l := im.LossPct / 100
			r := 1 / im.Burst
			p := l * r / (1 - l)
			a = append(a, "loss", "gemodel", pct(p*100), pct(r*100))
		} else {
			a = append(a, "loss", pct(im.LossPct))
		}
	}
	if im.DuplicatePct > 0 {
		a = append(a, "duplicate", pct(im.DuplicatePct))
	}
	if im.ReorderPct > 0 {
		a = append(a, "reorder", pct(im.ReorderPct))
	}
	return a
}

func us(d time.Duration) string { return strconv.FormatInt(d.Microseconds(), 10) + "us" }

func pct(v float64) string {
	return strconv.FormatFloat(math.Round(v*10000)/10000, 'f', -1, 64) + "%"
}

// Run runs cmds in order and stops at the first failure.
func (s *Shaper) Run(ctx context.Context, cmds [][]string) error {
	for _, c := range cmds {
		if err := s.exec(ctx, c...); err != nil {
			return err
		}
	}
	return nil
}

// RunAll runs every command, failures and all (for teardown).
func (s *Shaper) RunAll(ctx context.Context, cmds [][]string) {
	for _, c := range cmds {
		_ = s.exec(ctx, c...)
	}
}

func (s *Shaper) exec(ctx context.Context, args ...string) error {
	if s.Exec != nil {
		return s.Exec(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // tc and ip with arguments we built
	withNetAdmin(cmd)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		hint := ""
		if errors.Is(err, syscall.EPERM) || strings.Contains(out.String(), "not permitted") {
			hint = " (the runner needs NET_ADMIN: see compose.yaml and k8s/runner-job.yaml)"
		}
		return fmt.Errorf("%s: %w: %s%s", strings.Join(args, " "), err, strings.TrimSpace(out.String()), hint)
	}
	return nil
}

// IfaceToward returns the interface (and local address) the runner reaches addr (host:port) through.
func IfaceToward(addr string) (string, net.IP, error) {
	local, err := LocalIPToward(addr)
	if err != nil {
		return "", nil, err
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", nil, err
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
				return ifc.Name, local, nil
			}
		}
	}
	return "", local, fmt.Errorf("no interface holds %s, the address toward %s", local, addr)
}

// LocalIPToward returns the local address the runner reaches addr (host:port) from.
func LocalIPToward(addr string) (net.IP, error) {
	c, err := net.Dial("udp", addr) // sends nothing: just picks the route
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}
