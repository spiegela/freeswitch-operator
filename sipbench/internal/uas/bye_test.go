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
	if ok := recvFinal(t, moved); ok.Code != 200 {
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

// The same as the UAS: a caller moves after the call is answered, and the answering side's BYE must follow.
func TestHangupServerFollowsTheTargetRefresh(t *testing.T) {
	hangupServerAfterRefresh(t, "200 OK")
}

// A BYE the far end refuses still ends the dialog (RFC 3261 15.1.1): diago's hangup once the handler returns must
// not send another, to the Contact the call moved away from.
func TestHangupServerRefusedByeEndsTheDialog(t *testing.T) {
	hangupServerAfterRefresh(t, "481 Call/Transaction Does Not Exist")
}

func hangupServerAfterRefresh(t *testing.T, answer string) {
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
	answered, hang, hung := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	if err := dg.ServeBackground(ctx, func(d *diago.DialogServerSession) {
		if _, err := d.Answer(diago.AnswerOptions{}); err != nil {
			hung <- err
			return
		}
		close(answered)
		select {
		case <-hang:
		case <-ctx.Done():
		}
		hung <- hangupServer(ctx, d)
	}); err != nil {
		t.Fatal(err)
	}
	uas := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}

	from := fmt.Sprintf("<sip:caller@127.0.0.1:%d>;tag=caller", udpPort(old))
	callID := "hangup-server-refresh"
	sendSIP(t, old, uas, fmt.Sprintf("INVITE sip:far@127.0.0.1:%d SIP/2.0\r\n", port)+
		fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-first\r\n", udpPort(old))+
		"Max-Forwards: 70\r\nFrom: "+from+fmt.Sprintf("\r\nTo: <sip:far@127.0.0.1:%d>\r\n", port)+
		"Call-ID: "+callID+"\r\nCSeq: 1 INVITE\r\n"+
		fmt.Sprintf("Contact: <sip:caller@127.0.0.1:%d>\r\n", udpPort(old)), sdp(udpPort(old)))
	ok := recvFinal(t, old)
	if ok.Code != 200 {
		t.Fatalf("INVITE: want 200, got %d", ok.Code)
	}
	to, target := first(t, ok, "to"), contactURI(first(t, ok, "contact"))
	ack := func(c net.PacketConn, cseq int, branch string) {
		sendSIP(t, c, uas, "ACK "+target+" SIP/2.0\r\n"+
			fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-%s\r\n", udpPort(c), branch)+
			"Max-Forwards: 70\r\nFrom: "+from+"\r\nTo: "+to+"\r\nCall-ID: "+callID+fmt.Sprintf("\r\nCSeq: %d ACK\r\n", cseq), "")
	}
	ack(old, 1, "first-ack")
	// A re-INVITE before the answer is complete would get 491.
	select {
	case <-answered:
	case err := <-hung:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("not answered")
	}

	sendSIP(t, moved, uas, "INVITE "+target+" SIP/2.0\r\n"+
		fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-moved\r\n", udpPort(moved))+
		"Max-Forwards: 70\r\nFrom: "+from+"\r\nTo: "+to+"\r\nCall-ID: "+callID+"\r\nCSeq: 2 INVITE\r\n"+
		fmt.Sprintf("Contact: <sip:caller@127.0.0.1:%d>\r\n", udpPort(moved)), sdp(udpPort(moved)))
	if ok := recvFinal(t, moved); ok.Code != 200 {
		t.Fatalf("re-INVITE: want 200, got %d", ok.Code)
	}
	ack(moved, 2, "moved-ack")
	// diago reads the ACK on a goroutine of its own, and sends nothing for it. Read late, it would confirm the dialog
	// again after the refused BYE ended it, and diago would hang up once more, to the first Contact: a race of the
	// test's making (a real hangup comes long after the ACK), so give the ACK a moment.
	time.Sleep(200 * time.Millisecond)

	close(hang)
	bye, byeFrom := recvSIP(t, moved)
	if bye.Method != "BYE" || !strings.Contains(bye.URI, fmt.Sprintf(":%d", udpPort(moved))) {
		t.Fatalf("want a BYE to the moved Contact, got %s %s", bye.Method, bye.URI)
	}
	sendSIP(t, moved, byeFrom, "SIP/2.0 "+answer+"\r\nVia: "+first(t, bye, "via")+"\r\nFrom: "+first(t, bye, "from")+
		"\r\nTo: "+first(t, bye, "to")+"\r\nCall-ID: "+first(t, bye, "call-id")+"\r\nCSeq: "+first(t, bye, "cseq")+"\r\n", "")
	if err := <-hung; (err != nil) != !strings.HasPrefix(answer, "200") {
		t.Fatalf("answered %s, hangup returned %v", answer, err)
	}
	// The handler has returned, and diago hangs up whatever isn't over at once.
	b := make([]byte, 8192)
	_ = old.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	if n, _, err := old.ReadFrom(b); err == nil {
		t.Fatalf("the Contact the call left got %q", b[:min(n, 40)])
	}
}

// recvFinal reads responses until a final one.
func recvFinal(t *testing.T, c net.PacketConn) rawsip.Message {
	t.Helper()
	for {
		m, _ := recvSIP(t, c)
		if m.Code >= 200 {
			return m
		}
	}
}

// contactURI is the URI in a Contact header's value.
func contactURI(v string) string {
	if i := strings.IndexByte(v, '<'); i >= 0 {
		if j := strings.IndexByte(v[i:], '>'); j > 0 {
			return v[i+1 : i+j]
		}
	}
	v, _, _ = strings.Cut(v, ";")
	return v
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
