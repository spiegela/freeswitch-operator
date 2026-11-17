// Package esl is a minimal FreeSWITCH event socket client: enough to authenticate and run API commands.
package esl

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Client is one inbound event socket connection.
type Client struct {
	conn net.Conn
	r    *textproto.Reader
}

// Dial connects to addr (host:port) and authenticates with password.
func Dial(addr, password string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, r: textproto.NewReader(bufio.NewReader(conn))}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	h, _, err := c.read()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading greeting: %w", err)
	}
	if h.Get("Content-Type") != "auth/request" {
		conn.Close()
		return nil, fmt.Errorf("unexpected greeting %q", h.Get("Content-Type"))
	}
	if _, err := fmt.Fprintf(conn, "auth %s\n\n", password); err != nil {
		conn.Close()
		return nil, err
	}
	h, _, err = c.read()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading auth reply: %w", err)
	}
	if reply := h.Get("Reply-Text"); !strings.HasPrefix(reply, "+OK") {
		conn.Close()
		return nil, fmt.Errorf("auth failed: %s", reply)
	}
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

// API runs an API command and returns its output.
func (c *Client) API(cmd string, timeout time.Duration) (string, error) {
	_ = c.conn.SetDeadline(time.Now().Add(timeout))
	defer c.conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(c.conn, "api %s\n\n", cmd); err != nil {
		return "", err
	}
	for {
		h, body, err := c.read()
		if err != nil {
			return "", err
		}
		if h.Get("Content-Type") == "api/response" {
			out := strings.TrimSpace(string(body))
			if strings.HasPrefix(out, "-ERR") {
				return out, fmt.Errorf("%s: %s", cmd, out)
			}
			return out, nil
		}
	}
}

// Close ends the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) read() (textproto.MIMEHeader, []byte, error) {
	h, err := c.r.ReadMIMEHeader()
	if err != nil {
		return nil, nil, err
	}
	var body []byte
	if cl := h.Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil {
			return nil, nil, fmt.Errorf("bad Content-Length %q", cl)
		}
		body = make([]byte, n)
		if _, err := io.ReadFull(c.r.R, body); err != nil {
			return nil, nil, err
		}
	}
	return h, body, nil
}
