// Package netem impairs the network between the bench and the edge, in the vocabulary of the conformance record's
// impairment cases (conformance/impairment.yaml): random loss, delay and jitter, reordering, duplication and
// burst loss through the kernel's netem qdisc (tc), and named SIP messages dropped or delayed by the runner's own
// SIP layer (tc can't tell the first INVITE from its retransmission).
//
// Only the carrier-edge link is implemented: the runner shapes its own interface. The other links of the
// vocabulary (proxy-node, edge-backend, node-apiserver) parse, and need capabilities no runner offers yet
// (netem:<link>), so scenarios using them skip.
package netem

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Links of the call path an impairment can sit on.
const (
	LinkCarrierEdge   = "carrier-edge"
	LinkProxyNode     = "proxy-node"
	LinkEdgeBackend   = "edge-backend"
	LinkNodeAPIServer = "node-apiserver"
)

// Capability is what a runner offers when it can impair the carrier-edge link with tc: NET_ADMIN, and tc and ip
// in its image. Other links need Capability + ":" + link.
const Capability = "netem"

// TraceCapability is what a runner offers when it can send and receive all of a call's SIP on the call's own
// socket, for the SIP layer to drop, hold and record: when the edge address is a SIP node's own address. Behind a
// Service VIP it can't: in-dialog requests go to the node's own address (its Record-Route), and one socket
// talking to a node both through the VIP's NAT and directly collides in conntrack, which drops them.
const TraceCapability = "sip-trace"

// Spec is one impairment as a scenario writes it: the same keys as an impairment case in the conformance record.
// Percentages are written "5%", durations "400ms" or "2s".
type Spec struct {
	// Link is carrier-edge, proxy-node, edge-backend or node-apiserver.
	Link string `yaml:"link"`
	// Direction is to-edge, from-edge or both (on proxy-node also to-node, from-node). Default both.
	Direction string `yaml:"direction,omitempty"`
	// Traffic is sip, rtp or all. Default all (sip with drop or match).
	Traffic string `yaml:"traffic,omitempty"`
	// Loss is random packet loss; with Burst, Gilbert-Elliott loss with that mean loss run (in packets).
	Loss  string  `yaml:"loss,omitempty"`
	Burst float64 `yaml:"burst,omitempty"`
	// Delay is added one-way delay, and Jitter the spread around it (without Delay, Delay is taken as Jitter so
	// that no packet is sent early).
	Delay  string `yaml:"delay,omitempty"`
	Jitter string `yaml:"jitter,omitempty"`
	// Reorder sends this share of packets at once, ahead of the delayed rest; it needs Delay or Jitter.
	Reorder   string `yaml:"reorder,omitempty"`
	Duplicate string `yaml:"duplicate,omitempty"`
	// Drop names SIP messages to drop, e.g. "first INVITE", "every ACK", "first 200 to INVITE and first BYE".
	Drop string `yaml:"drop,omitempty"`
	// Match limits Delay and Loss to the named SIP messages, e.g. "ACK".
	Match string `yaml:"match,omitempty"`
	// At starts the impairment this long after the scenario's first call is answered, and For ends it this long
	// after it started. Without At it is in place before the first message and until the scenario ends.
	At  string `yaml:"at,omitempty"`
	For string `yaml:"for,omitempty"`
}

// Direction and traffic values, normalised.
const (
	ToEdge   = "to-edge"
	FromEdge = "from-edge"
	Both     = "both"

	TrafficSIP = "sip"
	TrafficRTP = "rtp"
	TrafficAll = "all"
)

// Impairment is a parsed, validated Spec.
type Impairment struct {
	Spec

	// Out and In say which way it applies: out is from the runner toward the edge (to-edge, to-node), in is
	// toward the runner.
	Out, In bool
	Traffic string

	LossPct      float64
	Burst        float64
	Delay        time.Duration
	Jitter       time.Duration
	ReorderPct   float64
	DuplicatePct float64

	// Drop and Match are handled by the runner's SIP layer, not tc.
	Drop  []Rule
	Match *Selector

	At     time.Duration
	HasAt  bool
	For    time.Duration
	HasFor bool
}

// Parse validates s and returns it parsed.
func Parse(s Spec) (*Impairment, error) { //nolint:gocyclo // one check per key
	im := &Impairment{Spec: s}
	switch s.Link {
	case LinkCarrierEdge, LinkProxyNode, LinkEdgeBackend, LinkNodeAPIServer:
	case "":
		return nil, fmt.Errorf("link is required (carrier-edge, proxy-node, edge-backend, node-apiserver)")
	default:
		return nil, fmt.Errorf("unknown link %q (carrier-edge, proxy-node, edge-backend, node-apiserver)", s.Link)
	}
	switch s.Direction {
	case "", Both:
		im.Out, im.In = true, true
	case ToEdge, "to-node":
		im.Out = true
	case FromEdge, "from-node":
		im.In = true
	default:
		return nil, fmt.Errorf("unknown direction %q (to-edge, from-edge, both)", s.Direction)
	}
	if strings.HasSuffix(s.Direction, "-node") && s.Link != LinkProxyNode {
		return nil, fmt.Errorf("direction %s is for the proxy-node link", s.Direction)
	}

	var err error
	if im.LossPct, err = percent("loss", s.Loss); err != nil {
		return nil, err
	}
	if im.ReorderPct, err = percent("reorder", s.Reorder); err != nil {
		return nil, err
	}
	if im.DuplicatePct, err = percent("duplicate", s.Duplicate); err != nil {
		return nil, err
	}
	if im.Delay, err = duration("delay", s.Delay); err != nil {
		return nil, err
	}
	if im.Jitter, err = duration("jitter", s.Jitter); err != nil {
		return nil, err
	}
	if s.At != "" {
		if im.At, err = duration("at", s.At); err != nil {
			return nil, err
		}
		im.HasAt = true
	}
	if s.For != "" {
		if im.For, err = duration("for", s.For); err != nil {
			return nil, err
		}
		if im.For <= 0 {
			return nil, fmt.Errorf("for must be more than 0")
		}
		im.HasFor = true
	}
	im.Burst = s.Burst
	if s.Burst != 0 && (s.Burst < 1 || im.LossPct == 0 || im.LossPct >= 100) {
		return nil, fmt.Errorf("burst is a mean loss run of at least 1 packet, with a loss between 0%% and 100%%")
	}
	if im.ReorderPct > 0 && im.Delay == 0 && im.Jitter == 0 {
		return nil, fmt.Errorf("reorder needs delay or jitter: reordered packets are the ones not delayed")
	}

	im.Traffic = s.Traffic
	if s.Drop != "" || s.Match != "" {
		if err := im.parseSIPLayer(); err != nil {
			return nil, err
		}
		return im, nil
	}
	switch s.Traffic {
	case "":
		im.Traffic = TrafficAll
	case TrafficSIP, TrafficRTP, TrafficAll:
	default:
		return nil, fmt.Errorf("unknown traffic %q (sip, rtp, all)", s.Traffic)
	}
	return im, nil
}

// parseSIPLayer checks an impairment the runner's SIP layer applies: drop, or match with delay and loss.
func (im *Impairment) parseSIPLayer() error {
	s := im.Spec
	if s.Traffic != "" && s.Traffic != TrafficSIP {
		return fmt.Errorf("drop and match name SIP messages, so traffic must be sip")
	}
	im.Traffic = TrafficSIP
	if s.Drop != "" && s.Match != "" {
		return fmt.Errorf("use drop or match, not both")
	}
	if im.HasAt || im.HasFor {
		return fmt.Errorf("at and for are for tc impairments, not drop or match")
	}
	if s.Drop != "" {
		if s.Loss != "" || s.Delay != "" || s.Jitter != "" || s.Reorder != "" || s.Duplicate != "" || s.Burst != 0 {
			return fmt.Errorf("drop names the messages lost; it takes no loss, delay, jitter, reorder, duplicate or burst")
		}
		rules, err := ParseRules(s.Drop)
		if err != nil {
			return fmt.Errorf("drop: %w", err)
		}
		im.Drop = rules
		return nil
	}
	if s.Jitter != "" || s.Reorder != "" || s.Duplicate != "" || s.Burst != 0 {
		return fmt.Errorf("match takes delay and loss only")
	}
	if s.Delay == "" && s.Loss == "" {
		return fmt.Errorf("match needs delay or loss")
	}
	sel, err := ParseSelector(s.Match)
	if err != nil {
		return fmt.Errorf("match: %w", err)
	}
	im.Match = &sel
	return nil
}

// SIPLayer reports whether the runner's SIP layer applies this impairment (drop or match) rather than tc.
func (im *Impairment) SIPLayer() bool { return len(im.Drop) > 0 || im.Match != nil }

// Requires is the capability a runner needs to apply this impairment: "sip-trace" when the runner's own SIP layer
// does it (drop and match on the carrier-edge link), "netem" for tc on the carrier-edge link, "netem:<link>"
// otherwise.
func (im *Impairment) Requires() string {
	switch {
	case im.Link != LinkCarrierEdge:
		return Capability + ":" + im.Link
	case im.SIPLayer():
		return TraceCapability
	}
	return Capability
}

// String describes the impairment in one line, for reports.
func (im *Impairment) String() string {
	dir := Both
	switch {
	case im.Out && !im.In:
		dir = "out"
	case im.In && !im.Out:
		dir = "in"
	}
	parts := []string{im.Link, dir, im.Traffic}
	if im.SIPLayer() {
		if im.Drop != nil {
			parts = append(parts, "drop "+im.Spec.Drop)
		} else {
			parts = append(parts, "match "+im.Spec.Match)
		}
	}
	sp := im.Spec // the knobs as written
	for _, kv := range [][2]string{
		{"loss", sp.Loss}, {"burst", burstString(im.Burst)}, {"delay", sp.Delay}, {"jitter", sp.Jitter},
		{"reorder", sp.Reorder}, {"duplicate", sp.Duplicate}, {"at", sp.At}, {"for", sp.For},
	} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, " ")
}

func burstString(b float64) string {
	if b == 0 {
		return ""
	}
	return strconv.FormatFloat(b, 'f', -1, 64)
}

func percent(name, v string) (float64, error) {
	if v == "" {
		return 0, nil
	}
	s, ok := strings.CutSuffix(strings.TrimSpace(v), "%")
	if !ok {
		return 0, fmt.Errorf("%s %q: write a percentage, e.g. 5%%", name, v)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 || f > 100 {
		return 0, fmt.Errorf("%s %q: want 0%% to 100%%", name, v)
	}
	return f, nil
}

func duration(name, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s %q: write a duration, e.g. 400ms or 2s", name, v)
	}
	return d, nil
}
