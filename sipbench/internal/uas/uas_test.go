package uas

import "testing"

func TestRunOf(t *testing.T) {
	for in, want := range map[string]string{
		"3fa2b1c9.4-basic-two-way-a-7": "3fa2b1c9.4",
		"norun":                        "norun",
		"":                             "",
	} {
		if got := RunOf(in); got != want {
			t.Errorf("RunOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeleteRun(t *testing.T) {
	s := &Store{}
	for _, b := range []string{"r1-x-a-1", "r2-x-a-2", "r1-y-a-3", ""} {
		s.add(Call{BenchCall: b})
	}
	s.DeleteRun("r1")
	got := make([]string, 0, 2)
	for _, c := range s.Calls("") {
		got = append(got, c.BenchCall)
	}
	if len(got) != 2 || got[0] != "r2-x-a-2" || got[1] != "" {
		t.Errorf("after DeleteRun(r1): %q", got)
	}
}
