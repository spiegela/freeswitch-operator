package runner

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/lkagent"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/netem"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/uas"
)

// check compares what a call saw against its expectations and returns each mismatch.
func check(c scenario.Call, cr CallResult, haveMock bool) []string {
	var out []string
	e := c.Expect
	if cr.Error != "" {
		out = append(out, "error: "+cr.Error)
	}
	if cr.Status != e.Status {
		out = append(out, fmt.Sprintf("status %d, want %d", cr.Status, e.Status))
	}
	for k, want := range e.ResponseHeaders {
		got, ok := cr.ResponseHeaders[strings.ToLower(k)]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("response header %s missing", k))
		case want != "" && got != want:
			out = append(out, fmt.Sprintf("response header %s = %q, want %q", k, got, want))
		}
	}
	out = append(out, tones("caller", cr.TonesHeard, e.TonesHeard, e.TonesNotHeard)...)
	out = append(out, gaps("caller", cr.AudioGapsMs, e.TonesHeard, e.MaxAudioGapMs)...)
	if e.Digits != nil && cr.Digits != *e.Digits {
		out = append(out, fmt.Sprintf("caller digits %q, want %q", cr.Digits, *e.Digits))
	}
	if e.EndedBy != "" && cr.EndedBy != e.EndedBy {
		out = append(out, fmt.Sprintf("ended by %q, want %q", cr.EndedBy, e.EndedBy))
	}
	if e.Codec != "" && !strings.EqualFold(cr.Codec, e.Codec) {
		out = append(out, fmt.Sprintf("caller codec %s, want %s", cr.Codec, e.Codec))
	}
	out = append(out, measures(e, cr)...)
	out = append(out, sipChecks(e.SIP, cr.SIP)...)
	if e.Backend != nil && haveMock {
		out = append(out, end("backend", cr.Backend, *e.Backend)...)
	}
	if e.Target != nil {
		out = append(out, end("target", cr.Target, *e.Target)...)
	}
	if e.LiveKit != nil {
		out = append(out, livekit(cr.LiveKit, *e.LiveKit)...)
	}
	return out
}

// livekit checks what the stand-in LiveKit agent saw of a call: the participant LiveKit SIP brought into its room
// (the last, if the call brought more than one) and the data packets that participant sent.
func livekit(got []lkagent.Participant, x scenario.LiveKitExpect) []string {
	if !x.WantJoined() {
		if len(got) > 0 {
			return []string{fmt.Sprintf("a participant (%s) joined the LiveKit room, want none", got[0].Identity)}
		}
		return nil
	}
	if len(got) == 0 {
		return []string{"no participant joined the LiveKit room for the call"}
	}
	p := got[len(got)-1]
	var out []string
	for _, k := range slices.Sorted(maps.Keys(x.Attributes)) {
		if v, ok := p.Attributes[k]; !ok || v != x.Attributes[k] {
			out = append(out, fmt.Sprintf("LiveKit participant attribute %s = %q, want %q", k, v, x.Attributes[k]))
		}
	}
	// Data packets match as INFOs do: the topic in the content type's place.
	infos := make([]uas.Info, len(p.Data))
	for i, d := range p.Data {
		infos[i] = uas.Info{At: d.At, ContentType: d.Topic, Body: d.Payload}
	}
	for _, d := range x.Data {
		n := 0
		for _, in := range infos {
			if (d.Topic == "" || in.ContentType == d.Topic) && infoMatches(in, scenario.InfoExpect{Fields: d.Fields}) {
				n++
			}
		}
		lo, hi := d.Bounds()
		switch {
		case n < lo:
			out = append(out, fmt.Sprintf("LiveKit room received %d %s, want at least %d (received %s)", n, d, lo, infoList(infos)))
		case hi >= 0 && n > hi:
			out = append(out, fmt.Sprintf("LiveKit room received %d %s, want at most %d", n, d, hi))
		}
	}
	return out
}

func end(name string, calls []uas.Call, e scenario.EndExpect) []string {
	var out []string
	want := 1
	if e.Calls != nil {
		want = *e.Calls
	}
	if len(calls) != want {
		out = append(out, fmt.Sprintf("%s saw %d legs, want %d", name, len(calls), want))
	}
	if want == 0 || len(calls) == 0 {
		return out
	}
	c := calls[len(calls)-1]
	for k, v := range e.Headers {
		got, ok := lookup(c.Headers, k)
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s did not receive header %s", name, k))
		case v != "" && got != v:
			out = append(out, fmt.Sprintf("%s header %s = %q, want %q", name, k, got, v))
		}
	}
	out = append(out, tones(name, c.TonesHeard, e.TonesHeard, e.TonesNotHeard)...)
	out = append(out, gaps(name, c.AudioGapsMs, e.TonesHeard, e.MaxAudioGapMs)...)
	if e.Digits != nil && c.Digits != *e.Digits {
		out = append(out, fmt.Sprintf("%s digits %q, want %q", name, c.Digits, *e.Digits))
	}
	if e.Codec != "" && !strings.EqualFold(c.Codec, e.Codec) {
		out = append(out, fmt.Sprintf("%s codec %s, want %s", name, c.Codec, e.Codec))
	}
	if e.Answered != nil && c.Answered != *e.Answered {
		out = append(out, fmt.Sprintf("%s answered=%v, want %v", name, c.Answered, *e.Answered))
	}
	if e.EndedBy != "" && c.EndedBy != e.EndedBy {
		out = append(out, fmt.Sprintf("%s ended by %q, want %q", name, c.EndedBy, e.EndedBy))
	}
	if e.ReferAccepted != nil {
		got := c.Refer != nil && c.Refer.Accepted
		if got != *e.ReferAccepted {
			detail := ""
			if c.Refer != nil && c.Refer.Error != "" {
				detail = ": " + c.Refer.Error
			}
			out = append(out, fmt.Sprintf("%s REFER accepted=%v, want %v%s", name, got, *e.ReferAccepted, detail))
		}
	}
	out = append(out, infos(name, c.Infos, e.Infos)...)
	for _, err := range c.Errors {
		out = append(out, fmt.Sprintf("%s error: %s", name, err))
	}
	return out
}

// infos checks the INFO requests an end received against each matcher's bounds.
func infos(name string, got []uas.Info, want []scenario.InfoExpect) []string {
	var out []string
	for _, x := range want {
		n := 0
		for _, in := range got {
			if infoMatches(in, x) {
				n++
			}
		}
		lo, hi := x.Bounds()
		switch {
		case n < lo:
			out = append(out, fmt.Sprintf("%s received %d %s, want at least %d (received %s)", name, n, x, lo, infoList(got)))
		case hi >= 0 && n > hi:
			out = append(out, fmt.Sprintf("%s received %d %s, want at most %d", name, n, x, hi))
		}
	}
	return out
}

func infoMatches(in uas.Info, x scenario.InfoExpect) bool {
	if x.ContentType != "" && !strings.EqualFold(in.ContentType, x.ContentType) {
		return false
	}
	if len(x.Fields) == 0 {
		return true
	}
	var body map[string]any
	if json.Unmarshal([]byte(in.Body), &body) != nil {
		return false
	}
	for k, want := range x.Fields {
		v, ok := body[k]
		if !ok || fmt.Sprint(v) != want {
			return false
		}
	}
	return true
}

// infoList summarises INFOs for a message: each one's type and body.
func infoList(got []uas.Info) string {
	if len(got) == 0 {
		return "none"
	}
	parts := make([]string, len(got))
	for i, in := range got {
		parts[i] = in.ContentType + " " + in.Body
	}
	return strings.Join(parts, "; ")
}

func tones(name string, heard, want, notWant []float64) []string {
	var out []string
	for _, hz := range want {
		if !slices.Contains(heard, hz) {
			out = append(out, fmt.Sprintf("%s did not hear %g Hz (heard %v)", name, hz, heard))
		}
	}
	for _, hz := range notWant {
		if slices.Contains(heard, hz) {
			out = append(out, fmt.Sprintf("%s heard %g Hz, which it should not have", name, hz))
		}
	}
	return out
}

// gaps checks no tone in want went missing for longer than maxMs (0: not checked).
func gaps(name string, gapsMs map[string]int, want []float64, maxMs int) []string {
	if maxMs <= 0 {
		return nil
	}
	var out []string
	for _, hz := range want {
		key := strconv.FormatFloat(hz, 'f', -1, 64)
		if g, ok := gapsMs[key]; ok && g > maxMs {
			out = append(out, fmt.Sprintf("%s lost %g Hz for %d ms, more than %d ms", name, hz, g, maxMs))
		}
	}
	return out
}

// measures checks the caller's media and timing measurements.
func measures(e scenario.Expect, cr CallResult) []string {
	var out []string
	if e.MinToneCoverage > 0 {
		for _, hz := range e.TonesHeard {
			key := strconv.FormatFloat(hz, 'f', -1, 64)
			if c, ok := cr.ToneCoverage[key]; ok && c < e.MinToneCoverage {
				out = append(out, fmt.Sprintf("caller heard %g Hz in %.1f%% of its frames, less than %.1f%%",
					hz, c*100, e.MinToneCoverage*100))
			}
		}
	}
	if b := e.RTPLossPct; b != nil {
		switch {
		case cr.RTP == nil || cr.RTP.Packets == 0:
			out = append(out, "caller received no RTP to measure loss on")
		case !b.Holds(cr.RTP.LossPct):
			out = append(out, fmt.Sprintf("caller measured %.2f%% RTP loss (%d of %d), want %s%%",
				cr.RTP.LossPct, cr.RTP.Lost, cr.RTP.Expected, b))
		}
	}
	if e.MaxSetupMs > 0 && cr.SetupMs > int64(e.MaxSetupMs) {
		out = append(out, fmt.Sprintf("setup took %d ms, more than %d ms", cr.SetupMs, e.MaxSetupMs))
	}
	if b := e.EchoMs; b != nil {
		if len(cr.EchoMs) == 0 {
			out = append(out, "caller heard none of the tones it played come back")
		}
		for hz, ms := range cr.EchoMs {
			if !b.Holds(float64(ms)) {
				out = append(out, fmt.Sprintf("caller heard its %s Hz back after %d ms, want %s ms", hz, ms, b))
			}
		}
	}
	if b := e.DurationMs; b != nil && cr.Status == 200 && !b.Holds(float64(cr.DurationMs)) {
		out = append(out, fmt.Sprintf("call was up %d ms, want %s ms", cr.DurationMs, b))
	}
	return out
}

// sipChecks checks the messages the caller's socket saw.
func sipChecks(want []scenario.SIPExpect, trace []netem.TraceEntry) []string {
	var out []string
	for _, x := range want {
		dir, msg := x.Message()
		sel, err := netem.ParseSelector(msg)
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		label := dir + " " + msg
		var copies []netem.TraceEntry
		for _, t := range trace {
			if t.Dir == dir && sel.Matches(summaryOf(t)) {
				copies = append(copies, t)
			}
		}
		n := len(copies)
		if x.Min != nil && n < *x.Min {
			out = append(out, fmt.Sprintf("sip: %d of %s crossed, want at least %d", n, label, *x.Min))
		}
		if x.Max != nil && n > *x.Max {
			out = append(out, fmt.Sprintf("sip: %d of %s crossed, want at most %d", n, label, *x.Max))
		}
		if n == 0 {
			continue
		}
		first := copies[0]
		if x.FirstMs != nil && !x.FirstMs.Holds(float64(first.Ms)) {
			out = append(out, fmt.Sprintf("sip: first %s at %d ms, want %s ms", label, first.Ms, x.FirstMs))
		}
		if x.DeliveredWithinMs > 0 {
			i := slices.IndexFunc(copies, func(t netem.TraceEntry) bool { return t.Impaired != "dropped" })
			switch {
			case i < 0:
				out = append(out, fmt.Sprintf("sip: no %s got through", label))
			case copies[i].Ms-first.Ms > int64(x.DeliveredWithinMs):
				out = append(out, fmt.Sprintf("sip: the first %s to get through came %d ms after the first copy, want at most %d",
					label, copies[i].Ms-first.Ms, x.DeliveredWithinMs))
			}
		}
		if x.OneToTag {
			for _, c := range copies[1:] {
				if c.ToTag != first.ToTag {
					out = append(out, fmt.Sprintf("sip: %s copies carry To tags %q and %q", label, first.ToTag, c.ToTag))
					break
				}
			}
		}
	}
	return out
}

// summaryOf turns a trace entry's name back into a summary, for matching.
func summaryOf(t netem.TraceEntry) netem.Summary {
	code, method, ok := strings.Cut(t.Msg, " ")
	if ok {
		if n, err := strconv.Atoi(code); err == nil {
			return netem.Summary{Code: n, CSeqMethod: method, ToTag: t.ToTag}
		}
	}
	return netem.Summary{Method: t.Msg, ToTag: t.ToTag}
}

// groupChecks checks what holds across the copies of a repeated call.
func groupChecks(calls []scenario.Call, results []CallResult) []string {
	setups := map[string][]int64{}
	want := map[string]int{}
	var order []string
	for i, c := range calls {
		if c.Group == "" || c.Repeat == nil || c.Repeat.SetupP95Ms == 0 {
			continue
		}
		if _, ok := setups[c.Group]; !ok {
			order = append(order, c.Group)
		}
		setups[c.Group] = append(setups[c.Group], results[i].SetupMs)
		want[c.Group] = c.Repeat.SetupP95Ms
	}
	var out []string
	for _, g := range order {
		p95 := percentile(setups[g], 0.95)
		if p95 > int64(want[g]) {
			out = append(out, fmt.Sprintf("call %s: setup time p95 %d ms over %d copies, more than %d ms",
				g, p95, len(setups[g]), want[g]))
		}
	}
	return out
}

// percentile is the nearest-rank percentile p (0-1) of xs.
func percentile(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(rank, 0)]
}

func lookup(h map[string]string, k string) (string, bool) {
	for name, v := range h {
		if strings.EqualFold(name, k) {
			return v, true
		}
	}
	return "", false
}
