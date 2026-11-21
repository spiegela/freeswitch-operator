package lkagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStore(t *testing.T) {
	s := &Store{}
	s.Joined("sip_1", map[string]string{BenchCallAttribute: "a"})
	s.Joined("sip_2", map[string]string{BenchCallAttribute: "b"})
	s.Received("sip_1", nil, "barge-in", []byte(`{"event":"speech-started"}`))
	s.Received("sip_3", map[string]string{BenchCallAttribute: "c"}, "barge-in", []byte(`{}`)) // never seen joining
	s.Left("sip_1")
	s.Joined("sip_1", map[string]string{BenchCallAttribute: "a2"}) // the same identity again, a new call
	s.Received("sip_1", nil, "barge-in", []byte(`{"event":"speech-stopped"}`))

	a := s.Calls("a")
	if len(a) != 1 || len(a[0].Data) != 1 || a[0].Data[0].Payload != `{"event":"speech-started"}` || a[0].LeftAt == nil {
		t.Errorf("call a: %+v", a)
	}
	if a2 := s.Calls("a2"); len(a2) != 1 || len(a2[0].Data) != 1 || a2[0].Data[0].Payload != `{"event":"speech-stopped"}` {
		t.Errorf("call a2: %+v", a2)
	}
	if b := s.Calls("b"); len(b) != 1 || len(b[0].Data) != 0 {
		t.Errorf("call b: %+v", b)
	}
	if c := s.Calls("c"); len(c) != 1 || len(c[0].Data) != 1 {
		t.Errorf("call c: %+v", c)
	}
	s.Attributes("sip_2", map[string]string{BenchCallAttribute: "b", BargeInAttribute: "freeswitch"})
	if b := s.Calls("b"); b[0].Attributes[BargeInAttribute] != "freeswitch" {
		t.Errorf("attributes not updated: %+v", b)
	}
}

func TestHandler(t *testing.T) {
	s := &Store{}
	s.Joined("sip_1", map[string]string{BenchCallAttribute: "a"})
	ready := false
	h := Handler(s, func() bool { return ready })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz before joining: %d", rec.Code)
	}
	ready = true
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz in the room: %d", rec.Code)
	}

	for bc, want := range map[string]int{"a": 1, "nope": 0} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/calls?benchCall="+bc, nil))
		var got []Participant
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != want {
			t.Errorf("calls %s: %d %q", bc, rec.Code, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/calls", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("calls without benchCall: %d", rec.Code)
	}
}
