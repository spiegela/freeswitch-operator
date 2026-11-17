// Package scenario defines bench scenarios: the calls the runner places into the edge, what the backend and any
// transfer target should do, and what each end must have seen afterwards.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// such as "codec:g722".
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
	// TimeoutMs bounds the whole scenario. Default 60000.
	TimeoutMs int `yaml:"timeoutMs,omitempty"`

	// File is where the scenario was loaded from.
	File string `yaml:"-"`
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
	// StartMs delays this call from the start of the scenario, so calls can overlap.
	StartMs int `yaml:"startMs,omitempty"`
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
	Digits     *string           `yaml:"digits,omitempty"`
	Codec      string            `yaml:"codec,omitempty"`
	Answered   *bool             `yaml:"answered,omitempty"`
	EndedBy    string            `yaml:"endedBy,omitempty"`
	// ReferAccepted checks the REFER this end sent was accepted (202).
	ReferAccepted *bool `yaml:"referAccepted,omitempty"`
}

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
	sort.Strings(matches)
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
	if len(s.Calls) == 0 && s.Options == nil {
		return fmt.Errorf("scenario %q has no calls and no options check", s.Name)
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
