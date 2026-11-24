package netem

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, s Spec) *Impairment {
	t.Helper()
	im, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%+v): %v", s, err)
	}
	return im
}

func TestParse(t *testing.T) {
	im := mustParse(t, Spec{Link: LinkCarrierEdge, Direction: ToEdge, Traffic: "rtp", Loss: "5%", Delay: "40ms",
		Jitter: "30ms", Reorder: "5%", At: "5s", For: "2s"})
	if !im.Out || im.In || im.Traffic != TrafficRTP || im.LossPct != 5 || im.Delay != 40*time.Millisecond ||
		im.Jitter != 30*time.Millisecond || im.ReorderPct != 5 || !im.HasAt || im.At != 5*time.Second || im.For != 2*time.Second {
		t.Errorf("parsed %+v", im)
	}
	if im.Requires() != Capability {
		t.Errorf("requires %q", im.Requires())
	}

	d := mustParse(t, Spec{Link: LinkCarrierEdge, Direction: FromEdge, Traffic: "sip", Drop: "first 100 and first 180"})
	if !d.SIPLayer() || d.Requires() != TraceCapability || len(d.Drop) != 2 || d.Drop[1].Sel.Code != 180 || d.Drop[0].Nth != 1 {
		t.Errorf("drop parsed %+v", d)
	}
	m := mustParse(t, Spec{Link: LinkCarrierEdge, Direction: ToEdge, Traffic: "sip", Delay: "1s", Match: "ACK"})
	if m.Match == nil || m.Match.Method != "ACK" || m.Delay != time.Second {
		t.Errorf("match parsed %+v", m)
	}
	if p := mustParse(t, Spec{Link: LinkProxyNode, Direction: "to-node", Loss: "5%"}); p.Requires() != "netem:proxy-node" {
		t.Errorf("proxy-node requires %q", p.Requires())
	}

	for _, bad := range []Spec{
		{},
		{Link: "carrier"},
		{Link: LinkCarrierEdge, Direction: "up"},
		{Link: LinkCarrierEdge, Direction: "to-node"},
		{Link: LinkCarrierEdge, Traffic: "dns"},
		{Link: LinkCarrierEdge, Loss: "5"},
		{Link: LinkCarrierEdge, Loss: "150%"},
		{Link: LinkCarrierEdge, Delay: "fast"},
		{Link: LinkCarrierEdge, Reorder: "5%"},
		{Link: LinkCarrierEdge, Loss: "1%", Burst: 0.5},
		{Link: LinkCarrierEdge, Burst: 2},
		{Link: LinkCarrierEdge, For: "0s"},
		{Link: LinkCarrierEdge, Traffic: "rtp", Drop: "first INVITE"},
		{Link: LinkCarrierEdge, Drop: "first INVITE", Loss: "5%"},
		{Link: LinkCarrierEdge, Drop: "the INVITE"},
		{Link: LinkCarrierEdge, Drop: "every end packet of the second digit"},
		{Link: LinkCarrierEdge, Drop: "first INVITE", At: "5s"},
		{Link: LinkCarrierEdge, Match: "ACK"},
		{Link: LinkCarrierEdge, Match: "ACK", Jitter: "5ms", Delay: "1s"},
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%+v) succeeded", bad)
		}
	}
}

func TestSelectorsAndSummaries(t *testing.T) {
	invite := []byte("INVITE sip:a@b SIP/2.0\r\nVia: x\r\nCSeq: 1 INVITE\r\nTo: <sip:a@b>\r\n\r\nv=0\r\n")
	ok := []byte("SIP/2.0 200 OK\r\nCSeq: 1 INVITE\r\nt: <sip:a@b>;tag=abc\r\n\r\n")
	byeOK := []byte("SIP/2.0 200 OK\r\nCSeq: 2 BYE\r\nTo: <sip:a@b>;tag=abc\r\n\r\n")

	if m := Summarize(invite); m.Name() != "INVITE" || m.ToTag != "" {
		t.Errorf("invite: %+v", m)
	}
	if m := Summarize(ok); m.Name() != "200 INVITE" || m.ToTag != "abc" {
		t.Errorf("200: %+v", m)
	}
	if m := Summarize([]byte("\r\n\r\n")); m.Name() != "?" {
		t.Errorf("keepalive: %+v", m)
	}

	for s, want := range map[string][3]bool{ // matches invite, ok, byeOK
		"INVITE":        {true, false, false},
		"200":           {false, true, true},
		"200 to INVITE": {false, true, false},
		"200 INVITE":    {false, true, false},
	} {
		sel, err := ParseSelector(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		got := [3]bool{sel.Matches(Summarize(invite)), sel.Matches(Summarize(ok)), sel.Matches(Summarize(byeOK))}
		if got != want {
			t.Errorf("%s: matches %v, want %v", s, got, want)
		}
	}
	for _, bad := range []string{"", "invite", "99", "200 to", "200 to INVITE now"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("ParseSelector(%q) succeeded", bad)
		}
	}
}

func TestSIPRulesCount(t *testing.T) {
	ims := []*Impairment{
		mustParse(t, Spec{Link: LinkCarrierEdge, Direction: ToEdge, Traffic: "sip", Drop: "first INVITE and second ACK"}),
		mustParse(t, Spec{Link: LinkCarrierEdge, Direction: FromEdge, Traffic: "sip", Drop: "every BYE"}),
		mustParse(t, Spec{Link: LinkCarrierEdge, Direction: ToEdge, Traffic: "sip", Match: "BYE", Delay: "1s"}),
	}
	r := NewSIPRules(ims)
	inv, ack, bye := Summary{Method: "INVITE"}, Summary{Method: "ACK"}, Summary{Method: "BYE"}
	steps := []struct {
		dir     string
		m       Summary
		drop    bool
		delayed bool
		why     string
	}{
		{DirOut, inv, true, false, "first INVITE"},
		{DirOut, inv, false, false, "retransmission"},
		{DirOut, ack, false, false, "first ACK"},
		{DirOut, ack, true, false, "second ACK"},
		{DirOut, ack, false, false, "third ACK"},
		{DirIn, bye, true, false, "every BYE in"},
		{DirIn, bye, true, false, "every BYE in, again"},
		{DirOut, bye, false, true, "BYE out is held"},
		{DirIn, inv, false, false, "an INVITE in is not the one out"},
	}
	for _, s := range steps {
		drop, delay := r.decide(s.dir, s.m)
		if drop != s.drop || (delay > 0) != s.delayed {
			t.Errorf("%s: drop=%v delay=%v", s.why, drop, delay)
		}
	}
}

// TestConn drops the first INVITE out and the first 200 in, and records all of it.
func TestConn(t *testing.T) {
	a, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	trace := NewTrace()
	c := NewConn(a, NewSIPRules([]*Impairment{
		mustParse(t, Spec{Link: LinkCarrierEdge, Direction: ToEdge, Traffic: "sip", Drop: "first INVITE"}),
		mustParse(t, Spec{Link: LinkCarrierEdge, Direction: FromEdge, Traffic: "sip", Drop: "first 200 to INVITE"}),
	}), trace)
	defer func() { _ = c.Close() }()

	invite := []byte("INVITE sip:a@b SIP/2.0\r\nCSeq: 1 INVITE\r\n\r\n")
	for range 2 {
		if n, err := c.WriteTo(invite, b.LocalAddr()); err != nil || n != len(invite) {
			t.Fatalf("write: %d, %v", n, err)
		}
	}
	buf := make([]byte, 1500)
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := b.ReadFrom(buf); err != nil {
		t.Fatalf("the retransmission did not arrive: %v", err)
	}
	_ = b.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := b.ReadFrom(buf); err == nil {
		t.Fatal("both copies arrived; the first should have been dropped")
	}

	ok := []byte("SIP/2.0 200 OK\r\nCSeq: 1 INVITE\r\nTo: <sip:a@b>;tag=x\r\n\r\n")
	for range 2 {
		if _, err := b.WriteTo(ok, a.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	got := make(chan string, 2)
	go func() {
		for {
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				close(got)
				return
			}
			got <- string(buf[:n])
		}
	}()
	select {
	case s := <-got:
		if !strings.HasPrefix(s, "SIP/2.0 200") {
			t.Fatalf("read %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second 200 did not come through")
	}
	select {
	case s, ok := <-got:
		if ok {
			t.Fatalf("a second message came through: %q", s)
		}
	case <-time.After(200 * time.Millisecond):
	}

	entries := trace.Entries()
	trail := make([]string, 0, len(entries))
	for _, e := range entries {
		trail = append(trail, e.Dir+" "+e.Msg+" "+e.Impaired)
	}
	want := []string{"out INVITE dropped", "out INVITE ", "in 200 INVITE dropped", "in 200 INVITE "}
	if !slices.Equal(trail, want) {
		t.Errorf("trace %q, want %q", trail, want)
	}
	_ = c.Close()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Error("ReadFrom did not return after Close")
	}
}

func TestNetemArgs(t *testing.T) {
	for spec, want := range map[Spec]string{
		{Link: LinkCarrierEdge, Loss: "5%"}:                                     "loss 5%",
		{Link: LinkCarrierEdge, Delay: "100ms", Jitter: "50ms", Loss: "0.1%"}:   "delay 100000us 50000us loss 0.1%",
		{Link: LinkCarrierEdge, Jitter: "30ms", Reorder: "5%", Duplicate: "5%"}: "delay 30000us 30000us duplicate 5% reorder 5%",
		// A mean run of 2 at 1% loss: r = 50%, p = 0.01*0.5/0.99.
		{Link: LinkCarrierEdge, Loss: "1%", Burst: 2}: "loss gemodel 0.5051% 50%",
		{Link: LinkCarrierEdge, Loss: "0%"}:           "",
	} {
		if got := strings.Join(NetemArgs(mustParse(t, spec)), " "); got != want {
			t.Errorf("%+v: %q, want %q", spec, got, want)
		}
	}
}

func TestPlan(t *testing.T) {
	s := &Shaper{Iface: "eth0", SIPPort: 5060}
	p, err := s.Plan([]*Impairment{
		mustParse(t, Spec{Link: LinkCarrierEdge, Direction: ToEdge, Traffic: "rtp", Loss: "10%"}),
		mustParse(t, Spec{Link: LinkCarrierEdge, Traffic: "sip", Delay: "400ms"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := func(cmds [][]string) string {
		out := make([]string, 0, len(cmds))
		for _, c := range cmds {
			out = append(out, strings.Join(c, " "))
		}
		return strings.Join(out, "\n")
	}
	zeros := strings.Repeat(" 0", 16)
	wantSetup := strings.Join([]string{
		"tc qdisc add dev eth0 root handle 1: prio bands 3 priomap" + zeros,
		"ip link add sbnetem0 type veth peer name sbnetem1",
		"ip link set sbnetem0 up",
		"ip link set sbnetem1 up",
		"tc qdisc add dev sbnetem1 handle ffff: ingress",
		"tc filter add dev sbnetem1 parent ffff: matchall action skbedit ptype host pipe action skbedit mark 0x5b4e pipe " +
			"action mirred ingress redirect dev eth0",
		"tc qdisc add dev sbnetem0 root handle 1: prio bands 3 priomap" + zeros,
		"tc qdisc add dev eth0 handle ffff: ingress",
		"tc filter add dev eth0 parent ffff: protocol ip prio 1 u32 match mark 0x5b4e 0xffffffff action pass",
		// Impairment 1: RTP out only, from the runner's RTP ports.
		"tc filter add dev eth0 parent 1: protocol ip prio 2 u32 match ip protocol 17 0xff match ip sport 20480 0xf000 flowid 1:2",
		// Impairment 2: SIP both ways, to and from the edge's SIP port, over UDP and TCP.
		"tc filter add dev eth0 parent 1: protocol ip prio 3 u32 match ip protocol 17 0xff match ip dport 5060 0xffff flowid 1:3",
		"tc filter add dev eth0 parent 1: protocol ip prio 3 u32 match ip protocol 6 0xff match ip dport 5060 0xffff flowid 1:3",
		"tc filter add dev eth0 parent ffff: protocol ip prio 3 u32 match ip protocol 17 0xff match ip sport 5060 0xffff action mirred egress redirect dev sbnetem0",
		"tc filter add dev sbnetem0 parent 1: protocol ip prio 3 u32 match ip protocol 17 0xff match ip sport 5060 0xffff flowid 1:3",
		"tc filter add dev eth0 parent ffff: protocol ip prio 3 u32 match ip protocol 6 0xff match ip sport 5060 0xffff action mirred egress redirect dev sbnetem0",
		"tc filter add dev sbnetem0 parent 1: protocol ip prio 3 u32 match ip protocol 6 0xff match ip sport 5060 0xffff flowid 1:3",
	}, "\n")
	if got := lines(p.Setup); got != wantSetup {
		t.Errorf("setup:\n%s\nwant:\n%s", got, wantSetup)
	}
	if got, want := lines(p.On[0]), "tc qdisc add dev eth0 parent 1:2 netem loss 10%"; got != want {
		t.Errorf("on 0: %s", got)
	}
	if got, want := lines(p.On[1]), "tc qdisc add dev eth0 parent 1:3 netem delay 400000us\n"+
		"tc qdisc add dev sbnetem0 parent 1:3 netem delay 400000us"; got != want {
		t.Errorf("on 1: %s", got)
	}
	if got, want := lines(p.Off[1]), "tc qdisc del dev eth0 parent 1:3\ntc qdisc del dev sbnetem0 parent 1:3"; got != want {
		t.Errorf("off 1: %s", got)
	}
	if got, want := lines(p.Teardown), "tc qdisc del dev eth0 root\ntc qdisc del dev eth0 ingress\nip link del sbnetem0"; got != want {
		t.Errorf("teardown: %s", got)
	}

	// Only out: no veth, no ingress.
	p, err = s.Plan([]*Impairment{mustParse(t, Spec{Link: LinkCarrierEdge, Direction: ToEdge, Loss: "1%"})})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lines(p.Setup), "ingress") || len(p.Setup) != 4 {
		t.Errorf("out-only setup:\n%s", lines(p.Setup))
	}

	if _, err := s.Plan([]*Impairment{mustParse(t, Spec{Link: LinkCarrierEdge, Drop: "first ACK"})}); err == nil {
		t.Error("planned a drop, which is the SIP layer's")
	}
	if _, err := s.Plan([]*Impairment{mustParse(t, Spec{Link: LinkProxyNode, Loss: "5%"})}); err == nil {
		t.Error("planned a proxy-node impairment")
	}

	var ran []string
	s.Exec = func(_ context.Context, args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		return nil
	}
	if err := s.Run(context.Background(), p.Setup); err != nil || len(ran) != len(p.Setup) {
		t.Errorf("ran %d of %d: %v", len(ran), len(p.Setup), err)
	}
}
