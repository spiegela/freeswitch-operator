package rawsip

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFill(t *testing.T) {
	msg, err := Fill("OPTIONS sip:{{edge}} SIP/2.0\nVia: SIP/2.0/UDP {{local}}\nContent-Length: {{len}}\n\nhello\n",
		map[string]string{"edge": "e:5060", "local": "l:1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := "OPTIONS sip:e:5060 SIP/2.0\r\nVia: SIP/2.0/UDP l:1\r\nContent-Length: 7\r\n\r\nhello\r\n"
	if string(msg) != want {
		t.Errorf("got %q\nwant %q", msg, want)
	}

	// A header-only message gets its closing blank line back.
	msg, err = Fill("OPTIONS sip:x SIP/2.0\nContent-Length: 0\n", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(msg), "Content-Length: 0\r\n\r\n") {
		t.Errorf("no closing blank line: %q", msg)
	}

	// Line ends kept as written.
	msg, _ = Fill("A\nB\n", nil, false)
	if string(msg) != "A\nB\n" {
		t.Errorf("keepLineEnds changed the message: %q", msg)
	}

	// Blank lines ahead of the start line aren't the end of the headers.
	msg, _ = Fill("\n\nINVITE sip:x SIP/2.0\nContent-Length: {{len}}\n\nab{{nul}}{{pad:3}}\n", nil, true)
	if want := "\r\n\r\nINVITE sip:x SIP/2.0\r\nContent-Length: 8\r\n\r\nab\x00xxx\r\n"; string(msg) != want {
		t.Errorf("got %q\nwant %q", msg, want)
	}
	msg, _ = Fill("\nOPTIONS sip:x SIP/2.0\nContent-Length: 0\n", nil, true)
	if !strings.HasSuffix(string(msg), "Content-Length: 0\r\n\r\n") {
		t.Errorf("a leading blank line kept the closing one from being added: %q", msg)
	}
	if _, err := Fill("{{pad:x}}", nil, true); err == nil {
		t.Errorf("a bad pad size must be an error")
	}

	if _, err := Fill("{{nope}}", nil, true); err == nil {
		t.Errorf("an unknown placeholder must be an error")
	}
	if _, err := Fill("{{edge", nil, true); err == nil {
		t.Errorf("an unclosed placeholder must be an error")
	}
}

func TestParse(t *testing.T) {
	raw := "SIP/2.0 180 Ringing\r\nv: SIP/2.0/UDP a:1;branch=z9hG4bK1\r\nVia: SIP/2.0/UDP b:2\r\n" +
		"Subject: one\r\n  two\r\nl: 3\r\n\r\nabcEXTRA"
	m, n, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Code != 180 || m.Reason != "Ringing" {
		t.Errorf("status %d %q", m.Code, m.Reason)
	}
	if vias := m.Get("VIA"); len(vias) != 2 || vias[0] != "SIP/2.0/UDP a:1;branch=z9hG4bK1" {
		t.Errorf("vias %q", vias)
	}
	if s, _ := m.First("subject"); s != "one two" {
		t.Errorf("folded subject %q", s)
	}
	if string(m.Body) != "abc" || n != len(raw)-len("EXTRA") {
		t.Errorf("body %q, used %d of %d", m.Body, n, len(raw))
	}

	req, _, err := Parse([]byte("INVITE sip:a@b SIP/2.0\r\nContent-Length: 0\r\n\r\n"))
	if err != nil || req.Method != "INVITE" || req.URI != "sip:a@b" {
		t.Errorf("request: %+v %v", req, err)
	}

	// Over a stream, a body still on its way is incomplete, not an error.
	if _, _, err := Parse([]byte("SIP/2.0 200 OK\r\nContent-Length: 10\r\n\r\nabc")); err != errIncomplete {
		t.Errorf("short body: %v", err)
	}
	if _, _, err := Parse([]byte("SIP/2.0 200 OK\r\nContent-Length: -1\r\n\r\n")); err == nil {
		t.Errorf("a negative Content-Length must be an error")
	}
}

// An edge may answer from another address than the one a datagram went to (a Service address, a host with
// several): the connection must take those answers too.
func TestUDPAnswerFromAnotherAddress(t *testing.T) {
	edge, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edge.Close() }()
	other, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	go func() {
		b := make([]byte, 2048)
		_, from, err := edge.ReadFrom(b)
		if err != nil {
			return
		}
		_, _ = other.WriteTo([]byte("SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n"), from)
	}()

	c, err := Dial(context.Background(), "udp", edge.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Send([]byte("OPTIONS sip:x SIP/2.0\r\nContent-Length: 0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	m, err := c.Recv(time.Now().Add(2 * time.Second))
	if err != nil || m.Code != 200 {
		t.Fatalf("got %+v, %v", m, err)
	}
}

func TestTCPFramingAndClose(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		b := make([]byte, 2048)
		_, _ = conn.Read(b)
		// Two responses in pieces, then close.
		_, _ = conn.Write([]byte("SIP/2.0 100 Trying\r\nContent-Length: 0\r\n\r\nSIP/2.0 400 Bad"))
		time.Sleep(50 * time.Millisecond)
		_, _ = conn.Write([]byte(" Request\r\nContent-Length: 2\r\n\r\nok"))
		_ = conn.Close()
	}()

	c, err := Dial(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Send([]byte("INVITE sip:x SIP/2.0\r\nContent-Length: 0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var codes []int
	for {
		m, err := c.Recv(deadline)
		if err != nil {
			break
		}
		codes = append(codes, m.Code)
	}
	if len(codes) != 2 || codes[0] != 100 || codes[1] != 400 {
		t.Errorf("codes %v", codes)
	}
	if !c.Closed() {
		t.Errorf("the close was not seen")
	}
}
