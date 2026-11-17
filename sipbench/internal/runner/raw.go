package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/rawsip"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/uas"
)

// UDP resends of a raw message that has had no answer yet, after these delays from the previous send.
var rawResends = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// RawResult is what came back for one raw message.
type RawResult struct {
	ID        string `json:"id"`
	BenchCall string `json:"benchCall"`
	Transport string `json:"transport"`
	// Responses are the status lines that came back, as "+12ms 100 Trying".
	Responses []string `json:"responses"`
	Status    int      `json:"status,omitempty"`
	// Closed is true when the edge closed the TCP connection.
	Closed bool `json:"closed,omitempty"`
	// ByeStatus is the answer to the BYE that ended a call the message set up.
	ByeStatus int        `json:"byeStatus,omitempty"`
	Error     string     `json:"error,omitempty"`
	Backend   []uas.Call `json:"backend,omitempty"`
}

// rawExchange sends one raw message and checks what came back. It returns the result and the failures.
func (r *Runner) rawExchange(ctx context.Context, x scenario.Raw, bench string) (RawResult, []string) {
	res := RawResult{ID: x.ID, BenchCall: bench, Transport: x.Transport, Responses: []string{}}
	if res.Transport == "" {
		res.Transport = transportUDP
	}
	fail := func(err error) (RawResult, []string) {
		res.Error = err.Error()
		return res, []string{err.Error()}
	}
	conn, err := rawsip.Dial(ctx, res.Transport, r.cfg.Edge)
	if err != nil {
		return fail(fmt.Errorf("connecting to the edge: %w", err))
	}
	defer func() { _ = conn.Close() }()
	host, port, _ := net.SplitHostPort(conn.Local())
	vars := map[string]string{
		"edge": r.cfg.Edge, "local": conn.Local(), "localHost": host, "localPort": port,
		"transport": strings.ToUpper(res.Transport), "branch": "z9hG4bK" + rawsip.Token(8),
		"callId": rawsip.Token(8) + "@sipbench", "tag": rawsip.Token(4), "bench": bench,
	}
	msg, err := rawsip.Fill(x.Message, vars, !x.KeepLineEnds)
	if err != nil {
		return fail(fmt.Errorf("message: %w", err))
	}
	// The request as we sent it, for the checks and the ACK; a deliberately malformed one may not parse.
	req, _, reqErr := rawsip.Parse(msg)

	wait := msOr(x.WaitMs, 3*time.Second)
	got := r.rawReceive(conn, msg, wait, x.Expect.Closed)
	final := finalOf(got)
	for _, m := range got {
		res.Responses = append(res.Responses, fmt.Sprintf("+%dms %s", m.At.Milliseconds(), m.StatusLine()))
	}
	res.Closed = conn.Closed()
	if final != nil {
		res.Status = final.Code
	}

	var out []string
	if final != nil && reqErr == nil && strings.EqualFold(req.Method, "INVITE") {
		bye, err := r.rawFinishInvite(conn, &req, final, msOr(x.HangupAfterMs, 500*time.Millisecond))
		res.ByeStatus = bye
		if err != nil {
			out = append(out, err.Error())
		}
	}
	out = append(out, rawChecks(x.Expect, &req, reqErr, got, final, conn.Closed())...)
	if e := x.Expect.Backend; e != nil {
		if r.cfg.MockURL == "" {
			out = append(out, "backend expectations need the mock backend")
		} else {
			time.Sleep(300 * time.Millisecond) // let the mock finish its record
			calls, err := r.mockCalls(ctx, bench)
			if err != nil {
				out = append(out, "reading backend records: "+err.Error())
			}
			res.Backend = calls
			out = append(out, end("backend", calls, *e)...)
		}
	}
	return res, out
}

// rawReceive sends msg and collects what comes back until a final response (and, with untilClosed, the
// connection closing) or wait passes. Over UDP it resends while nothing has come back.
func (r *Runner) rawReceive(conn *rawsip.Conn, msg []byte, wait time.Duration, untilClosed bool) []rawsip.Message {
	var got []rawsip.Message
	if err := conn.Send(msg); err != nil {
		r.log.Warn("raw send", "error", err)
		return nil
	}
	deadline := time.Now().Add(wait)
	resends := slices.Clone(rawResends)
	nextResend := time.Now().Add(resends[0])
	for time.Now().Before(deadline) {
		until := deadline
		if conn.Transport() == transportUDP && len(got) == 0 && len(resends) > 0 && nextResend.Before(until) {
			until = nextResend
		}
		m, err := conn.Recv(until)
		switch {
		case err == nil:
			if m.Code == 0 {
				continue // a request toward us; not ours to answer here
			}
			got = append(got, m)
			if m.Code >= 200 && !untilClosed {
				return got
			}
		case errors.Is(err, io.EOF) || conn.Closed():
			return got
		case errors.Is(err, os.ErrDeadlineExceeded):
			if len(got) == 0 && len(resends) > 0 && !time.Now().Before(nextResend) {
				_ = conn.Send(msg)
				resends = resends[1:]
				if len(resends) > 0 {
					nextResend = time.Now().Add(resends[0])
				}
			}
		default:
			return got
		}
	}
	return got
}

// finalOf returns the first final response in got, or nil.
func finalOf(got []rawsip.Message) *rawsip.Message {
	for i := range got {
		if got[i].Code >= 200 {
			return &got[i]
		}
	}
	return nil
}

// rawFinishInvite acknowledges the final response to an INVITE we sent, and for a 2xx hangs the call up after
// hold. It returns the BYE's answer.
func (r *Runner) rawFinishInvite(conn *rawsip.Conn, req, final *rawsip.Message, hold time.Duration) (int, error) {
	from, _ := req.First("from")
	to, _ := final.First("to")
	callID, _ := req.First("call-id")
	cseqV, _ := req.First("cseq")
	n, _ := strconv.Atoi(strings.Fields(cseqV + " 0")[0])
	if final.Code >= 300 {
		// Non-2xx: the ACK belongs to the INVITE's transaction (same Via, same branch).
		via, _ := req.First("via")
		ack := fmt.Sprintf("ACK %s SIP/2.0\r\nVia: %s\r\nMax-Forwards: 70\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\n"+
			"CSeq: %d ACK\r\nContent-Length: 0\r\n\r\n", req.URI, via, from, to, callID, n)
		return 0, conn.Send([]byte(ack))
	}
	contact, ok := final.First("contact")
	if !ok {
		return 0, errors.New("the 2xx to the INVITE has no Contact, so the call can't be ended")
	}
	target := uriOf(contact)
	// The route set is the Record-Route list, reversed for the caller's side.
	var routes []string
	for _, rr := range final.Get("record-route") {
		for v := range strings.SplitSeq(rr, ",") {
			if v = strings.TrimSpace(v); v != "" {
				routes = append(routes, v)
			}
		}
	}
	slices.Reverse(routes)
	inDialog := func(method string, seq int) []byte {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s SIP/2.0\r\nVia: SIP/2.0/%s %s;branch=z9hG4bK%s\r\nMax-Forwards: 70\r\n",
			method, target, strings.ToUpper(conn.Transport()), conn.Local(), rawsip.Token(8))
		for _, rt := range routes {
			fmt.Fprintf(&b, "Route: %s\r\n", rt)
		}
		fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %d %s\r\nContent-Length: 0\r\n\r\n", from, to, callID, seq, method)
		return []byte(b.String())
	}
	if err := conn.Send(inDialog("ACK", n)); err != nil {
		return 0, err
	}
	time.Sleep(hold)
	bye := finalOf(r.rawReceive(conn, inDialog("BYE", n+1), 5*time.Second, false))
	if bye == nil {
		return 0, errors.New("the BYE ending the call got no final response")
	}
	if bye.Code != 200 {
		return bye.Code, fmt.Errorf("the BYE ending the call got %s", bye.StatusLine())
	}
	return bye.Code, nil
}

// viaWithoutReceived matches the received and rport parameters a server may add to a Via.
var viaWithoutReceived = regexp.MustCompile(`;(received=[^;,\s]*|rport=[0-9]+)`)

// uriOf takes the URI out of a name-addr (<sip:...>) or an addr-spec with parameters.
func uriOf(v string) string {
	if i := strings.Index(v, "<"); i >= 0 {
		if j := strings.Index(v[i:], ">"); j > 0 {
			return v[i+1 : i+j]
		}
	}
	u, _, _ := strings.Cut(strings.TrimSpace(v), ";")
	return u
}

func rawChecks(e scenario.RawExpect, req *rawsip.Message, reqErr error, got []rawsip.Message, final *rawsip.Message, closed bool) []string {
	var out []string
	if e.NoResponse {
		if len(got) > 0 {
			out = append(out, fmt.Sprintf("expected no response, got %s", got[0].StatusLine()))
		}
		return out
	}
	if e.Status != 0 {
		switch {
		case final == nil:
			out = append(out, fmt.Sprintf("no final response, want %d", e.Status))
		case final.Code != e.Status:
			out = append(out, fmt.Sprintf("final response %s, want %d", final.StatusLine(), e.Status))
		}
	}
	if e.Closed && !closed {
		out = append(out, "the edge kept the TCP connection open")
	}
	for _, code := range e.Provisional {
		if !slices.ContainsFunc(got, func(m rawsip.Message) bool { return m.Code == code }) {
			out = append(out, fmt.Sprintf("no %d provisional response", code))
		}
	}
	if e.WithinMs > 0 && len(got) > 0 && got[0].At > time.Duration(e.WithinMs)*time.Millisecond {
		out = append(out, fmt.Sprintf("first response after %dms, want within %dms", got[0].At.Milliseconds(), e.WithinMs))
	}
	if final != nil {
		for name, want := range e.Headers {
			vs := final.Get(name)
			switch {
			case len(vs) == 0:
				out = append(out, fmt.Sprintf("final response has no %s header", name))
			case want != "" && !strings.Contains(strings.ToLower(strings.Join(vs, ", ")), strings.ToLower(want)):
				out = append(out, fmt.Sprintf("final response %s = %q, want it to contain %q", name, strings.Join(vs, ", "), want))
			}
		}
	}
	if len(e.Copied) > 0 && len(got) > 0 {
		if reqErr != nil {
			out = append(out, "copied: the request itself doesn't parse: "+reqErr.Error())
		} else {
			for _, name := range e.Copied {
				sent, back := strings.Join(req.Get(name), "\n"), strings.Join(got[0].Get(name), "\n")
				if strings.EqualFold(name, "via") {
					// A server adds received (and fills rport) when the source address differs from the Via's, as
					// RFC 3261 18.2.1 and RFC 3581 have it; the rest must come back unchanged.
					back = viaWithoutReceived.ReplaceAllString(back, "")
				}
				if sent != back {
					out = append(out, fmt.Sprintf("%s in %s is %q, want %q as sent", name, got[0].StatusLine(), back, sent))
				}
			}
		}
	}
	return out
}
