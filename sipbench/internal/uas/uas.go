// Package uas answers bench calls by running a script, and records what each call received. The mock backend
// and the runner's transfer targets both use it.
package uas

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo/sip"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/script"
)

// BenchCallHeader carries the runner's id for a call through the edge, so each end's records can be matched
// to the call that caused them. The edge must pass X- headers through, which is itself under test.
const BenchCallHeader = "X-Sipbench-Call"

// RunOf returns the run part of a bench call id: everything before the first "-". The runner gives each scenario
// run its own, so an end can keep one run's records and routes apart from another's.
func RunOf(benchCall string) string {
	run, _, _ := strings.Cut(benchCall, "-")
	return run
}

// MinToneFrames is how many 20 ms frames a tone must be present for to count as heard (200 ms).
const MinToneFrames = 10

// Call is what one end saw of one call.
type Call struct {
	ID         string            `json:"id"`
	BenchCall  string            `json:"benchCall,omitempty"`
	Route      string            `json:"route"`
	From       string            `json:"from"`
	To         string            `json:"to"`
	RequestURI string            `json:"requestUri"`
	Headers    map[string]string `json:"headers"`

	StartedAt  time.Time  `json:"startedAt"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	// Answered is true once 200 OK was sent; Rejected is the final failure code sent, if any.
	Answered bool `json:"answered"`
	Rejected int  `json:"rejected,omitempty"`
	// EndedBy is "local" (this end hung up), "remote" (the other end did), or "" while the call is up.
	EndedBy string `json:"endedBy,omitempty"`

	// Source is where the INVITE came from (host:port): the edge node that sent the call here.
	Source string `json:"source,omitempty"`

	Codec      string    `json:"codec,omitempty"`
	TonesHeard []float64 `json:"tonesHeard"`
	// AudioGapsMs is, for each tone heard, the longest it went missing once it had started (see tone.Detector).
	AudioGapsMs map[string]int `json:"audioGapsMs,omitempty"`
	Digits      string         `json:"digits"`
	MediaFrames int64          `json:"mediaFrames"`

	Refer  *ReferResult `json:"refer,omitempty"`
	Errors []string     `json:"errors,omitempty"`
}

// ReferResult is the outcome of a REFER this end sent.
type ReferResult struct {
	To       string `json:"to"`
	Accepted bool   `json:"accepted"`
	Notify   []int  `json:"notify,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Store keeps calls in arrival order. It is safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	calls []*live
}

type live struct {
	mu   sync.Mutex
	call Call
}

func (l *live) update(f func(c *Call)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(&l.call)
}

func (l *live) snapshot() Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.call
	c.Headers = make(map[string]string, len(l.call.Headers))
	maps.Copy(c.Headers, l.call.Headers)
	c.TonesHeard = append([]float64{}, l.call.TonesHeard...)
	c.AudioGapsMs = maps.Clone(l.call.AudioGapsMs)
	c.Errors = append([]string(nil), l.call.Errors...)
	if l.call.Refer != nil {
		r := *l.call.Refer
		r.Notify = append([]int(nil), l.call.Refer.Notify...)
		c.Refer = &r
	}
	return c
}

func (s *Store) add(c Call) *live {
	l := &live{call: c}
	s.mu.Lock()
	s.calls = append(s.calls, l)
	s.mu.Unlock()
	return l
}

// Calls returns a snapshot of every call, or of those whose bench call id equals benchCall when it is set.
func (s *Store) Calls(benchCall string) []Call {
	s.mu.Lock()
	ls := append([]*live(nil), s.calls...)
	s.mu.Unlock()
	out := []Call{}
	for _, l := range ls {
		c := l.snapshot()
		if benchCall == "" || c.BenchCall == benchCall {
			out = append(out, c)
		}
	}
	return out
}

// Reset forgets every call.
func (s *Store) Reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

// DeleteRun forgets the calls of one run (see RunOf).
func (s *Store) DeleteRun(run string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.calls[:0]
	for _, l := range s.calls {
		if RunOf(l.snapshot().BenchCall) != run {
			kept = append(kept, l)
		}
	}
	clear(s.calls[len(kept):])
	s.calls = kept
}

// Options tune Handle.
type Options struct {
	// DetectHz are the tone frequencies to listen for.
	DetectHz []float64
	// Hold is how long to keep an answered call up after the script ends, waiting for the other end to hang up.
	Hold time.Duration
	Log  *slog.Logger
}

// Handle answers d by running steps, recording everything into store. It returns when the call has ended.
func Handle( //nolint:gocyclo // one dispatch over every script step; splitting it scatters the call flow
	d *diago.DialogServerSession, route string, steps []script.Step, store *Store, opts Options,
) {
	if opts.Hold <= 0 {
		opts.Hold = 60 * time.Second
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	req := d.InviteRequest
	headers := map[string]string{}
	for _, h := range req.Headers() {
		if strings.HasPrefix(strings.ToLower(h.Name()), "x-") {
			headers[canonical(h.Name())] = h.Value()
		}
	}
	l := store.add(Call{
		ID:         d.ID,
		BenchCall:  headers[BenchCallHeader],
		Route:      route,
		From:       req.From().Address.User,
		To:         req.To().Address.User,
		RequestURI: req.Recipient.String(),
		Headers:    headers,
		Source:     req.Source(),
		StartedAt:  time.Now(),
		TonesHeard: []float64{},
	})
	fail := func(err error) {
		log.Warn("call step failed", "call", d.ID, "error", err)
		l.update(func(c *Call) { c.Errors = append(c.Errors, err.Error()) })
	}

	ctx := d.Context()
	var med *diago.DialogMedia
	var eng *script.Engine
	answered, finished := false, false
	hangup := func() error {
		hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l.update(func(c *Call) { c.EndedBy = "local" })
		finished = true
		return d.Hangup(hctx)
	}
	startMedia := func() error {
		if eng != nil {
			return nil
		}
		var err error
		eng, err = script.Start(med, opts.DetectHz)
		return err
	}

	_ = d.Trying()
	for _, s := range steps {
		if ctx.Err() != nil || finished {
			break
		}
		var err error
		switch {
		case s.Ring != nil:
			if err = d.Ringing(); err != nil {
				break
			}
			if s.Ring.Ms <= 0 {
				<-ctx.Done()
			} else {
				sleep(ctx, time.Duration(s.Ring.Ms)*time.Millisecond)
			}
		case s.Reject != nil:
			reason := s.Reject.Reason
			if reason == "" {
				reason = script.Reason(s.Reject.Code)
			}
			err = d.Respond(s.Reject.Code, reason, nil)
			l.update(func(c *Call) { c.Rejected = s.Reject.Code })
			finished = true
		case s.EarlyMedia != nil:
			if med, err = d.ProgressMedia(diago.ProgressMediaOptions{}); err == nil {
				if err = startMedia(); err == nil {
					err = eng.PlayTone(ctx, s.EarlyMedia.Hz, time.Duration(s.EarlyMedia.Ms)*time.Millisecond)
				}
			}
		case s.Answer != nil:
			if med != nil {
				err = d.AnswerEarlyMedia(med, diago.AnswerOptions{})
			} else {
				med, err = d.Answer(diago.AnswerOptions{})
			}
			if err == nil {
				answered = true
				now := time.Now()
				l.update(func(c *Call) { c.Answered = true; c.AnsweredAt = &now })
				err = startMedia()
			}
		case s.Refer != nil:
			err = refer(ctx, d, s.Refer.To, l)
		default:
			if eng == nil {
				err = fmt.Errorf("%s before the call was answered", s.Kind())
				break
			}
			err = eng.RunMedia(ctx, s, hangup)
		}
		if err != nil {
			// A step cut short because the other end hung up is how calls end, not a failure. The media
			// socket can close a moment before the dialog context does, so give it a moment to catch up.
			select {
			case <-ctx.Done():
			case <-time.After(300 * time.Millisecond):
				fail(fmt.Errorf("%s: %w", s.Kind(), err))
			}
			break
		}
	}

	switch {
	case finished:
	case !answered && ctx.Err() == nil:
		// The script never answered or rejected: refuse rather than leave the caller hanging.
		_ = d.Respond(sip.StatusTemporarilyUnavailable, "Script ended without answering", nil)
		l.update(func(c *Call) { c.Rejected = int(sip.StatusTemporarilyUnavailable) })
	case answered:
		select {
		case <-ctx.Done():
		case <-time.After(opts.Hold):
			_ = hangup()
		}
	}
	if ctx.Err() != nil && !finished {
		l.update(func(c *Call) {
			if c.EndedBy == "" {
				c.EndedBy = "remote"
			}
		})
	}
	now := time.Now()
	l.update(func(c *Call) {
		c.EndedAt = &now
		if eng != nil {
			c.Codec = eng.Codec()
			c.TonesHeard = eng.Heard(MinToneFrames)
			c.AudioGapsMs = eng.MaxGapsMs()
			c.Digits = eng.Digits()
			c.MediaFrames = eng.FramesReceived()
		}
		if c.EndedBy == "" && c.Answered {
			c.EndedBy = "remote"
		}
	})
	snap := l.snapshot()
	log.Info("call ended", "call", d.ID, "route", route, "bench", snap.BenchCall, "answered", snap.Answered,
		"rejected", snap.Rejected, "endedBy", snap.EndedBy, "codec", snap.Codec, "tones", snap.TonesHeard,
		"digits", snap.Digits)
}

func refer(ctx context.Context, d *diago.DialogServerSession, to string, l *live) error {
	target := to
	if !strings.Contains(target, ":") || !strings.HasPrefix(strings.ToLower(target), "sip") {
		// A bare user: send it to where the call came from.
		c := d.RemoteContact()
		if c == nil {
			return fmt.Errorf("no remote contact to build the Refer-To from")
		}
		hp := c.Address.Host
		if c.Address.Port > 0 {
			hp = fmt.Sprintf("%s:%d", hp, c.Address.Port)
		}
		target = fmt.Sprintf("sip:%s@%s", to, hp)
	}
	var uri sip.Uri
	if err := sip.ParseUri(target, &uri); err != nil {
		return fmt.Errorf("refer target %q: %w", target, err)
	}
	res := &ReferResult{To: target}
	l.update(func(c *Call) { c.Refer = res })
	err := d.ReferOptions(ctx, uri, diago.ReferServerOptions{
		OnNotify: func(code int) {
			l.update(func(c *Call) { c.Refer.Notify = append(c.Refer.Notify, code) })
		},
	})
	l.update(func(c *Call) {
		c.Refer.Accepted = err == nil
		if err != nil {
			c.Refer.Error = err.Error()
		}
	})
	return err
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// canonical writes a header name as X-Foo-Bar, so lookups don't depend on the sender's casing.
func canonical(name string) string {
	parts := strings.Split(strings.ToLower(name), "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

// SortedHeaderNames is a helper for stable output.
func SortedHeaderNames(h map[string]string) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
