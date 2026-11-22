package script

import "testing"

func TestValidate(t *testing.T) {
	ok := []Step{{Answer: &Answer{}}, {Tone: &Tone{Hz: 1000, Ms: 100}}, {Speech: &Speech{Ms: 100}}, {Digits: &Digits{Digits: "0123456789*#ABCD"}}, {Refer: &Refer{To: "x"}}, {Hangup: &Hangup{}}}
	if err := Validate(ok, true); err != nil {
		t.Errorf("valid answering script: %v", err)
	}
	bad := map[string][]Step{
		"two actions":      {{Answer: &Answer{}, Hangup: &Hangup{}}},
		"no action":        {{}},
		"reject code":      {{Reject: &Reject{Code: 200}}},
		"reject not last":  {{Reject: &Reject{Code: 486}}, {Wait: &Wait{Ms: 1}}},
		"tone too high":    {{Tone: &Tone{Hz: 5000, Ms: 10}}},
		"tone no duration": {{Tone: &Tone{Hz: 500}}},
		"speech no length": {{Speech: &Speech{Seed: 2}}},
		"bad digit":        {{Digits: &Digits{Digits: "12x"}}},
		"refer no target":  {{Refer: &Refer{}}},
	}
	for name, steps := range bad {
		if err := Validate(steps, true); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := Validate([]Step{{Ring: &Ring{Ms: 10}}}, false); err == nil {
		t.Errorf("ring on the calling side should be rejected")
	}
}

func TestReason(t *testing.T) {
	if Reason(486) != "Busy Here" || Reason(603) != "Decline" || Reason(699) == "" {
		t.Errorf("unexpected reason phrases")
	}
}
