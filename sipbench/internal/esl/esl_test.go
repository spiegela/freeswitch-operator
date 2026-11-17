package esl

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSwitch speaks just enough of the event socket protocol for the client.
func fakeSwitch(t *testing.T, password string, replies map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				_, _ = fmt.Fprint(conn, "Content-Type: auth/request\n\n")
				readCmd := func() string {
					var lines []string
					for {
						l, err := r.ReadString('\n')
						if err != nil {
							return ""
						}
						l = strings.TrimRight(l, "\n")
						if l == "" {
							return strings.Join(lines, "\n")
						}
						lines = append(lines, l)
					}
				}
				if readCmd() != "auth "+password {
					_, _ = fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: -ERR invalid\n\n")
					return
				}
				_, _ = fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
				for {
					cmd := readCmd()
					if cmd == "" {
						return
					}
					// An unrelated event first, as a real switch may send.
					_, _ = fmt.Fprint(conn, "Content-Type: text/disconnect-notice\nContent-Length: 3\n\nbye")
					body := replies[strings.TrimPrefix(cmd, "api ")]
					_, _ = fmt.Fprintf(conn, "Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body)
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestAPI(t *testing.T) {
	addr := fakeSwitch(t, "secret", map[string]string{
		"fsctl max_sessions 4": "+OK max sessions: 4\n",
		"bogus":                "-ERR bogus Command not found!\n",
	})
	if _, err := Dial(addr, "wrong", time.Second); err == nil {
		t.Fatal("bad password accepted")
	}
	c, err := Dial(addr, "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	out, err := c.API("fsctl max_sessions 4", time.Second)
	if err != nil || out != "+OK max sessions: 4" {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := c.API("bogus", time.Second); err == nil {
		t.Fatal("-ERR not reported as an error")
	}
}
