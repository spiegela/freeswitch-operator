// Package runner runs bench scenarios: it places calls into the edge as a carrier or PBX would, answers
// transfers as their target, and checks what every end saw.
package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"gopkg.in/yaml.v3"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/esl"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/fleet"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/script"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/uas"
)

// Result statuses, as they appear in reports.
const (
	statusPass = "pass"
	statusFail = "fail"
	statusSkip = "skip"
)

// transportUDP is the SIP transport the bench uses.
const transportUDP = "udp"

// Standard bench tones. Each end plays a frequency only it uses, so hearing it proves the path from that end.
const (
	CallerHz   = 600.0  // the far end (runner caller)
	BackendHz  = 1000.0 // the backend (mock)
	FallbackHz = 1400.0 // the edge's fallback
	TargetHz   = 1800.0 // a transfer target
)

// StandardTones are always listened for.
var StandardTones = []float64{CallerHz, BackendHz, FallbackHz, TargetHz}

// Config says where the edge and its neighbours are.
type Config struct {
	// Edge is the far-end-facing SIP address of the edge, host:port.
	Edge string
	// MockURL is the mock backend's control API, e.g. http://mockbackend:8080. Empty when the backend isn't
	// the mock, in which case backend checks are skipped.
	MockURL string
	// ESL is FreeSWITCH's event socket, host:port, and its password. Empty disables setup/teardown commands.
	ESL         string
	ESLPassword string
	// TargetListen is where the runner answers transfers, host:port. The edge must route transfers here.
	TargetListen string
	// CallerBindHost is the address calls are placed from. Default 0.0.0.0 (diago advertises the host's IP).
	CallerBindHost string
	// Caps are the capabilities this run provides; scenarios requiring others are skipped.
	Caps map[string]bool
	// Fleet is the FreeSWITCH fleet scenario actions act on; with it, add "fleet" to Caps.
	Fleet fleet.Fleet
	Log   *slog.Logger
}

// Runner runs scenarios. Create one with New and call Start before Run. Run may be called concurrently: each call
// is its own run, with its own mock routes, transfer target and records (see uas.RunOf), though scenarios that
// change the edge itself (setup commands, fleet actions) will still disturb each other.
type Runner struct {
	cfg   Config
	log   *slog.Logger
	http  *http.Client
	runID string
	runs  atomic.Int64
	// targets holds each run's transfer target script, by run key.
	targets sync.Map
	tstore  *uas.Store
	seq     atomic.Int64
}

// New returns a runner for cfg.
func New(cfg Config) *Runner {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		cfg:    cfg,
		log:    log,
		http:   &http.Client{Timeout: 10 * time.Second},
		runID:  newRunID(),
		tstore: &uas.Store{},
	}
}

// newRunID returns 8 random hex digits: unlikely to meet another runner's on the same mock.
func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// DefaultTarget answers and plays the target tone until the caller hangs up.
var DefaultTarget = scenario.Target{Steps: []script.Step{
	{Answer: &script.Answer{}},
	{Tone: &script.Tone{Hz: TargetHz, Ms: 3000}},
	{Wait: &script.Wait{Ms: 30000}},
}}

// Start begins answering transfers on cfg.TargetListen.
func (r *Runner) Start(ctx context.Context) error {
	if r.cfg.TargetListen == "" {
		return nil
	}
	host, portS, err := net.SplitHostPort(r.cfg.TargetListen)
	if err != nil {
		return fmt.Errorf("target listen address: %w", err)
	}
	port, _ := strconv.Atoi(portS)
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sipbench-target"))
	if err != nil {
		return err
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{Transport: transportUDP, BindHost: host, BindPort: port}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000}}),
		diago.WithLogger(r.log.With("end", "target")),
	)
	return dg.ServeBackground(ctx, func(d *diago.DialogServerSession) {
		t := &DefaultTarget
		if h := d.InviteRequest.GetHeader(uas.BenchCallHeader); h != nil {
			if v, ok := r.targets.Load(uas.RunOf(h.Value())); ok {
				t = v.(*scenario.Target)
			}
		}
		detect := append(append([]float64{}, StandardTones...), t.DetectHz...)
		uas.Handle(d, "target", t.Steps, r.tstore, uas.Options{DetectHz: detect, Hold: 30 * time.Second, Log: r.log.With("end", "target")})
	})
}

// Result is the outcome of one scenario.
type Result struct {
	Name     string        `json:"name"`
	File     string        `json:"file,omitempty"`
	Area     string        `json:"area,omitempty"`
	Status   string        `json:"status"` // pass, fail, skip
	Reason   string        `json:"reason,omitempty"`
	Failures []string      `json:"failures,omitempty"`
	Duration time.Duration `json:"durationNs"`
	Calls    []CallResult  `json:"calls,omitempty"`
	// Timeline is what the scenario's actions did, when, as "+12.3s action: what happened".
	Timeline []string `json:"timeline,omitempty"`
}

// CallResult is what the runner saw of one call.
type CallResult struct {
	ID              string            `json:"id"`
	BenchCall       string            `json:"benchCall"`
	Status          int               `json:"status"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
	Codec           string            `json:"codec,omitempty"`
	TonesHeard      []float64         `json:"tonesHeard"`
	AudioGapsMs     map[string]int    `json:"audioGapsMs,omitempty"`
	Digits          string            `json:"digits"`
	EndedBy         string            `json:"endedBy,omitempty"`
	MediaFrames     int64             `json:"mediaFrames"`
	Error           string            `json:"error,omitempty"`
	Backend         []uas.Call        `json:"backend,omitempty"`
	Target          []uas.Call        `json:"target,omitempty"`
}

// Run runs one scenario.
func (r *Runner) Run(ctx context.Context, s *scenario.Scenario) (res Result) {
	start := time.Now()
	res = Result{Name: s.Name, File: s.File, Area: s.Area}
	defer func() { res.Duration = time.Since(start) }()

	if missing := s.Missing(r.cfg.Caps); len(missing) > 0 {
		res.Status = statusSkip
		res.Reason = "needs " + strings.Join(missing, ", ")
		return res
	}
	timeout := 60 * time.Second
	if s.TimeoutMs > 0 {
		timeout = time.Duration(s.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	fail := func(format string, args ...any) {
		res.Failures = append(res.Failures, fmt.Sprintf(format, args...))
	}

	if s.NeedsFleet() && r.cfg.Fleet == nil {
		res.Status, res.Reason = statusFail, "the scenario acts on the fleet, and the runner has none (-fleet)"
		return res
	}

	// Prepare the ends, for this run only: its key starts every bench call id it places.
	key := fmt.Sprintf("%s.%d", r.runID, r.runs.Add(1))
	if r.cfg.MockURL != "" {
		defer r.mockForget(key)
		if s.Backend != nil && len(s.Backend.Routes) > 0 {
			if err := r.mockRoutes(ctx, key, s.Backend.Routes); err != nil {
				res.Status, res.Reason = statusFail, "configuring mock backend: "+err.Error()
				return res
			}
		}
	}
	defer r.tstore.DeleteRun(key)
	if s.Target != nil {
		t := *s.Target
		r.targets.Store(key, &t)
		defer r.targets.Delete(key)
	}
	if len(s.Setup) > 0 {
		if err := r.eslRun(s.Setup); err != nil {
			res.Status, res.Reason = statusFail, "setup: "+err.Error()
			return res
		}
	}
	if len(s.Teardown) > 0 {
		defer func() {
			if err := r.eslRun(s.Teardown); err != nil {
				fail("teardown: %v", err)
				res.Status = statusFail
			}
		}()
	}

	if s.Options != nil {
		code, err := r.options(ctx)
		switch {
		case err != nil:
			fail("OPTIONS: %v", err)
		case code != s.Options.ExpectStatus:
			fail("OPTIONS: got %d, want %d", code, s.Options.ExpectStatus)
		}
	}

	// Each call's state lets the timeline wait on it and hang it up.
	states := map[string]*callState{}
	for _, c := range s.Calls {
		states[c.ID] = newCallState(fmt.Sprintf("%s-%s-%s-%d", key, s.Name, c.ID, r.seq.Add(1)))
	}
	tl := newTimeline(r, s, key, states, start)
	defer func() {
		tl.cleanup()
		res.Timeline = tl.log
		if len(tl.failures) > 0 {
			res.Failures = append(res.Failures, tl.failures...)
			res.Status = statusFail
		}
	}()
	results := r.placeCalls(ctx, s, states, tl)

	// Give the ends a moment to finish recording after the last BYE.
	time.Sleep(500 * time.Millisecond)

	for i, c := range s.Calls {
		cr := &results[i]
		if r.cfg.MockURL != "" {
			calls, err := r.mockCalls(ctx, cr.BenchCall)
			if err != nil {
				fail("call %s: reading backend records: %v", c.ID, err)
			}
			cr.Backend = calls
		}
		cr.Target = r.tstore.Calls(cr.BenchCall)
		for _, f := range check(c, *cr, r.cfg.MockURL != "") {
			fail("call %s: %s", c.ID, f)
		}
	}
	for _, f := range crossChecks(s.Calls, results) {
		fail("%s", f)
	}
	res.Calls = results
	if len(res.Failures) > 0 {
		res.Status = statusFail
	} else {
		res.Status = statusPass
	}
	return res
}

// placeCalls places every call, each after its action and start delay, runs the timeline alongside, and waits for
// all of it.
func (r *Runner) placeCalls(ctx context.Context, s *scenario.Scenario, states map[string]*callState, tl *timeline) []CallResult {
	results := make([]CallResult, len(s.Calls))
	var wg sync.WaitGroup
	for i, c := range s.Calls {
		wg.Go(func() {
			st := states[c.ID]
			defer st.end()
			if c.StartAfter != "" {
				if err := tl.waitFor(ctx, c.StartAfter); err != nil {
					results[i] = CallResult{ID: c.ID, BenchCall: st.bench, Error: "not started: " + err.Error()}
					return
				}
			}
			if c.StartMs > 0 {
				select {
				case <-ctx.Done():
					results[i] = CallResult{ID: c.ID, BenchCall: st.bench, Error: "scenario timed out before start"}
					return
				case <-time.After(time.Duration(c.StartMs) * time.Millisecond):
				}
			}
			results[i] = r.call(ctx, s, c, st)
		})
	}
	if len(s.Actions) > 0 {
		wg.Go(func() { tl.run(ctx) })
	}
	wg.Wait()
	return results
}

// call places one call and returns what the runner saw.
func (r *Runner) call( //nolint:gocyclo // one call's linear lifecycle; splitting it scatters the steps
	ctx context.Context, s *scenario.Scenario, c scenario.Call, st *callState,
) CallResult {
	bench := st.bench
	cr := CallResult{ID: c.ID, BenchCall: bench, TonesHeard: []float64{}}
	log := r.log.With("scenario", s.Name, "call", c.ID)

	codecs := []media.Codec{}
	if len(c.Codecs) == 0 {
		codecs = append(codecs, media.CodecAudioUlaw, media.CodecAudioAlaw)
	}
	for _, name := range c.Codecs {
		switch strings.ToLower(name) {
		case "pcmu":
			codecs = append(codecs, media.CodecAudioUlaw)
		case "pcma":
			codecs = append(codecs, media.CodecAudioAlaw)
		default:
			cr.Error = "the runner cannot offer " + name + " yet"
			return cr
		}
	}
	codecs = append(codecs, media.CodecTelephoneEvent8000)

	host, portS, err := net.SplitHostPort(r.cfg.Edge)
	if err != nil {
		cr.Error = "edge address: " + err.Error()
		return cr
	}
	port, _ := strconv.Atoi(portS)

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sipbench"))
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	// diago closes the transport when callCtx ends; closing the UA as well double-closes it.
	bind := r.cfg.CallerBindHost
	if bind == "" {
		bind = "0.0.0.0"
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{Transport: transportUDP, BindHost: bind, BindPort: 0}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: codecs}),
		diago.WithLogger(log),
	)
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	if err := dg.ServeBackground(callCtx, func(d *diago.DialogServerSession) {
		_ = d.Respond(sip.StatusForbidden, "Caller does not take calls", nil)
	}); err != nil {
		cr.Error = "listen: " + err.Error()
		return cr
	}

	recipient := sip.Uri{Scheme: "sip", User: c.To, Host: host, Port: port}
	d, err := dg.NewDialog(recipient, diago.NewDialogOptions{Transport: transportUDP})
	if err != nil {
		cr.Error = "new dialog: " + err.Error()
		return cr
	}
	defer func() { _ = d.Close() }()

	opts := diago.InviteClientOptions{Headers: []sip.Header{sip.NewHeader(uas.BenchCallHeader, bench)}}
	names := make([]string, 0, len(c.Headers))
	for k := range c.Headers {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		opts.Headers = append(opts.Headers, sip.NewHeader(k, c.Headers[k]))
	}
	from := c.From
	if from == "" {
		from = "sipbench"
	}
	opts.WithCaller("", from, host)

	var final atomic.Pointer[sip.Response]
	opts.OnResponse = func(res *sip.Response) error {
		if res.StatusCode >= 200 {
			final.Store(res)
		}
		return nil
	}

	inviteCtx := callCtx
	if c.CancelAfterMs > 0 {
		var cancel context.CancelFunc
		inviteCtx, cancel = context.WithTimeout(callCtx, time.Duration(c.CancelAfterMs)*time.Millisecond)
		defer cancel()
	}
	med, err := d.Invite(inviteCtx, opts)
	if err != nil {
		var dr sipgo.ErrDialogResponse
		switch {
		case errors.As(err, &dr):
			cr.Status = dr.Res.StatusCode
			cr.ResponseHeaders = responseHeaders(dr.Res)
		case final.Load() != nil:
			cr.Status = final.Load().StatusCode
			cr.ResponseHeaders = responseHeaders(final.Load())
		case c.CancelAfterMs > 0 && errors.Is(inviteCtx.Err(), context.DeadlineExceeded):
			// We cancelled; the 487 may not have been reported back through Invite.
			cr.Status = int(sip.StatusRequestTerminated)
		default:
			cr.Error = "invite: " + err.Error()
		}
		return cr
	}
	if err := d.Ack(callCtx); err != nil {
		cr.Error = "ack: " + err.Error()
		return cr
	}
	cr.Status = 200
	if res := d.InviteResponse; res != nil {
		cr.ResponseHeaders = responseHeaders(res)
	}
	st.answer()

	detect := append(append([]float64{}, StandardTones...), c.DetectHz...)
	detect = append(detect, c.Expect.TonesHeard...)
	detect = append(detect, c.Expect.TonesNotHeard...)
	eng, err := script.Start(med, dedupe(detect))
	if err != nil {
		cr.Error = "media: " + err.Error()
		_ = d.Hangup(context.Background())
		return cr
	}

	dialogDone := d.Context().Done()
	endedLocal := false
	hangup := func() error {
		endedLocal = true
		hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return d.Hangup(hctx)
	}
	stepCtx, cancelSteps := context.WithCancel(callCtx)
	// An action may hang the call up (Action.Hangup): that ends the steps, and isn't the steps' error.
	var stopped atomic.Bool
	go func() {
		select {
		case <-dialogDone:
			cancelSteps()
		case <-st.stop:
			stopped.Store(true)
			cancelSteps()
		case <-stepCtx.Done():
		}
	}()
	for _, step := range c.Steps {
		if stepCtx.Err() != nil || endedLocal {
			break
		}
		if step.Refer != nil {
			cr.Error = "refer from the caller is not supported yet"
			break
		}
		if err := eng.RunMedia(stepCtx, step, hangup); err != nil {
			if stopped.Load() {
				break
			}
			log.Warn("step failed", "step", step.Kind(), "error", err)
			if cr.Error == "" {
				cr.Error = step.Kind() + ": " + err.Error()
			}
			break
		}
	}
	cancelSteps()
	if !endedLocal {
		max := 30 * time.Second
		if c.MaxDurationMs > 0 {
			max = time.Duration(c.MaxDurationMs) * time.Millisecond
		}
		select {
		case <-dialogDone:
		case <-st.stop:
			_ = hangup()
		case <-time.After(max):
			_ = hangup()
		case <-ctx.Done():
			_ = hangup()
		}
	}
	if endedLocal {
		cr.EndedBy = "local"
	} else {
		cr.EndedBy = "remote"
	}
	// Let the last packets drain into the detector.
	time.Sleep(100 * time.Millisecond)
	cr.Codec = eng.Codec()
	cr.TonesHeard = eng.Heard(uas.MinToneFrames)
	cr.AudioGapsMs = eng.MaxGapsMs()
	cr.Digits = eng.Digits()
	cr.MediaFrames = eng.FramesReceived()
	return cr
}

// options sends OPTIONS to the edge and returns the response code.
func (r *Runner) options(ctx context.Context) (int, error) {
	host, portS, err := net.SplitHostPort(r.cfg.Edge)
	if err != nil {
		return 0, err
	}
	port, _ := strconv.Atoi(portS)
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sipbench"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = ua.Close() }()
	client, err := sipgo.NewClient(ua)
	if err != nil {
		return 0, err
	}
	req := sip.NewRequest(sip.OPTIONS, sip.Uri{Scheme: "sip", Host: host, Port: port})
	octx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := client.Do(octx, req)
	if err != nil {
		return 0, err
	}
	return res.StatusCode, nil
}

func (r *Runner) eslRun(cmds []string) error {
	if r.cfg.ESL == "" {
		return fmt.Errorf("no event socket configured (-esl)")
	}
	c, err := esl.Dial(r.cfg.ESL, r.cfg.ESLPassword, 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	for _, cmd := range cmds {
		out, err := c.API(cmd, 5*time.Second)
		if err != nil {
			return err
		}
		r.log.Debug("esl", "cmd", cmd, "out", out)
	}
	return nil
}

// mockForget has the mock drop one run's records and routes, once the run has read them.
func (r *Runner) mockForget(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.mockDo(ctx, http.MethodDelete, "/runs/"+url.PathEscape(key), nil, nil); err != nil {
		r.log.Warn("mock backend kept a finished run's records", "run", key, "error", err)
	}
}

func (r *Runner) mockRoutes(ctx context.Context, key string, routes map[string]scenario.Route) error {
	b, err := yaml.Marshal(routes)
	if err != nil {
		return err
	}
	return r.mockDo(ctx, http.MethodPost, "/runs/"+url.PathEscape(key)+"/routes", b, nil)
}

func (r *Runner) mockCalls(ctx context.Context, benchCall string) ([]uas.Call, error) {
	var calls []uas.Call
	err := r.mockDo(ctx, http.MethodGet, "/calls?benchCall="+url.QueryEscape(benchCall), nil, &calls)
	return calls, err
}

func (r *Runner) mockDo(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(r.cfg.MockURL, "/")+path, rd)
	if err != nil {
		return err
	}
	res, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

func responseHeaders(res *sip.Response) map[string]string {
	out := map[string]string{}
	for _, h := range res.Headers() {
		if strings.HasPrefix(strings.ToLower(h.Name()), "x-") {
			out[strings.ToLower(h.Name())] = h.Value()
		}
	}
	return out
}

func dedupe(fs []float64) []float64 {
	seen := map[float64]bool{}
	var out []float64
	for _, f := range fs {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// WaitReady polls the edge with OPTIONS until it answers 200 or timeout passes.
func (r *Runner) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		code, err := r.options(ctx)
		if err == nil && code == 200 {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("OPTIONS answered %d", code)
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("edge %s not ready after %s: %v", r.cfg.Edge, timeout, last)
}
