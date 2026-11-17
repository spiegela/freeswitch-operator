// Package rawsip sends SIP messages exactly as written, byte for byte, and reads back what comes in response. The
// runner's SIP stack (sipgo) only builds well-formed requests; conformance cases need malformed, unusual and
// out-of-dialog ones: Max-Forwards of zero, a missing Call-ID, an unknown scheme or SIP version, a CANCEL or
// response that matches nothing, odd but legal syntax.
package rawsip

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Fill builds a message from a template: each {{name}} is replaced from vars, line ends become CRLF when crlf is
// set (YAML block scalars carry LF; a message with no blank line gets one at the end), and {{len}} becomes the length in bytes of the body, everything after the
// first blank line, once the rest is filled in. An unknown placeholder is an error, so a typo can't go out as
// literal braces.
func Fill(tmpl string, vars map[string]string, crlf bool) ([]byte, error) {
	var out strings.Builder
	rest := tmpl
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			out.WriteString(rest)
			break
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unclosed {{ at %q", rest[i:min(len(rest), i+20)])
		}
		name := strings.TrimSpace(rest[i+2 : i+j])
		out.WriteString(rest[:i])
		switch v, ok := vars[name]; {
		case name == "len":
			out.WriteString("{{len}}") // filled in last
		case ok:
			out.WriteString(v)
		default:
			return nil, fmt.Errorf("unknown placeholder {{%s}}", name)
		}
		rest = rest[i+j+2:]
	}
	msg := out.String()
	if crlf {
		msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r\n", "\n"), "\n", "\r\n")
		// A header-only message written as a YAML block loses its closing blank line: put it back.
		if !strings.Contains(msg, "\r\n\r\n") {
			msg = strings.TrimSuffix(msg, "\r\n") + "\r\n\r\n"
		}
	}
	if strings.Contains(msg, "{{len}}") {
		sep := "\n\n"
		if crlf {
			sep = "\r\n\r\n"
		}
		body := ""
		if k := strings.Index(msg, sep); k >= 0 {
			body = msg[k+len(sep):]
		}
		msg = strings.ReplaceAll(msg, "{{len}}", strconv.Itoa(len(body)))
	}
	return []byte(msg), nil
}

// Token returns n random bytes as hex, for branches, tags and Call-IDs.
func Token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Header is one header field as received: the name as written and the value, unfolded.
type Header struct {
	Name  string
	Value string
}

// Message is a parsed SIP message: a response (Code > 0) or a request (Method set).
type Message struct {
	// The start line's parts: Method and URI for a request, Code and Reason for a response.
	Method, URI string
	Code        int
	Reason      string
	Headers     []Header
	Body        []byte
	// At is when it arrived, from the first send.
	At time.Duration
}

// compact maps RFC 3261's compact header names to their long forms.
var compact = map[string]string{
	"i": "call-id", "m": "contact", "e": "content-encoding", "l": "content-length", "c": "content-type",
	"f": "from", "s": "subject", "k": "supported", "t": "to", "v": "via", "o": "event", "r": "refer-to",
	"u": "allow-events", "b": "referred-by", "x": "session-expires",
}

// canonical is a header name's lower-case long form.
func canonical(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if long, ok := compact[n]; ok {
		return long
	}
	return n
}

// Get returns every value of the named header, in order: comma-separated values stay as one entry per line.
// Compact and long names match each other, case-insensitively.
func (m *Message) Get(name string) []string {
	want := canonical(name)
	var out []string
	for _, h := range m.Headers {
		if canonical(h.Name) == want {
			out = append(out, h.Value)
		}
	}
	return out
}

// First returns the first value of the named header, and whether there was one.
func (m *Message) First(name string) (string, bool) {
	vs := m.Get(name)
	if len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// StatusLine is the response's start line, for reports.
func (m *Message) StatusLine() string {
	if m.Code > 0 {
		return fmt.Sprintf("%d %s", m.Code, m.Reason)
	}
	return m.Method + " " + m.URI
}

var errIncomplete = errors.New("incomplete message")

// Parse reads one message from the front of b and returns how many bytes it took. Header names may be compact
// and any case, values may be folded. The body is Content-Length bytes (the rest of b without one, as over UDP);
// errIncomplete means b ends before the message does (a stream needs more).
func Parse(b []byte) (Message, int, error) {
	end := bytes.Index(b, []byte("\r\n\r\n"))
	sepLen := 4
	if lf := bytes.Index(b, []byte("\n\n")); lf >= 0 && (end < 0 || lf < end) {
		end, sepLen = lf, 2
	}
	if end < 0 {
		return Message{}, 0, errIncomplete
	}
	lines := strings.Split(strings.ReplaceAll(string(b[:end]), "\r\n", "\n"), "\n")
	var m Message
	start := strings.TrimSpace(lines[0])
	if strings.HasPrefix(strings.ToUpper(start), "SIP/") {
		parts := strings.SplitN(start, " ", 3)
		if len(parts) < 2 {
			return Message{}, 0, fmt.Errorf("bad status line %q", start)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return Message{}, 0, fmt.Errorf("bad status code in %q", start)
		}
		m.Code = code
		if len(parts) == 3 {
			m.Reason = parts[2]
		}
	} else {
		parts := strings.Fields(start)
		if len(parts) < 2 {
			return Message{}, 0, fmt.Errorf("bad request line %q", start)
		}
		m.Method, m.URI = parts[0], parts[1]
	}
	for _, line := range lines[1:] {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(m.Headers) > 0 {
			m.Headers[len(m.Headers)-1].Value += " " + strings.TrimSpace(line)
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m.Headers = append(m.Headers, Header{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	bodyStart := end + sepLen
	n := len(b) - bodyStart
	if cl, ok := m.First("content-length"); ok {
		v, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || v < 0 {
			return Message{}, 0, fmt.Errorf("bad Content-Length %q", cl)
		}
		if v > n {
			return Message{}, 0, errIncomplete
		}
		n = v
	}
	m.Body = append([]byte(nil), b[bodyStart:bodyStart+n]...)
	return m, bodyStart + n, nil
}

// Conn is a UDP or TCP connection to the edge. Over UDP it is an unconnected socket: an edge behind a Service
// address (or with several addresses) may answer from an address other than the one the message went to, as SIP
// allows, and a connected socket would drop those answers.
type Conn struct {
	transport string
	c         net.Conn       // TCP
	pc        net.PacketConn // UDP
	remote    net.Addr       // UDP: where messages go
	start     time.Time
	buf       []byte
	closed    bool
}

// Dial connects to addr over transport ("udp" or "tcp").
func Dial(ctx context.Context, transport, addr string) (*Conn, error) {
	switch transport {
	case "", "udp":
		transport = "udp"
	case "tcp":
	default:
		return nil, fmt.Errorf("transport %q: udp or tcp", transport)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, transport, addr)
	if err != nil {
		return nil, err
	}
	if transport == "tcp" {
		return &Conn{transport: transport, c: c}, nil
	}
	// A connected socket picks the local address the edge will see; listen on that address instead.
	local := c.LocalAddr().(*net.UDPAddr)
	remote := c.RemoteAddr()
	_ = c.Close()
	pc, err := net.ListenPacket("udp", net.JoinHostPort(local.IP.String(), "0"))
	if err != nil {
		return nil, err
	}
	return &Conn{transport: transport, pc: pc, remote: remote}, nil
}

// Local is this end's address, host:port, as the edge will see it.
func (c *Conn) Local() string {
	if c.pc != nil {
		return c.pc.LocalAddr().String()
	}
	return c.c.LocalAddr().String()
}

// Transport is "udp" or "tcp".
func (c *Conn) Transport() string { return c.transport }

// Closed reports whether the edge closed a TCP connection.
func (c *Conn) Closed() bool { return c.closed }

// Close closes the connection.
func (c *Conn) Close() error {
	if c.pc != nil {
		return c.pc.Close()
	}
	return c.c.Close()
}

// Send writes msg: one datagram over UDP. The first send starts the clock for Message.At.
func (c *Conn) Send(msg []byte) error {
	if c.start.IsZero() {
		c.start = time.Now()
	}
	var err error
	if c.pc != nil {
		_, err = c.pc.WriteTo(msg, c.remote)
	} else {
		_, err = c.c.Write(msg)
	}
	return err
}

// Recv returns the next message to arrive before deadline. It returns os.ErrDeadlineExceeded when none does, and
// io.EOF once the edge has closed a TCP connection. A datagram that doesn't parse is skipped.
func (c *Conn) Recv(deadline time.Time) (Message, error) {
	if c.pc != nil {
		if err := c.pc.SetReadDeadline(deadline); err != nil {
			return Message{}, err
		}
		b := make([]byte, 65535)
		for {
			n, _, err := c.pc.ReadFrom(b)
			if err != nil {
				return Message{}, err
			}
			m, _, perr := Parse(b[:n])
			if perr != nil {
				continue
			}
			m.At = time.Since(c.start)
			return m, nil
		}
	}
	if err := c.c.SetReadDeadline(deadline); err != nil {
		return Message{}, err
	}
	for {
		if len(c.buf) > 0 {
			// Skip keepalive CRLFs between messages.
			c.buf = bytes.TrimLeft(c.buf, "\r\n")
			m, used, err := Parse(c.buf)
			if err == nil {
				c.buf = c.buf[used:]
				m.At = time.Since(c.start)
				return m, nil
			}
			if !errors.Is(err, errIncomplete) {
				c.buf = nil
				return Message{}, err
			}
		}
		b := make([]byte, 65535)
		n, err := c.c.Read(b)
		c.buf = append(c.buf, b[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
				c.closed = true
			}
			if n == 0 {
				return Message{}, err
			}
		}
	}
}
