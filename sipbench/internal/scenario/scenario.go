// Package scenario defines bench scenarios: the calls the runner places into the edge, what the backend and any
// transfer target should do, and what each end must have seen afterwards.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/script"
)

// Scenario is one YAML file under scenarios/.
type Scenario struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Area groups scenarios in reports, e.g. "basic", "failures", "transfers".
	Area string `yaml:"area,omitempty"`
	// Requires lists capabilities the run must provide, or the scenario is skipped: a backend kind ("mock",
	// "livekit-sip", "runtime"), "esl" (the runner can reach FreeSWITCH's event socket), or an edge feature
	// such as "codec:g722", "fleet" (the runner can act on the FreeSWITCH fleet; see Actions), or "proxy" (the
	// edge address is Kamailio, not a FreeSWITCH node).
	Requires []string `yaml:"requires,omitempty"`
	// Setup and Teardown are FreeSWITCH API commands run over the event socket around the scenario.
	Setup    []string `yaml:"setup,omitempty"`
	Teardown []string `yaml:"teardown,omitempty"`
	// Backend routes are added to the mock backend for this scenario (merged over its defaults).
	Backend *Backend `yaml:"backend,omitempty"`
	// Target is how the runner's transfer target answers. Defaults to answering and playing 1800 Hz.
	Target *Target `yaml:"target,omitempty"`
	// Options sends an OPTIONS request to the edge and checks the response code.
	Options *Options `yaml:"options,omitempty"`
	Calls   []Call   `yaml:"calls,omitempty"`
	// Raw messages are sent to the edge byte for byte, one exchange after another, before the calls start.
	Raw []Raw `yaml:"raw,omitempty"`
	// Actions run one after another while the calls are up: the scenario's timeline.
	Actions []Action `yaml:"actions,omitempty"`
	// Metrics are counters the edge should have moved by the end of the scenario, read from the metrics backend
	// the runner queries (-prometheus). Without one they aren't checked, and the result says so.
	Metrics []MetricCheck `yaml:"metrics,omitempty"`
	// TimeoutMs bounds the whole scenario. Default 60000.
	TimeoutMs int `yaml:"timeoutMs,omitempty"`

	// File is where the scenario was loaded from.
	File string `yaml:"-"`
}

// MetricCheck is one counter checked through the metrics backend: its value must rise by at least Increase from
// before the scenario to after it.
type MetricCheck struct {
	// Query is PromQL giving one number, such as sum(kamailio_rejected_requests_total{reason="uri_scheme"}). No
	// result counts as 0, so a series that first appears during the scenario counts from zero.
	Query string `yaml:"query"`
	// Increase is the least the value must rise by. The runner waits for the baseline to settle before the
	// scenario and for the rise after it, so a scenario run just before can't count toward it.
	Increase float64 `yaml:"increase"`
}

// Backend configures the mock backend for a scenario.
type Backend struct {
	Routes map[string]Route `yaml:"routes"`
}

// Route is a mock backend route.
type Route struct {
	Steps []script.Step `yaml:"steps" json:"steps"`
}

// Target configures the runner's transfer target.
type Target struct {
	Steps    []script.Step `yaml:"steps"`
	DetectHz []float64     `yaml:"detectHz,omitempty"`
}

// Options is an OPTIONS check.
type Options struct {
	ExpectStatus int `yaml:"expectStatus"`
}

// Call is one call the runner places, as a far end would.
type Call struct {
	ID string `yaml:"id"`
	// To is the user dialled at the edge.
	To string `yaml:"to"`
	// From is the calling user. Default "sipbench".
	From string `yaml:"from,omitempty"`
	// StartMs delays this call from the start of the scenario, so calls can overlap; with StartAfter, from the
	// end of that action.
	StartMs int `yaml:"startMs,omitempty"`
	// StartAfter holds the call until the named action has finished.
	StartAfter string `yaml:"startAfter,omitempty"`
	// Codecs offered, in order: pcmu, pcma, g722 (g722 needs runner support; scenarios using it should
	// require codec:g722). Default pcmu then pcma. telephone-event is always offered.
	Codecs  []string          `yaml:"codecs,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	// CancelAfterMs sends CANCEL if the call isn't answered within this time.
	CancelAfterMs int `yaml:"cancelAfterMs,omitempty"`
	// Steps run after the call is answered: tone, digits, wait, echo, hangup.
	Steps []script.Step `yaml:"steps,omitempty"`
	// DetectHz are the frequencies the caller listens for. Default: every frequency named in expectations,
	// plus the bench's standard tones.
	DetectHz []float64 `yaml:"detectHz,omitempty"`
	// MaxDurationMs hangs the call up if it is still up this long after answer. Default 30000.
	MaxDurationMs int    `yaml:"maxDurationMs,omitempty"`
	Expect        Expect `yaml:"expect"`
}

// Expect is what must be true of a call afterwards.
type Expect struct {
	// Status is the final response code to the INVITE: 200, 486, 487 (cancelled), 503...
	Status int `yaml:"status"`
	// ResponseHeaders must be on the final response. An empty value means present with any value.
	ResponseHeaders map[string]string `yaml:"responseHeaders,omitempty"`
	TonesHeard      []float64         `yaml:"tonesHeard,omitempty"`
	TonesNotHeard   []float64         `yaml:"tonesNotHeard,omitempty"`
	// MaxAudioGapMs bounds how long any tone in tonesHeard may go missing once it has started: with a far end
	// playing it throughout, "two-way audio throughout" rather than "some audio at some point".
	MaxAudioGapMs int `yaml:"maxAudioGapMs,omitempty"`
	// Digits received from the far side, in order.
	Digits *string `yaml:"digits,omitempty"`
	// EndedBy is "remote" (the edge hung up) or "local" (the runner did).
	EndedBy string `yaml:"endedBy,omitempty"`
	// Codec negotiated with the edge: PCMU or PCMA.
	Codec string `yaml:"codec,omitempty"`
	// Backend checks what the mock backend recorded for this call.
	Backend *EndExpect `yaml:"backend,omitempty"`
	// Target checks what the transfer target recorded for this call.
	Target *EndExpect `yaml:"target,omitempty"`
}

// EndExpect is what the backend or a transfer target must have seen of a call.
type EndExpect struct {
	// Calls is how many legs reached this end for the call. Default 1; 0 means none must have.
	Calls *int `yaml:"calls,omitempty"`
	// Headers that must have arrived. An empty value means present with any value.
	Headers    map[string]string `yaml:"headers,omitempty"`
	TonesHeard []float64         `yaml:"tonesHeard,omitempty"`
	// MaxAudioGapMs bounds how long any tone in tonesHeard may go missing at this end (see Expect).
	MaxAudioGapMs int     `yaml:"maxAudioGapMs,omitempty"`
	Digits        *string `yaml:"digits,omitempty"`
	Codec         string  `yaml:"codec,omitempty"`
	Answered      *bool   `yaml:"answered,omitempty"`
	EndedBy       string  `yaml:"endedBy,omitempty"`
	// ReferAccepted checks the REFER this end sent was accepted (202).
	ReferAccepted *bool `yaml:"referAccepted,omitempty"`
	// OtherNodeThan names another call: this end must have received this call from a different edge node (INVITE
	// source address) than that one. Backend only.
	OtherNodeThan string `yaml:"otherNodeThan,omitempty"`
}

// Raw is one hand-written message sent to the edge, and what must come back.
type Raw struct {
	ID string `yaml:"id"`
	// Transport is udp (default) or tcp.
	Transport string `yaml:"transport,omitempty"`
	// Message is the message as sent, after placeholders are filled in: {{edge}} (the edge's host:port as the
	// runner reaches it), {{local}} / {{localHost}} / {{localPort}} (the runner's end of the connection),
	// {{transport}} (UDP or TCP, for Via), {{branch}} (a fresh z9hG4bK branch), {{callId}}, {{tag}}, {{bench}}
	// (an X-Sipbench-Call value, so the mock files the call under this exchange) and {{len}} (the body's length).
	// Line ends become CRLF unless keepLineEnds is set.
	Message      string `yaml:"message"`
	KeepLineEnds bool   `yaml:"keepLineEnds,omitempty"`
	// WaitMs is how long to wait for a final response, or for nothing to come (default 3000). Over UDP the
	// message is resent after 500 ms, 1 s and 2 s while nothing has come back.
	WaitMs int `yaml:"waitMs,omitempty"`
	// HangupAfterMs: an INVITE answered 2xx is acknowledged, then hung up with BYE this much later (default 500).
	HangupAfterMs int       `yaml:"hangupAfterMs,omitempty"`
	Expect        RawExpect `yaml:"expect"`
}

// RawExpect is what must come back for a raw message. Give status, noResponse, or closed.
type RawExpect struct {
	// Status is the final response's code.
	Status int `yaml:"status,omitempty"`
	// NoResponse: nothing at all may come back within waitMs.
	NoResponse bool `yaml:"noResponse,omitempty"`
	// Closed: the edge closes the TCP connection within waitMs (after any response).
	Closed bool `yaml:"closed,omitempty"`
	// Provisional codes that must all arrive before the final response.
	Provisional []int `yaml:"provisional,omitempty"`
	// WithinMs bounds when the first response may arrive, from sending.
	WithinMs int `yaml:"withinMs,omitempty"`
	// Headers must be on the final response; an empty value means present with any value, otherwise the value
	// must contain it (case-insensitively).
	Headers map[string]string `yaml:"headers,omitempty"`
	// Copied names headers the first response must carry exactly as the request did (Via, From, To...).
	Copied []string `yaml:"copied,omitempty"`
	// Backend checks what the mock recorded under {{bench}}: calls: 0 for a message that must go no further.
	Backend *EndExpect `yaml:"backend,omitempty"`
}

// Action is one step of a scenario's timeline. It waits for its triggers, in the order listed (afterAnswered,
// afterEnded, afterMs), then does exactly one thing. Actions on the fleet need the "fleet" capability; the fleet is
// the FreeswitchCluster the runner was pointed at.
type Action struct {
	// Name lets calls start after this action (Call.StartAfter).
	Name string `yaml:"name,omitempty"`
	// AfterAnswered waits until the named call is answered.
	AfterAnswered string `yaml:"afterAnswered,omitempty"`
	// AfterEnded waits until the named call has ended.
	AfterEnded string `yaml:"afterEnded,omitempty"`
	// AfterMs waits this long (after the other triggers, or after the previous action).
	AfterMs int `yaml:"afterMs,omitempty"`

	// DrainNodeOf drains the edge node that carries the named call, as kubectl drain would: it cordons the node
	// and evicts the call's FreeSWITCH pod. The node is uncordoned when the scenario ends, if Uncordon hasn't.
	DrainNodeOf string `yaml:"drainNodeOf,omitempty"`
	// Uncordon uncordons every node this scenario cordoned.
	Uncordon *Empty `yaml:"uncordon,omitempty"`
	// RollFleet changes the fleet's pod template (an environment variable), so the operator rolls every pod.
	RollFleet *Empty `yaml:"rollFleet,omitempty"`
	// BreakConfig edits a ConfigMap the fleet reads. It is put back when the scenario ends, if RestoreConfig
	// hasn't.
	BreakConfig *BreakConfig `yaml:"breakConfig,omitempty"`
	// RestoreConfig puts back every ConfigMap BreakConfig edited.
	RestoreConfig *Empty `yaml:"restoreConfig,omitempty"`
	// WaitMember waits until the pod that carries a call is in a given state.
	WaitMember *WaitMember `yaml:"waitMember,omitempty"`
	// WaitFleetCurrent waits until every pod runs the latest template and takes calls.
	WaitFleetCurrent *Wait `yaml:"waitFleetCurrent,omitempty"`
	// ExpectRollStuck waits for a roll to replace its first pod, then checks it goes no further: no second pod
	// is replaced and the rest of the fleet keeps taking calls.
	ExpectRollStuck *RollStuck `yaml:"expectRollStuck,omitempty"`
	// Hangup ends the named call from the runner's side.
	Hangup string `yaml:"hangup,omitempty"`
}

// Empty is an action with no settings: write it as {}.
type Empty struct{}

// BreakConfig replaces text in one key of a ConfigMap.
type BreakConfig struct {
	ConfigMap string `yaml:"configMap"`
	Key       string `yaml:"key"`
	Replace   string `yaml:"replace"`
	With      string `yaml:"with"`
}

// WaitMember waits for the pod that carries call Of to reach State: Active, Draining, Starting, or Gone (no
// longer a member).
type WaitMember struct {
	Of        string `yaml:"of"`
	State     string `yaml:"state"`
	TimeoutMs int    `yaml:"timeoutMs,omitempty"`
}

// Wait bounds a wait. Default 300000 ms.
type Wait struct {
	TimeoutMs int `yaml:"timeoutMs,omitempty"`
}

// RollStuck is ExpectRollStuck's settings.
type RollStuck struct {
	// ForMs is how long the roll must stay stuck once its first pod is replaced.
	ForMs int `yaml:"forMs"`
	// TimeoutMs bounds the wait for the first pod to be replaced. Default 120000.
	TimeoutMs int `yaml:"timeoutMs,omitempty"`
}

// Kind names what the action does.
func (a *Action) Kind() string {
	var kinds []string
	add := func(set bool, name string) {
		if set {
			kinds = append(kinds, name)
		}
	}
	add(a.DrainNodeOf != "", "drainNodeOf")
	add(a.Uncordon != nil, "uncordon")
	add(a.RollFleet != nil, "rollFleet")
	add(a.BreakConfig != nil, "breakConfig")
	add(a.RestoreConfig != nil, "restoreConfig")
	add(a.WaitMember != nil, "waitMember")
	add(a.WaitFleetCurrent != nil, "waitFleetCurrent")
	add(a.ExpectRollStuck != nil, "expectRollStuck")
	add(a.Hangup != "", "hangup")
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// OnFleet reports whether the action needs the fleet capability.
func (a *Action) OnFleet() bool { return a.Kind() != "hangup" }

// Load reads one scenario file.
func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.File = path
	return s, nil
}

// Parse reads a scenario from YAML and validates it.
func Parse(b []byte) (*Scenario, error) {
	var s Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadDir loads every *.yaml file in dir, sorted by file name.
func LoadDir(dir string) ([]*Scenario, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	slices.Sort(matches)
	var out []*Scenario
	for _, m := range matches {
		s, err := Load(m)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Validate checks the scenario is complete and consistent.
func (s *Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(s.Calls) == 0 && s.Options == nil && len(s.Raw) == 0 {
		return fmt.Errorf("scenario %q has no calls, raw messages or options check", s.Name)
	}
	if err := s.validateRaw(); err != nil {
		return err
	}
	if err := s.validateMetrics(); err != nil {
		return err
	}
	ids := map[string]bool{}
	for i, c := range s.Calls {
		if c.ID == "" {
			return fmt.Errorf("call %d: id is required", i+1)
		}
		if ids[c.ID] {
			return fmt.Errorf("call %q: duplicate id", c.ID)
		}
		ids[c.ID] = true
		if c.To == "" {
			return fmt.Errorf("call %q: to is required", c.ID)
		}
		if c.Expect.Status == 0 {
			return fmt.Errorf("call %q: expect.status is required", c.ID)
		}
		for _, codec := range c.Codecs {
			switch strings.ToLower(codec) {
			case "pcmu", "pcma", "g722":
			default:
				return fmt.Errorf("call %q: unknown codec %q (pcmu, pcma, g722)", c.ID, codec)
			}
		}
		if err := script.Validate(c.Steps, false); err != nil {
			return fmt.Errorf("call %q: %w", c.ID, err)
		}
		switch c.Expect.EndedBy {
		case "", "local", "remote":
		default:
			return fmt.Errorf("call %q: expect.endedBy must be local or remote", c.ID)
		}
		if c.Expect.Target != nil && c.Expect.Target.OtherNodeThan != "" {
			return fmt.Errorf("call %q: otherNodeThan is for the backend only", c.ID)
		}
	}
	for _, c := range s.Calls {
		if b := c.Expect.Backend; b != nil && b.OtherNodeThan != "" && (!ids[b.OtherNodeThan] || b.OtherNodeThan == c.ID) {
			return fmt.Errorf("call %q: otherNodeThan names no other call %q", c.ID, b.OtherNodeThan)
		}
	}
	if err := s.validateActions(ids); err != nil {
		return err
	}
	if s.Backend != nil {
		for name, r := range s.Backend.Routes {
			if len(r.Steps) == 0 {
				return fmt.Errorf("backend route %q has no steps", name)
			}
			if err := script.Validate(r.Steps, true); err != nil {
				return fmt.Errorf("backend route %q: %w", name, err)
			}
		}
	}
	if s.Target != nil {
		if err := script.Validate(s.Target.Steps, true); err != nil {
			return fmt.Errorf("target: %w", err)
		}
	}
	return nil
}

func (s *Scenario) validateMetrics() error {
	for i, m := range s.Metrics {
		if strings.TrimSpace(m.Query) == "" {
			return fmt.Errorf("metrics %d: query is required", i+1)
		}
		if m.Increase <= 0 {
			return fmt.Errorf("metrics %d: increase must be above 0", i+1)
		}
	}
	return nil
}

func (s *Scenario) validateRaw() error {
	ids := map[string]bool{}
	for i, x := range s.Raw {
		if x.ID == "" {
			return fmt.Errorf("raw %d: id is required", i+1)
		}
		if ids[x.ID] {
			return fmt.Errorf("raw %q: duplicate id", x.ID)
		}
		ids[x.ID] = true
		if strings.TrimSpace(x.Message) == "" {
			return fmt.Errorf("raw %q: message is required", x.ID)
		}
		switch x.Transport {
		case "", "udp", "tcp":
		default:
			return fmt.Errorf("raw %q: transport must be udp or tcp", x.ID)
		}
		e := x.Expect
		if e.Status == 0 && !e.NoResponse && !e.Closed {
			return fmt.Errorf("raw %q: expect needs status, noResponse or closed", x.ID)
		}
		if e.NoResponse && (e.Status != 0 || len(e.Provisional) > 0 || len(e.Headers) > 0 || len(e.Copied) > 0) {
			return fmt.Errorf("raw %q: noResponse can't be combined with expectations about a response", x.ID)
		}
		if e.Closed && x.Transport != "tcp" {
			return fmt.Errorf("raw %q: closed is for tcp", x.ID)
		}
		if e.Backend != nil && e.Backend.OtherNodeThan != "" {
			return fmt.Errorf("raw %q: otherNodeThan is for calls", x.ID)
		}
	}
	return nil
}

func (s *Scenario) validateActions(calls map[string]bool) error {
	names := map[string]bool{}
	fleet := false
	for i, a := range s.Actions {
		label := fmt.Sprintf("action %d", i+1)
		if a.Name != "" {
			label = fmt.Sprintf("action %q", a.Name)
			if names[a.Name] {
				return fmt.Errorf("%s: duplicate name", label)
			}
			names[a.Name] = true
		}
		kind := a.Kind()
		if kind == "" {
			return fmt.Errorf("%s: needs exactly one of drainNodeOf, uncordon, rollFleet, breakConfig, "+
				"restoreConfig, waitMember, waitFleetCurrent, expectRollStuck, hangup", label)
		}
		fleet = fleet || a.OnFleet()
		for field, call := range map[string]string{
			"afterAnswered": a.AfterAnswered, "afterEnded": a.AfterEnded, "drainNodeOf": a.DrainNodeOf, "hangup": a.Hangup,
		} {
			if call != "" && !calls[call] {
				return fmt.Errorf("%s: %s names no call %q", label, field, call)
			}
		}
		if a.AfterMs < 0 {
			return fmt.Errorf("%s: afterMs must not be negative", label)
		}
		switch {
		case a.BreakConfig != nil:
			b := a.BreakConfig
			if b.ConfigMap == "" || b.Key == "" || b.Replace == "" || b.Replace == b.With {
				return fmt.Errorf("%s: breakConfig needs configMap, key, and replace different from with", label)
			}
		case a.WaitMember != nil:
			if !calls[a.WaitMember.Of] {
				return fmt.Errorf("%s: waitMember.of names no call %q", label, a.WaitMember.Of)
			}
			switch a.WaitMember.State {
			case "Active", "Draining", "Starting", "Gone":
			default:
				return fmt.Errorf("%s: waitMember.state must be Active, Draining, Starting or Gone", label)
			}
		case a.ExpectRollStuck != nil:
			if a.ExpectRollStuck.ForMs <= 0 {
				return fmt.Errorf("%s: expectRollStuck.forMs is required", label)
			}
		}
	}
	for _, c := range s.Calls {
		if c.StartAfter != "" && !names[c.StartAfter] {
			return fmt.Errorf("call %q: startAfter names no action %q", c.ID, c.StartAfter)
		}
	}
	if fleet && !slices.Contains(s.Requires, "fleet") {
		return fmt.Errorf("scenario %q acts on the fleet, so it must require fleet", s.Name)
	}
	return nil
}

// NeedsFleet reports whether any action acts on the fleet.
func (s *Scenario) NeedsFleet() bool {
	for i := range s.Actions {
		if s.Actions[i].OnFleet() {
			return true
		}
	}
	return false
}

// Missing returns the capabilities s requires that caps lacks.
func (s *Scenario) Missing(caps map[string]bool) []string {
	var out []string
	for _, r := range s.Requires {
		if !caps[r] {
			out = append(out, r)
		}
	}
	return out
}
