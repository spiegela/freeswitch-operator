// Package script defines the step language both ends of a bench call use, and the media engine that runs the
// audio steps on an answered call.
//
// A script is a list of steps, each a single-key map in YAML:
//
//   - ring: {ms: 500}
//   - answer: {}
//   - tone: {hz: 1000, ms: 2000}
//   - speech: {ms: 2000}
//   - digits: {digits: "123#"}
//   - wait: {ms: 1000}
//   - refer: {to: "target-1"}
//   - hangup: {}
//
// Signaling steps (ring, reject, earlyMedia, answer, refer) only make sense on the answering side and are run by
// that side; media steps (tone, speech, digits, wait, echo, hangup) run on any answered call through Engine.
package script

import (
	"fmt"
	"strings"
	"time"
)

// Step is one action. Exactly one field is set.
type Step struct {
	Ring       *Ring       `yaml:"ring,omitempty" json:"ring,omitempty"`
	Reject     *Reject     `yaml:"reject,omitempty" json:"reject,omitempty"`
	EarlyMedia *EarlyMedia `yaml:"earlyMedia,omitempty" json:"earlyMedia,omitempty"`
	Answer     *Answer     `yaml:"answer,omitempty" json:"answer,omitempty"`
	Tone       *Tone       `yaml:"tone,omitempty" json:"tone,omitempty"`
	Speech     *Speech     `yaml:"speech,omitempty" json:"speech,omitempty"`
	Digits     *Digits     `yaml:"digits,omitempty" json:"digits,omitempty"`
	Wait       *Wait       `yaml:"wait,omitempty" json:"wait,omitempty"`
	Echo       *Echo       `yaml:"echo,omitempty" json:"echo,omitempty"`
	Refer      *Refer      `yaml:"refer,omitempty" json:"refer,omitempty"`
	Hangup     *Hangup     `yaml:"hangup,omitempty" json:"hangup,omitempty"`
}

// Ring sends 180 Ringing and waits ms before the next step. ms 0 rings until the caller gives up.
type Ring struct {
	Ms int `yaml:"ms" json:"ms"`
}

// Reject ends the call with a final response code. It is the last step.
type Reject struct {
	Code   int    `yaml:"code" json:"code"`
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// EarlyMedia sends 183 Session Progress with SDP and plays a tone before answering.
type EarlyMedia struct {
	Hz float64 `yaml:"hz" json:"hz"`
	Ms int     `yaml:"ms" json:"ms"`
}

// Answer sends 200 OK.
type Answer struct{}

// Tone plays a sine tone.
type Tone struct {
	Hz float64 `yaml:"hz" json:"hz"`
	Ms int     `yaml:"ms" json:"ms"`
}

// Speech talks for Ms: synthetic speech that voice detectors take for a person (see package speech), with no
// pauses of its own; put a wait between two to pause. Seed picks the voice's syllables (default 1). Speech is no
// tone: an end that hears it reports no tone heard.
type Speech struct {
	Ms   int    `yaml:"ms" json:"ms"`
	Seed uint64 `yaml:"seed,omitempty" json:"seed,omitempty"`
}

// Digits sends keypad tones as RFC 2833 events, GapMs apart.
type Digits struct {
	Digits string `yaml:"digits" json:"digits"`
	GapMs  int    `yaml:"gapMs,omitempty" json:"gapMs,omitempty"`
}

// Wait pauses; it ends early if the call ends.
type Wait struct {
	Ms int `yaml:"ms" json:"ms"`
}

// Echo plays back what is received for Ms.
type Echo struct {
	Ms int `yaml:"ms" json:"ms"`
}

// Refer asks the other side to transfer the call (blind transfer). To is a SIP URI, or a bare user that is sent
// to the address the call came from.
type Refer struct {
	To string `yaml:"to" json:"to"`
}

// Hangup ends the call with BYE.
type Hangup struct{}

// Kind names the step's action.
func (s Step) Kind() string {
	var kinds []string
	if s.Ring != nil {
		kinds = append(kinds, "ring")
	}
	if s.Reject != nil {
		kinds = append(kinds, "reject")
	}
	if s.EarlyMedia != nil {
		kinds = append(kinds, "earlyMedia")
	}
	if s.Answer != nil {
		kinds = append(kinds, "answer")
	}
	if s.Tone != nil {
		kinds = append(kinds, "tone")
	}
	if s.Speech != nil {
		kinds = append(kinds, "speech")
	}
	if s.Digits != nil {
		kinds = append(kinds, "digits")
	}
	if s.Wait != nil {
		kinds = append(kinds, "wait")
	}
	if s.Echo != nil {
		kinds = append(kinds, "echo")
	}
	if s.Refer != nil {
		kinds = append(kinds, "refer")
	}
	if s.Hangup != nil {
		kinds = append(kinds, "hangup")
	}
	if len(kinds) != 1 {
		return "invalid(" + strings.Join(kinds, ",") + ")"
	}
	return kinds[0]
}

// Validate checks that every step sets exactly one action and its values make sense. answering says whether
// the script runs on the answering side, where signaling steps are allowed.
func Validate(steps []Step, answering bool) error {
	for i, s := range steps {
		k := s.Kind()
		switch k {
		case "ring", "reject", "earlyMedia", "answer":
			if !answering {
				return fmt.Errorf("step %d: %s is only valid on the answering side", i+1, k)
			}
		case "refer", "tone", "speech", "digits", "wait", "echo", "hangup":
		default:
			return fmt.Errorf("step %d: each step must set exactly one action, got %s", i+1, k)
		}
		switch {
		case s.Reject != nil && (s.Reject.Code < 300 || s.Reject.Code > 699):
			return fmt.Errorf("step %d: reject code %d is not a final failure code", i+1, s.Reject.Code)
		case s.Tone != nil && (s.Tone.Hz <= 0 || s.Tone.Hz >= 4000 || s.Tone.Ms <= 0):
			return fmt.Errorf("step %d: tone needs 0 < hz < 4000 and ms > 0", i+1)
		case s.Speech != nil && s.Speech.Ms <= 0:
			return fmt.Errorf("step %d: speech needs ms > 0", i+1)
		case s.EarlyMedia != nil && (s.EarlyMedia.Hz <= 0 || s.EarlyMedia.Hz >= 4000 || s.EarlyMedia.Ms <= 0):
			return fmt.Errorf("step %d: earlyMedia needs 0 < hz < 4000 and ms > 0", i+1)
		case s.Digits != nil && strings.Trim(s.Digits.Digits, "0123456789*#ABCD") != "":
			return fmt.Errorf("step %d: digits may only contain 0-9 * # A-D", i+1)
		case s.Refer != nil && s.Refer.To == "":
			return fmt.Errorf("step %d: refer needs a target", i+1)
		}
		if s.Reject != nil && i != len(steps)-1 {
			return fmt.Errorf("step %d: reject must be the last step", i+1)
		}
	}
	return nil
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

var reasons = map[int]string{
	403: "Forbidden", 404: "Not Found", 408: "Request Timeout", 480: "Temporarily Unavailable",
	486: "Busy Here", 487: "Request Terminated", 488: "Not Acceptable Here", 500: "Server Internal Error",
	503: "Service Unavailable", 603: "Decline", 604: "Does Not Exist Anywhere",
}

// Reason is the usual reason phrase for a SIP status code.
func Reason(code int) string {
	if r, ok := reasons[code]; ok {
		return r
	}
	return "Rejected"
}
