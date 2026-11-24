package calltrace

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// Message is what the tracer reads from a SIP message: the start line and a few headers. The body is ignored.
type Message struct {
	Request bool
	// Method is the request's method, or for a response the method in its CSeq.
	Method     string
	RequestURI string
	Status     int
	Phrase     string
	CallID     string
	CSeq       uint32
	FromTag    string
	ToTag      string
	FromUser   string
	ToUser     string
	// Headers by lower-case name (compact forms expanded), each value as it appeared.
	Headers map[string][]string
}

// Header returns the first value of a header, by name in any case.
func (m *Message) Header(name string) string {
	if v := m.Headers[strings.ToLower(name)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Final reports whether m is a final response.
func (m *Message) Final() bool { return !m.Request && m.Status >= 200 }

var compactHeaders = map[string]string{
	"i": "call-id", "f": "from", "t": "to", "v": "via", "m": "contact", "l": "content-length",
	"c": "content-type", "k": "supported", "s": "subject", "e": "content-encoding", "o": "event",
	"r": "refer-to", "b": "referred-by", "x": "session-expires", "u": "allow-events",
}

// ParseSIP parses a SIP message's start line and headers.
func ParseSIP(b []byte) (*Message, error) {
	head, _, _ := bytes.Cut(b, []byte("\r\n\r\n"))
	lines := strings.Split(string(head), "\r\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, errors.New("empty SIP message")
	}
	m := &Message{Headers: map[string][]string{}}
	start := strings.SplitN(lines[0], " ", 3)
	if len(start) < 3 {
		return nil, errors.New("malformed start line")
	}
	if start[0] == "SIP/2.0" {
		code, err := strconv.Atoi(start[1])
		if err != nil || code < 100 || code > 699 {
			return nil, errors.New("malformed status line")
		}
		m.Status, m.Phrase = code, start[2]
	} else if start[2] == "SIP/2.0" {
		m.Request, m.Method, m.RequestURI = true, start[0], start[1]
	} else {
		return nil, errors.New("not a SIP message")
	}
	var name string
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && name != "" { // folded onto the previous header
			vs := m.Headers[name]
			vs[len(vs)-1] += " " + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(k))
		if full, ok := compactHeaders[name]; ok {
			name = full
		}
		m.Headers[name] = append(m.Headers[name], strings.TrimSpace(v))
	}
	m.CallID = m.Header("call-id")
	if m.CallID == "" {
		return nil, errors.New("no Call-ID")
	}
	num, method, _ := strings.Cut(m.Header("cseq"), " ")
	n, err := strconv.ParseUint(strings.TrimSpace(num), 10, 32)
	if err != nil {
		return nil, errors.New("malformed CSeq")
	}
	m.CSeq = uint32(n)
	if !m.Request {
		m.Method = strings.TrimSpace(method)
	}
	from, to := m.Header("from"), m.Header("to")
	m.FromTag, m.ToTag = param(from, "tag"), param(to, "tag")
	m.FromUser, m.ToUser = uriUser(from), uriUser(to)
	return m, nil
}

// param returns a header parameter (after the URI's closing >, if any).
func param(h, name string) string {
	if i := strings.LastIndexByte(h, '>'); i >= 0 {
		h = h[i+1:]
	} else if i := strings.IndexByte(h, ';'); i >= 0 {
		h = h[i:]
	} else {
		return ""
	}
	for p := range strings.SplitSeq(h, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// uriUser returns the user part of a From or To header's URI.
func uriUser(h string) string {
	if i := strings.IndexByte(h, '<'); i >= 0 {
		h = h[i+1:]
		if j := strings.IndexByte(h, '>'); j >= 0 {
			h = h[:j]
		}
	} else if j := strings.IndexByte(h, ';'); j >= 0 {
		h = h[:j]
	}
	_, rest, ok := strings.Cut(h, ":")
	if !ok {
		return ""
	}
	user, _, ok := strings.Cut(rest, "@")
	if !ok {
		return ""
	}
	user, _, _ = strings.Cut(user, ";")
	return user
}

// uriHost returns a SIP URI's host (and port), without user or parameters.
func uriHost(uri string) string {
	_, rest, ok := strings.Cut(uri, ":")
	if !ok {
		return ""
	}
	if _, host, ok := strings.Cut(rest, "@"); ok {
		rest = host
	}
	rest, _, _ = strings.Cut(rest, ";")
	rest, _, _ = strings.Cut(rest, "?")
	return rest
}
