package calltrace

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Config tunes a Tracker. Zero values take the defaults.
type Config struct {
	// CaptureNames names the components by HEP capture ID; they become the spans' service names. An unnamed one
	// is hep-<id>.
	CaptureNames map[uint32]string
	// CorrelationHeader carries the Call-ID a call arrived with onto the legs a B2BUA starts for it (default X-CID,
	// as Homer uses). A leg whose INVITE has it joins that call's trace.
	CorrelationHeader string
	// Grace is how long after a call ends the trace waits for stragglers (LiveKit's participant_left and
	// room_finished) before it is closed. Default 60s.
	Grace time.Duration
	// SetupTimeout closes a call that has had no final answer and no message for this long. Default 3m.
	SetupTimeout time.Duration
	// MaxCallAge closes any call this old. Default 4h.
	MaxCallAge time.Duration
	// MaxCalls bounds the calls traced at once; past it the oldest is closed. Default 20000.
	MaxCalls int
	// LiveKitService is the service name of LiveKit's spans. Default livekit.
	LiveKitService string
}

func (c Config) withDefaults() Config {
	if c.CorrelationHeader == "" {
		c.CorrelationHeader = "X-CID"
	}
	if c.Grace == 0 {
		c.Grace = 60 * time.Second
	}
	if c.SetupTimeout == 0 {
		c.SetupTimeout = 3 * time.Minute
	}
	if c.MaxCallAge == 0 {
		c.MaxCallAge = 4 * time.Hour
	}
	if c.MaxCalls == 0 {
		c.MaxCalls = 20000
	}
	if c.LiveKitService == "" {
		c.LiveKitService = "livekit"
	}
	return c
}

const (
	methodInvite        = "INVITE"
	evParticipantJoined = "participant_joined"
	evParticipantLeft   = "participant_left"
	evRoomStarted       = "room_started"
	evRoomFinished      = "room_finished"
	evTrackPublished    = "track_published"
	attrRoomName        = "livekit.room.name"
)

// settle is how long a finished transaction waits before its span goes out, for the other end's copies of its
// messages to arrive.
const settle = 2 * time.Second

// Tracker builds calls out of SIP messages and LiveKit webhooks, and hands back spans as they finish. It is safe
// for concurrent use.
type Tracker struct {
	cfg     Config
	metrics *Metrics
	now     func() time.Time

	mu       sync.Mutex
	calls    map[TraceID]*call
	byCallID map[string]*call
	byRoom   map[string]*call
	pending  map[string]*pendingRoom
	// endpoints maps an address to the component (capture ID) learned to own it: when two components capture the
	// same request, the one that captured it first sent it.
	endpoints map[netip.AddrPort]uint32
}

// NewTracker returns a tracker. now may be nil (time.Now).
func NewTracker(cfg Config, m *Metrics, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	if m == nil {
		m = NewMetrics()
	}
	return &Tracker{
		cfg: cfg.withDefaults(), metrics: m, now: now,
		calls: map[TraceID]*call{}, byCallID: map[string]*call{}, byRoom: map[string]*call{},
		pending: map[string]*pendingRoom{}, endpoints: map[netip.AddrPort]uint32{},
	}
}

type txnKey struct {
	callID   string
	cseq     uint32
	method   string
	src, dst netip.AddrPort
}

type call struct {
	trace      TraceID
	rootCallID string
	callIDs    []string
	start      time.Time
	last       time.Time
	// endAt is when the call ended (a BYE answered, or the caller's INVITE refused); zero while it lasts.
	endAt    time.Time
	list     []*txn
	first    *txn // the caller's INVITE, on the first hop
	fromUser string
	toUser   string
	answered time.Time
	agentAt  time.Time
	hangup   string
	room     *room
}

type txn struct {
	key       txnKey
	id        SpanID
	initial   bool // an INVITE starting a dialog
	start     time.Time
	firstCap  uint32
	reporters map[uint32]bool
	events    []Event
	seen      map[string]int // event name -> index
	status    int
	phrase    string
	end       time.Time
	retrans   int
	ruriHost  string
	userAgent string
	reason    string
	fromTag   string
	emitted   bool
}

type room struct {
	name, sid  string
	id         SpanID
	sipCallID  string
	start, end time.Time
	events     []Event
	parts      map[string]*participant
	order      []*participant
}

type participant struct {
	sid, identity, kind string
	attrs               []Attr
	start, end          time.Time
	events              []Event
}

type pendingRoom struct {
	sid    string
	start  time.Time
	seen   time.Time
	events []queuedEvent
}

type queuedEvent struct {
	ev *LiveKitEvent
	at time.Time
}

func (t *Tracker) name(captureID uint32) string {
	if n, ok := t.cfg.CaptureNames[captureID]; ok {
		return n
	}
	return "hep-" + strconv.FormatUint(uint64(captureID), 10)
}

// HandleHEP decodes a HEP datagram and traces its SIP message. It returns spans to export (only when a new call
// pushes out the oldest).
func (t *Tracker) HandleHEP(b []byte) []Span {
	p, err := DecodeHEP(b)
	if err != nil {
		t.metrics.HEPPackets.WithLabelValues("bad").Inc()
		return nil
	}
	if p.PayloadType != hepProtoSIP {
		t.metrics.HEPPackets.WithLabelValues("other").Inc()
		return nil
	}
	t.metrics.HEPPackets.WithLabelValues("sip").Inc()
	msg, err := ParseSIP(p.Payload)
	if err != nil {
		t.metrics.SIPMessages.WithLabelValues("bad").Inc()
		return nil
	}
	return t.HandleSIP(p, msg)
}

// HandleSIP traces one captured SIP message.
func (t *Tracker) HandleSIP(p Packet, msg *Message) []Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Span
	c := t.byCallID[msg.CallID]
	if c == nil {
		if !msg.Request || msg.Method != methodInvite || msg.ToTag != "" {
			t.metrics.SIPMessages.WithLabelValues("untraced").Inc()
			return nil
		}
		root := msg.CallID
		if v := msg.Header(t.cfg.CorrelationHeader); v != "" {
			root = v
		}
		trace := TraceIDFor(root)
		if c = t.calls[trace]; c == nil {
			out = t.makeRoom()
			c = &call{trace: trace, rootCallID: root, start: p.Time, last: p.Time}
			t.calls[trace] = c
			t.metrics.ActiveCalls.Set(float64(len(t.calls)))
		}
		c.callIDs = append(c.callIDs, msg.CallID)
		t.byCallID[msg.CallID] = c
	}
	t.metrics.SIPMessages.WithLabelValues("traced").Inc()
	// A component listening on a wildcard address (Kamailio on 0.0.0.0) captures its own side as that: it is
	// the one address in the copy known to be the capturing component's.
	for _, a := range []netip.AddrPort{p.Src, p.Dst} {
		if a.Addr().IsUnspecified() {
			t.endpoints[a] = p.CaptureID
		}
	}
	if p.Time.After(c.last) {
		c.last = p.Time
	}
	if p.Time.Before(c.start) {
		c.start = p.Time
	}
	switch {
	case msg.Request && msg.Method == "ACK":
		if tx := c.find(txnKey{msg.CallID, msg.CSeq, methodInvite, p.Src, p.Dst}); tx != nil {
			tx.event(p.Time, "ACK")
		}
	case msg.Request:
		t.request(c, p, msg)
	default:
		t.response(c, p, msg)
	}
	return out
}

func (t *Tracker) request(c *call, p Packet, msg *Message) {
	key := txnKey{msg.CallID, msg.CSeq, msg.Method, p.Src, p.Dst}
	if tx := c.find(key); tx != nil {
		if tx.reporters[p.CaptureID] {
			tx.retrans++
			return
		}
		tx.reporters[p.CaptureID] = true
		tx.key.src, tx.key.dst = specific(tx.key.src, key.src), specific(tx.key.dst, key.dst)
		if p.Time.Before(tx.start) {
			tx.start, tx.firstCap = p.Time, p.CaptureID
		}
		return
	}
	tx := &txn{
		key: key, id: spanIDFor(c.trace, "sip", key.callID, strconv.FormatUint(uint64(key.cseq), 10), key.method,
			key.src.String(), key.dst.String()),
		initial: msg.Method == methodInvite && msg.ToTag == "", start: p.Time, firstCap: p.CaptureID,
		reporters: map[uint32]bool{p.CaptureID: true}, seen: map[string]int{},
		ruriHost: uriHost(msg.RequestURI), userAgent: msg.Header("user-agent"), reason: msg.Header("reason"),
		fromTag: msg.FromTag,
	}
	c.list = append(c.list, tx)
	if tx.initial && c.first == nil {
		c.first, c.fromUser, c.toUser = tx, msg.FromUser, msg.ToUser
	}
}

func (t *Tracker) response(c *call, p Packet, msg *Message) {
	tx := c.find(txnKey{msg.CallID, msg.CSeq, msg.Method, p.Dst, p.Src})
	if tx == nil {
		return
	}
	if !msg.Final() {
		tx.event(p.Time, strconv.Itoa(msg.Status)+" "+msg.Phrase)
		return
	}
	if tx.status == 0 || (msg.Status == tx.status && p.Time.Before(tx.end)) {
		tx.status, tx.phrase, tx.end = msg.Status, msg.Phrase, p.Time
		if r := msg.Header("reason"); r != "" {
			tx.reason = r
		}
	}
	switch {
	case tx == c.first && msg.Status < 300:
		if c.answered.IsZero() || p.Time.Before(c.answered) {
			c.answered = p.Time
		}
	case tx == c.first:
		c.ended(p.Time)
	case tx.key.method == "BYE":
		c.ended(p.Time)
		if c.hangup == "" && c.first != nil {
			if tx.fromTag == c.first.fromTag {
				c.hangup = "caller"
			} else {
				c.hangup = "callee"
			}
		}
	}
}

// find returns the transaction k belongs to. An unspecified address (a wildcard listener's own) matches any address
// on the same port, so both ends' copies of a message land in one transaction.
func (c *call) find(k txnKey) *txn {
	for _, tx := range c.list {
		o := tx.key
		if o.callID == k.callID && o.cseq == k.cseq && o.method == k.method && sameEnd(o.src, k.src) &&
			sameEnd(o.dst, k.dst) {
			return tx
		}
	}
	return nil
}

func sameEnd(a, b netip.AddrPort) bool {
	return a == b || (a.Port() == b.Port() && (a.Addr().IsUnspecified() || b.Addr().IsUnspecified()))
}

// specific returns whichever of two matching addresses is not a wildcard.
func specific(a, b netip.AddrPort) netip.AddrPort {
	if a.Addr().IsUnspecified() {
		return b
	}
	return a
}

func (c *call) ended(at time.Time) {
	if c.endAt.IsZero() || at.Before(c.endAt) {
		c.endAt = at
	}
}

func (tx *txn) event(at time.Time, name string) {
	if i, ok := tx.seen[name]; ok {
		if at.Before(tx.events[i].Time) {
			tx.events[i].Time = at
		}
		return
	}
	tx.seen[name] = len(tx.events)
	tx.events = append(tx.events, Event{Time: at, Name: name})
}

// makeRoom closes the oldest call when a new one would pass MaxCalls. Called with t.mu held.
func (t *Tracker) makeRoom() []Span {
	if len(t.calls) < t.cfg.MaxCalls {
		return nil
	}
	var oldest *call
	for _, c := range t.calls {
		if oldest == nil || c.start.Before(oldest.start) {
			oldest = c
		}
	}
	return t.close(oldest, "evicted", t.now())
}

// HandleLiveKit traces one verified LiveKit webhook.
func (t *Tracker) HandleLiveKit(ev *LiveKitEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	at := t.now()
	switch ev.Event {
	case evRoomStarted, evRoomFinished, evParticipantJoined, evParticipantLeft, evTrackPublished:
	default:
		t.metrics.Webhooks.WithLabelValues("ignored").Inc()
		return
	}
	if ev.Room == nil || ev.Room.Name == "" {
		t.metrics.Webhooks.WithLabelValues("untraced").Inc()
		return
	}
	c := t.byRoom[ev.Room.Name]
	if c == nil && ev.Event == evParticipantJoined && ev.Participant != nil {
		if c = t.callFor(ev.Participant.Attributes); c != nil {
			t.linkRoom(c, ev.Room, ev.Participant.Attributes)
		}
	}
	if c == nil {
		pr := t.pending[ev.Room.Name]
		if pr == nil {
			pr = &pendingRoom{}
			t.pending[ev.Room.Name] = pr
		}
		pr.seen = at
		if ev.Event == evRoomStarted {
			pr.sid, pr.start = ev.Room.SID, roomStart(ev.Room, at)
		} else {
			pr.events = append(pr.events, queuedEvent{ev, at})
		}
		return
	}
	t.metrics.Webhooks.WithLabelValues("traced").Inc()
	t.roomEvent(c, ev, at)
}

func roomStart(r *LiveKitRoom, at time.Time) time.Time {
	if r.CreationTime > 0 {
		// creationTime is in whole seconds; the webhook's own arrival is more precise unless it came late.
		if ct := time.Unix(int64(r.CreationTime), 0); at.Sub(ct) > 2*time.Second {
			return ct
		}
	}
	return at
}

// callFor finds the call a SIP participant belongs to: by the Call-ID of the leg that reached LiveKit
// (sip.callIDFull), or by the correlation header when the trunk passes it on as an attribute.
func (t *Tracker) callFor(attrs map[string]string) *call {
	if id := attrs["sip.callIDFull"]; id != "" {
		if c := t.byCallID[id]; c != nil {
			return c
		}
	}
	if id := attrs["sip.h."+strings.ToLower(t.cfg.CorrelationHeader)]; id != "" {
		if c := t.calls[TraceIDFor(id)]; c != nil {
			return c
		}
		return t.byCallID[id]
	}
	return nil
}

func (t *Tracker) linkRoom(c *call, r *LiveKitRoom, attrs map[string]string) {
	if c.room != nil && c.room.name != r.Name {
		delete(t.byRoom, c.room.name) // a call moved to another room: trace the latest
	}
	rm := &room{name: r.Name, sid: r.SID, parts: map[string]*participant{}, sipCallID: attrs["sip.callIDFull"]}
	t.byRoom[r.Name] = c
	c.room = rm
	if pr := t.pending[r.Name]; pr != nil {
		delete(t.pending, r.Name)
		if pr.sid != "" {
			rm.sid = pr.sid
		}
		rm.start = pr.start
		for _, q := range pr.events {
			t.metrics.Webhooks.WithLabelValues("traced").Inc()
			t.roomEvent(c, q.ev, q.at)
		}
	}
	rm.id = spanIDFor(c.trace, "room", r.Name, rm.sid)
}

func (t *Tracker) roomEvent(c *call, ev *LiveKitEvent, at time.Time) {
	rm := c.room
	if rm.start.IsZero() || (ev.Event == evRoomStarted && roomStart(ev.Room, at).Before(rm.start)) {
		rm.start = roomStart(ev.Room, at)
	}
	if rm.sid == "" {
		rm.sid = ev.Room.SID
	}
	switch ev.Event {
	case evRoomFinished:
		rm.end = at
	case evParticipantJoined, evParticipantLeft, evTrackPublished:
		pp := ev.Participant
		if pp == nil || pp.SID == "" {
			return
		}
		p := rm.parts[pp.SID]
		if p == nil {
			p = &participant{sid: pp.SID, identity: pp.Identity, kind: pp.Kind, start: at}
			if j := pp.joined(); !j.IsZero() && j.Before(at) && at.Sub(j) < time.Minute {
				p.start = j
			}
			rm.parts[pp.SID] = p
			rm.order = append(rm.order, p)
			if p.start.Before(rm.start) {
				rm.start = p.start
			}
		}
		if len(p.attrs) == 0 {
			p.attrs = participantAttrs(pp.Attributes)
		}
		switch ev.Event {
		case evParticipantJoined:
			if p.kind == "AGENT" && (c.agentAt.IsZero() || p.start.Before(c.agentAt)) {
				c.agentAt = p.start
			}
			if id := pp.Attributes["sip.callIDFull"]; id != "" && rm.sipCallID == "" {
				rm.sipCallID = id
			}
		case evParticipantLeft:
			p.end = at
		case evTrackPublished:
			var a []Attr
			if ev.Track != nil {
				a = []Attr{{"livekit.track.type", ev.Track.Type}, {"livekit.track.source", ev.Track.Source},
					{"livekit.track.sid", ev.Track.SID}}
			}
			p.events = append(p.events, Event{Time: at, Name: "track published", Attrs: a})
		}
	}
}

// participantAttrs keeps a participant's sip.* and lk.* attributes, except headers passed through from SIP.
func participantAttrs(in map[string]string) []Attr {
	keys := make([]string, 0, len(in))
	for k := range in {
		if (strings.HasPrefix(k, "sip.") || strings.HasPrefix(k, "lk.")) && !strings.HasPrefix(k, "sip.h.") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := make([]Attr, 0, len(keys))
	for _, k := range keys {
		out = append(out, Attr{"livekit.attr." + k, in[k]})
	}
	return out
}

// Sweep closes calls that are done and returns the spans finished since the last sweep. Call it every second or so.
func (t *Tracker) Sweep() []Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []Span
	for _, c := range t.calls {
		switch {
		case !c.endAt.IsZero() && now.Sub(c.endAt) >= t.cfg.Grace:
			out = append(out, t.close(c, "ended", now)...)
		case now.Sub(c.start) >= t.cfg.MaxCallAge,
			c.answered.IsZero() && c.endAt.IsZero() && now.Sub(c.last) >= t.cfg.SetupTimeout:
			out = append(out, t.close(c, "timeout", now)...)
		default:
			var ready []*txn
			for _, tx := range c.list {
				if !tx.emitted && !tx.end.IsZero() && now.Sub(tx.end) >= settle {
					t.learn(tx)
					ready = append(ready, tx)
				}
			}
			for _, tx := range ready {
				out = append(out, t.txnSpan(c, tx))
			}
		}
	}
	for name, pr := range t.pending {
		if now.Sub(pr.seen) >= 5*time.Minute {
			t.metrics.Webhooks.WithLabelValues("untraced").Add(float64(len(pr.events)))
			delete(t.pending, name)
		}
	}
	return out
}

// Flush closes every call, for shutdown.
func (t *Tracker) Flush() []Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Span, 0, 8*len(t.calls))
	for _, c := range t.calls {
		out = append(out, t.close(c, "shutdown", t.now())...)
	}
	return out
}

// close emits whatever of c is left, its root span last, and forgets it. Called with t.mu held.
func (t *Tracker) close(c *call, how string, now time.Time) []Span {
	var out []Span
	for _, tx := range c.list {
		if !tx.emitted {
			t.learn(tx)
		}
	}
	for _, tx := range c.list {
		if !tx.emitted {
			out = append(out, t.txnSpan(c, tx))
		}
	}
	end := c.last
	if !c.endAt.IsZero() && c.endAt.After(end) {
		end = c.endAt
	}
	if rm := c.room; rm != nil {
		out = append(out, t.roomSpans(c, rm, now)...)
		for _, s := range out {
			if s.End.After(end) {
				end = s.End
			}
		}
		delete(t.byRoom, rm.name)
	}
	out = append(out, t.rootSpan(c, how, end))
	for _, id := range c.callIDs {
		delete(t.byCallID, id)
	}
	delete(t.calls, c.trace)
	t.metrics.ActiveCalls.Set(float64(len(t.calls)))
	t.metrics.Calls.WithLabelValues(how).Inc()
	return out
}

func (c *call) rootID() SpanID { return spanIDFor(c.trace, "call") }

// effectiveEnd is when a transaction ended: its final answer, or, for one never answered, when the next branch
// of the same request started (a proxy failing over), or the call's last message.
func (c *call) effectiveEnd(tx *txn) (time.Time, bool) {
	if !tx.end.IsZero() {
		return tx.end, true
	}
	for _, o := range c.list {
		if o != tx && o.initial == tx.initial && o.key.callID == tx.key.callID && o.key.method == tx.key.method &&
			sameEnd(o.key.src, tx.key.src) && o.start.After(tx.start) {
			return o.start, false
		}
	}
	return c.last, false
}

// parentOf is the span a transaction belongs under. A new INVITE goes under the latest INVITE still open when it
// started: the hop it continues. Anything else goes under the call, except a CANCEL, under the INVITE it cancels.
func (c *call) parentOf(tx *txn) SpanID {
	switch {
	case tx.initial:
		var best *txn
		for _, o := range c.list {
			if o == tx || !o.initial || o.start.After(tx.start) || (o.start.Equal(tx.start) && !c.before(o, tx)) {
				continue
			}
			if end, _ := c.effectiveEnd(o); end.Before(tx.start) || (end.Equal(tx.start) && o.end.IsZero()) {
				continue
			}
			if best == nil || o.start.After(best.start) {
				best = o
			}
		}
		if best != nil {
			return best.id
		}
	case tx.key.method == "CANCEL":
		k := tx.key
		k.method = methodInvite
		if inv := c.find(k); inv != nil {
			return inv.id
		}
	}
	return c.rootID()
}

func (c *call) before(a, b *txn) bool {
	for _, o := range c.list {
		if o == a {
			return true
		}
		if o == b {
			return false
		}
	}
	return false
}

// learn records which component owns each end of a request both ends captured: the one that captured it first
// sent it.
func (t *Tracker) learn(tx *txn) {
	if len(tx.reporters) > 1 {
		t.endpoints[tx.key.src] = tx.firstCap
		for r := range tx.reporters {
			if r != tx.firstCap {
				t.endpoints[tx.key.dst] = r
			}
		}
	}
}

func (t *Tracker) txnSpan(c *call, tx *txn) Span {
	tx.emitted = true
	k := tx.key
	owner := t.name(tx.firstCap)
	name := k.method
	kind := tracepb.Span_SPAN_KIND_INTERNAL
	if d, ok := t.endpoints[k.dst]; ok && d != tx.firstCap {
		name += " to " + t.name(d)
		kind = tracepb.Span_SPAN_KIND_CLIENT
	} else if ok && d == tx.firstCap {
		kind = tracepb.Span_SPAN_KIND_SERVER
	} else if s, ok := t.endpoints[k.src]; ok && s == tx.firstCap {
		kind = tracepb.Span_SPAN_KIND_CLIENT
	}
	end, answered := c.effectiveEnd(tx)
	s := Span{
		Trace: c.trace, ID: tx.id, Parent: c.parentOf(tx), Service: owner, Name: name, Kind: kind,
		Start: tx.start, End: end, Events: tx.events,
		Attrs: []Attr{
			{"sip.method", k.method}, {"sip.call_id", k.callID}, {"sip.cseq", int64(k.cseq)},
			{"net.src", k.src.String()}, {"net.dst", k.dst.String()}, {"sip.request_uri.host", tx.ruriHost},
			{"sip.user_agent", tx.userAgent}, {"sip.reason", tx.reason},
			{"sip.capture_points", len(tx.reporters)},
		},
	}
	if tx.retrans > 0 {
		s.Attrs = append(s.Attrs, Attr{"sip.retransmissions", tx.retrans})
	}
	if answered {
		s.Attrs = append(s.Attrs, Attr{"sip.status_code", tx.status}, Attr{"sip.reason_phrase", tx.phrase})
		s.Error = failed(tx.status)
		s.Status = strconv.Itoa(tx.status) + " " + tx.phrase
	} else {
		s.Attrs = append(s.Attrs, Attr{"sip.no_final_response", true})
		s.Error, s.Status = true, "no final response"
	}
	return s
}

// failed reports whether a final status counts as an error: 4xx-6xx, except authentication challenges and a
// cancelled request.
func failed(status int) bool {
	return status >= 400 && status != 401 && status != 407 && status != 487
}

func (t *Tracker) roomSpans(c *call, rm *room, now time.Time) []Span {
	parent := c.rootID()
	var hop *txn
	for _, tx := range c.list {
		if tx.initial && tx.key.callID == rm.sipCallID && (hop == nil || tx.start.After(hop.start)) {
			hop = tx
		}
	}
	if hop != nil {
		parent = hop.id
	}
	last := rm.start
	out := make([]Span, 0, len(rm.order))
	for _, p := range rm.order {
		end, incomplete := p.end, p.end.IsZero()
		if incomplete {
			end = now
			if !rm.end.IsZero() {
				end = rm.end
			}
		}
		if end.After(last) {
			last = end
		}
		kind := strings.ToLower(p.kind)
		if kind == "" {
			kind = "standard"
		}
		s := Span{
			Trace: c.trace, ID: spanIDFor(c.trace, "participant", p.sid), Parent: rm.id,
			Service: t.cfg.LiveKitService, Name: kind + " participant", Kind: tracepb.Span_SPAN_KIND_INTERNAL,
			Start: p.start, End: end, Events: p.events,
			Attrs: append([]Attr{{"livekit.participant.identity", p.identity}, {"livekit.participant.sid", p.sid},
				{"livekit.participant.kind", p.kind}, {attrRoomName, rm.name}}, p.attrs...),
		}
		if incomplete {
			s.Attrs = append(s.Attrs, Attr{"livekit.left_unseen", true})
		}
		out = append(out, s)
	}
	end := rm.end
	if end.IsZero() {
		end = last
	}
	room := Span{
		Trace: c.trace, ID: rm.id, Parent: parent, Service: t.cfg.LiveKitService, Name: "room",
		Kind: tracepb.Span_SPAN_KIND_SERVER, Start: rm.start, End: end, Events: rm.events,
		Attrs: []Attr{{attrRoomName, rm.name}, {"livekit.room.sid", rm.sid},
			{"livekit.participants", len(rm.order)}},
	}
	if rm.end.IsZero() {
		room.Attrs = append(room.Attrs, Attr{"livekit.finished_unseen", true})
	}
	return append([]Span{room}, out...)
}

// rootStartLead is how much earlier than the call's first message the call span starts. The first transaction's span
// starts at that message too, and trace lists (Grafana's Jaeger search, for one) name a trace and give its duration
// from its earliest span, taking whichever the backend returned first on a tie: often the INVITE, not the call. One
// microsecond is Jaeger's resolution. The call's own timings (call.answer_ms, ...) still count from the first message.
const rootStartLead = time.Microsecond

func (t *Tracker) rootSpan(c *call, how string, end time.Time) Span {
	s := Span{
		Trace: c.trace, ID: c.rootID(), Service: "sip", Name: "call", Kind: tracepb.Span_SPAN_KIND_SERVER,
		Start: c.start.Add(-rootStartLead), End: end,
		Attrs: []Attr{
			{"sip.call_id", c.rootCallID}, {"sip.from.user", c.fromUser}, {"sip.to.user", c.toUser},
			{"call.legs", len(c.callIDs)}, {"call.trace_end", how},
		},
	}
	if c.first != nil {
		s.Service = t.name(c.first.firstCap)
		if c.first.status != 0 {
			s.Attrs = append(s.Attrs, Attr{"sip.status_code", c.first.status})
		}
	}
	outcome := "no_answer"
	switch {
	case !c.answered.IsZero():
		outcome = "answered"
		s.Attrs = append(s.Attrs, Attr{"call.answer_ms", c.answered.Sub(c.start).Milliseconds()})
		if !c.endAt.IsZero() {
			s.Attrs = append(s.Attrs, Attr{"call.talk_ms", c.endAt.Sub(c.answered).Milliseconds()})
		}
	case c.first != nil && c.first.status == 487:
		outcome = "cancelled"
	case c.first != nil && c.first.status >= 300:
		outcome = "refused"
		s.Error, s.Status = true, strconv.Itoa(c.first.status)+" "+c.first.phrase
	default:
		s.Error, s.Status = true, "no final response"
	}
	s.Attrs = append(s.Attrs, Attr{"call.outcome", outcome}, Attr{"call.hangup", c.hangup})
	if !c.agentAt.IsZero() {
		s.Attrs = append(s.Attrs, Attr{"call.agent_join_ms", c.agentAt.Sub(c.start).Milliseconds()})
	}
	if c.room != nil {
		s.Attrs = append(s.Attrs, Attr{attrRoomName, c.room.name})
	}
	return s
}
