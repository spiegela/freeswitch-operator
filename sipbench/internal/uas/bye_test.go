package uas

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/rawsip"
)

// A far end answers from one address, then moves (a re-INVITE from another, with its Contact), as FreeSWITCH does
// when another node recovers the call: the BYE must go to where it moved.
func TestHangupClientFollowsTheTargetRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	old, moved := listenUDP(t), listenUDP(t)

	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	_ = l.Close()
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ua.Close() }()
	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: port}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}),
	)
	if err := dg.ServeBackground(ctx, func(*diago.DialogServerSession) {}); err != nil {
		t.Fatal(err)
	}

	type placed struct {
		d   *diago.DialogClientSession
		err error
	}
	got := make(chan placed, 1)
	go func() {
		d, _, err := dg.Invite(ctx, sip.Uri{User: "far", Host: "127.0.0.1", Port: udpPort(old)}, diago.InviteOptions{})
		got <- placed{d, err}
	}()
	inv, caller := recvSIP(t, old)
	to := first(t, inv, "to") + ";tag=far"
	sendSIP(t, old, caller, "SIP/2.0 200 OK\r\n"+
		"Via: "+first(t, inv, "via")+"\r\nFrom: "+first(t, inv, "from")+"\r\nTo: "+to+"\r\n"+
		"Call-ID: "+first(t, inv, "call-id")+"\r\nCSeq: "+first(t, inv, "cseq")+"\r\n"+
		fmt.Sprintf("Contact: <sip:far@127.0.0.1:%d>\r\n", udpPort(old)), sdp(udpPort(old)))
	p := <-got
	if p.err != nil {
		t.Fatal(p.err)
	}
	if ack, _ := recvSIP(t, old); ack.Method != "ACK" {
		t.Fatalf("want the ACK, got %s", ack.Method)
	}

	sendSIP(t, moved, caller, "INVITE sip:"+caller.String()+" SIP/2.0\r\n"+
		fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-moved\r\n", udpPort(moved))+
		"From: "+to+"\r\nTo: "+first(t, inv, "from")+"\r\nCall-ID: "+first(t, inv, "call-id")+"\r\nCSeq: 1 INVITE\r\n"+
		fmt.Sprintf("Contact: <sip:far@127.0.0.1:%d>\r\n", udpPort(moved)), sdp(udpPort(moved)))
	if ok, _ := recvSIP(t, moved); ok.Code != 200 {
		t.Fatalf("re-INVITE: want 200, got %d", ok.Code)
	}

	hung := make(chan error, 1)
	go func() { hung <- HangupClient(ctx, p.d) }()
	bye, from := recvSIP(t, moved)
	if bye.Method != "BYE" || !strings.Contains(bye.URI, fmt.Sprintf(":%d", udpPort(moved))) {
		t.Fatalf("want a BYE to the moved Contact, got %s %s", bye.Method, bye.URI)
	}
	sendSIP(t, moved, from, "SIP/2.0 200 OK\r\nVia: "+first(t, bye, "via")+"\r\nFrom: "+first(t, bye, "from")+
		"\r\nTo: "+first(t, bye, "to")+"\r\nCall-ID: "+first(t, bye, "call-id")+"\r\nCSeq: "+first(t, bye, "cseq")+"\r\n", "")
	if err := <-hung; err != nil {
		t.Fatal(err)
	}
}

func listenUDP(t *testing.T) net.PacketConn {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func udpPort(c net.PacketConn) int { return c.LocalAddr().(*net.UDPAddr).Port }

func recvSIP(t *testing.T, c net.PacketConn) (rawsip.Message, net.Addr) {
	t.Helper()
	b := make([]byte, 8192)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, from, err := c.ReadFrom(b)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := rawsip.Parse(b[:n])
	if err != nil {
		t.Fatal(err)
	}
	return m, from
}

func sendSIP(t *testing.T, c net.PacketConn, to net.Addr, head, body string) {
	t.Helper()
	if body != "" {
		head += "Content-Type: application/sdp\r\n"
	}
	msg := fmt.Sprintf("%sContent-Length: %d\r\n\r\n%s", head, len(body), body)
	if _, err := c.WriteTo([]byte(msg), to); err != nil {
		t.Fatal(err)
	}
}

func first(t *testing.T, m rawsip.Message, name string) string {
	t.Helper()
	v, ok := m.First(name)
	if !ok {
		t.Fatalf("no %s header", name)
	}
	return v
}

func sdp(port int) string {
	return fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n"+
		"m=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n", port)
}
