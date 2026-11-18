package netem

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Selector names SIP messages: a request by method ("INVITE"), or a response by code, optionally to one method
// ("200", "200 to INVITE"; "200 INVITE" is read the same way).
type Selector struct {
	Method string // a request's method; empty for responses
	Code   int    // a response's code; 0 for requests
	// CSeqMethod limits a response to those answering this method.
	CSeqMethod string
}

// ParseSelector reads a selector.
func ParseSelector(s string) (Selector, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return Selector{}, fmt.Errorf("no message named")
	}
	if code, err := strconv.Atoi(f[0]); err == nil {
		if code < 100 || code > 699 {
			return Selector{}, fmt.Errorf("%q: response codes are 100 to 699", s)
		}
		sel := Selector{Code: code}
		rest := f[1:]
		if len(rest) == 2 && strings.EqualFold(rest[0], "to") {
			rest = rest[1:]
		}
		switch {
		case len(rest) == 0:
		case len(rest) == 1 && method(rest[0]) != "":
			sel.CSeqMethod = rest[0]
		default:
			return Selector{}, fmt.Errorf("%q: write a code, optionally followed by [to] METHOD", s)
		}
		return sel, nil
	}
	if len(f) != 1 || method(f[0]) != f[0] {
		return Selector{}, fmt.Errorf("%q: write a method in capitals (INVITE, ACK, BYE...) or a response code", s)
	}
	return Selector{Method: f[0]}, nil
}

// method returns s if it looks like a SIP method (letters only, in capitals), else "".
func method(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return s
}

// Matches reports whether m is one of the messages s names.
func (s Selector) Matches(m Summary) bool {
	if s.Method != "" {
		return m.Method == s.Method
	}
	return m.Code == s.Code && (s.CSeqMethod == "" || m.CSeqMethod == s.CSeqMethod)
}

func (s Selector) String() string {
	if s.Method != "" {
		return s.Method
	}
	if s.CSeqMethod != "" {
		return fmt.Sprintf("%d %s", s.Code, s.CSeqMethod)
	}
	return strconv.Itoa(s.Code)
}

// Rule drops the nth message a selector names (Nth 0: every one).
type Rule struct {
	Nth int
	Sel Selector
}

var ordinals = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "fifth": 5, "every": 0}

// ParseRules reads a drop list: rules joined by "and", each an ordinal (first...fifth, or every) and a selector:
// "first 100 and first 180", "every ACK", "first 200 to INVITE".
func ParseRules(s string) ([]Rule, error) {
	var out []Rule
	for part := range strings.SplitSeq(s, " and ") {
		f := strings.Fields(part)
		if len(f) < 2 {
			return nil, fmt.Errorf("%q: write first|second|third|every followed by a message", part)
		}
		nth, ok := ordinals[strings.ToLower(f[0])]
		if !ok {
			return nil, fmt.Errorf("%q: %q is not first, second, third, fourth, fifth or every", part, f[0])
		}
		sel, err := ParseSelector(strings.Join(f[1:], " "))
		if err != nil {
			return nil, err
		}
		out = append(out, Rule{Nth: nth, Sel: sel})
	}
	return out, nil
}

// Summary is what a SIP message is, as far as rules and checks care.
type Summary struct {
	Method     string // request method, or "" for a response
	Code       int    // response code, or 0 for a request
	CSeqMethod string
	ToTag      string
}

// Name is how the message appears in traces and checks: "INVITE", or "200 INVITE".
func (m Summary) Name() string {
	if m.Method != "" {
		return m.Method
	}
	if m.Code == 0 {
		return "?"
	}
	return fmt.Sprintf("%d %s", m.Code, m.CSeqMethod)
}

// Summarize reads the start line, CSeq and To tag of a SIP message. Anything unreadable comes back empty, and
// matches no selector.
func Summarize(b []byte) Summary {
	var m Summary
	head, _, _ := bytes.Cut(b, []byte("\r\n\r\n"))
	lines := strings.Split(string(head), "\n")
	if len(lines) == 0 {
		return m
	}
	first := strings.Fields(strings.TrimSpace(lines[0]))
	switch {
	case len(first) >= 2 && strings.HasPrefix(first[0], "SIP/"):
		m.Code, _ = strconv.Atoi(first[1])
	case len(first) >= 3 && strings.HasPrefix(first[2], "SIP/"):
		m.Method = first[0]
	default:
		return Summary{}
	}
	for _, l := range lines[1:] {
		name, value, ok := strings.Cut(strings.TrimSpace(l), ":")
		if !ok {
			continue
		}
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)
		switch name {
		case "cseq":
			if f := strings.Fields(value); len(f) == 2 {
				m.CSeqMethod = f[1]
			}
		case "to", "t":
			for p := range strings.SplitSeq(value, ";") {
				if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(k, "tag") {
					m.ToTag = v
				}
			}
		}
	}
	return m
}

// TraceEntry is one SIP message that crossed the runner's socket.
type TraceEntry struct {
	// Ms is when, in milliseconds since the trace started (just before the call's first INVITE).
	Ms  int64  `json:"ms"`
	Dir string `json:"dir"` // out: the runner sent it; in: the edge did
	Msg string `json:"msg"` // "INVITE", "200 INVITE"
	// ToTag is the To header's tag, if any.
	ToTag string `json:"toTag,omitempty"`
	// Impaired says what the SIP layer did to it: "dropped", or "delayed 1000ms".
	Impaired string `json:"impaired,omitempty"`
}

// Trace records the SIP messages of one call. It is safe for concurrent use.
type Trace struct {
	start   time.Time
	mu      sync.Mutex
	entries []TraceEntry
}

// NewTrace starts a trace now.
func NewTrace() *Trace { return &Trace{start: time.Now()} }

func (t *Trace) add(dir string, m Summary, impaired string) {
	if t == nil || (m.Method == "" && m.Code == 0) {
		return // a keepalive, or not SIP
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = append(t.entries, TraceEntry{
		Ms: time.Since(t.start).Milliseconds(), Dir: dir, Msg: m.Name(), ToTag: m.ToTag, Impaired: impaired,
	})
}

// Entries returns what was recorded so far.
func (t *Trace) Entries() []TraceEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TraceEntry(nil), t.entries...)
}

// Since is how long ago the trace started.
func (t *Trace) Since(at time.Time) time.Duration { return at.Sub(t.start) }

// Trace directions.
const (
	DirOut = "out"
	DirIn  = "in"
)

// sipRule is one impairment's effect on one direction of a call's SIP, with its own counts.
type sipRule struct {
	drop   []Rule
	counts []int
	match  *Selector
	delay  time.Duration
	loss   float64 // percent
}

// SIPRules is what the SIP layer does to one call's messages, per direction. Build one per call with
// NewSIPRules: the counts behind "first" and "second" are per call.
type SIPRules struct {
	mu      sync.Mutex
	out, in []*sipRule
}

// NewSIPRules collects the SIP-layer impairments among ims.
func NewSIPRules(ims []*Impairment) *SIPRules {
	r := &SIPRules{}
	for _, im := range ims {
		if !im.SIPLayer() {
			continue
		}
		mk := func() *sipRule {
			return &sipRule{drop: im.Drop, counts: make([]int, len(im.Drop)), match: im.Match, delay: im.Delay, loss: im.LossPct}
		}
		if im.Out {
			r.out = append(r.out, mk())
		}
		if im.In {
			r.in = append(r.in, mk())
		}
	}
	return r
}

// Empty reports whether there is nothing to do.
func (r *SIPRules) Empty() bool { return r == nil || (len(r.out) == 0 && len(r.in) == 0) }

// decide returns whether to drop the message and how long to hold it.
func (r *SIPRules) decide(dir string, m Summary) (drop bool, delay time.Duration) {
	if r == nil {
		return false, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rules := r.out
	if dir == DirIn {
		rules = r.in
	}
	for _, sr := range rules {
		for i, d := range sr.drop {
			if !d.Sel.Matches(m) {
				continue
			}
			sr.counts[i]++
			if d.Nth == 0 || d.Nth == sr.counts[i] {
				drop = true
			}
		}
		if sr.match != nil && sr.match.Matches(m) {
			if sr.loss > 0 && rand.Float64()*100 < sr.loss { //nolint:gosec // impairment, not security
				drop = true
			}
			delay = max(delay, sr.delay)
		}
	}
	return drop, delay
}

// Conn is a UDP socket that a call's SIP stack uses, with the SIP layer's impairments applied to what passes and
// every message recorded in a trace. Out is WriteTo, in is ReadFrom.
type Conn struct {
	net.PacketConn
	rules *SIPRules
	trace *Trace

	in      chan packet
	done    chan struct{}
	err     error
	closed  sync.Once
	stop    chan struct{}
	serving chan struct{}
	reading sync.Once
}

type packet struct {
	b    []byte
	addr net.Addr
}

// NewConn wraps pc. rules may be nil (trace only), and trace nil (record nothing).
func NewConn(pc net.PacketConn, rules *SIPRules, trace *Trace) *Conn {
	c := &Conn{PacketConn: pc, rules: rules, trace: trace, in: make(chan packet, 64), done: make(chan struct{}),
		stop: make(chan struct{}), serving: make(chan struct{})}
	go c.read()
	return c
}

// Serving is closed on the first ReadFrom: by then a sipgo server serving this socket has registered it, so
// requests sent from its address reuse it instead of trying to bind the address again.
func (c *Conn) Serving() <-chan struct{} { return c.serving }

func (c *Conn) read() {
	defer close(c.done)
	buf := make([]byte, 65535)
	for {
		n, addr, err := c.PacketConn.ReadFrom(buf)
		if err != nil {
			c.err = err
			return
		}
		b := append([]byte(nil), buf[:n]...)
		m := Summarize(b)
		drop, delay := c.rules.decide(DirIn, m)
		switch {
		case drop:
			c.trace.add(DirIn, m, "dropped")
		case delay > 0:
			c.trace.add(DirIn, m, fmt.Sprintf("delayed %dms", delay.Milliseconds()))
			time.AfterFunc(delay, func() { c.deliver(packet{b, addr}) })
		default:
			c.trace.add(DirIn, m, "")
			c.deliver(packet{b, addr})
		}
	}
}

func (c *Conn) deliver(p packet) {
	select {
	case c.in <- p:
	case <-c.stop:
	}
}

// ReadFrom returns the next message that got through.
func (c *Conn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.reading.Do(func() { close(c.serving) })
	select {
	case p := <-c.in:
		return copy(b, p.b), p.addr, nil
	case <-c.done:
		// Hand over anything already through before reporting the socket closed.
		select {
		case p := <-c.in:
			return copy(b, p.b), p.addr, nil
		default:
		}
		if c.err == nil {
			return 0, nil, net.ErrClosed
		}
		return 0, nil, c.err
	}
}

// WriteTo sends b unless the SIP layer drops or holds it; a dropped message counts as sent.
func (c *Conn) WriteTo(b []byte, addr net.Addr) (int, error) {
	m := Summarize(b)
	drop, delay := c.rules.decide(DirOut, m)
	switch {
	case drop:
		c.trace.add(DirOut, m, "dropped")
		return len(b), nil
	case delay > 0:
		c.trace.add(DirOut, m, fmt.Sprintf("delayed %dms", delay.Milliseconds()))
		held := append([]byte(nil), b...)
		time.AfterFunc(delay, func() {
			select {
			case <-c.stop:
			default:
				_, _ = c.PacketConn.WriteTo(held, addr)
			}
		})
		return len(b), nil
	}
	c.trace.add(DirOut, m, "")
	return c.PacketConn.WriteTo(b, addr)
}

// Close closes the socket; held messages are discarded.
func (c *Conn) Close() error {
	var err error
	c.closed.Do(func() {
		close(c.stop)
		err = c.PacketConn.Close()
	})
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
