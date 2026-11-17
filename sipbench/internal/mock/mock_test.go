package mock

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShippedConfigLoads(t *testing.T) {
	c, err := LoadConfig("../../backends/mock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent", "busy", "decline", "noanswer", "hangup", "dtmf", "transfer", "echo", "hold"} {
		if _, ok := c.Routes[want]; !ok {
			t.Errorf("route %q missing", want)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	cases := map[string]string{
		"unknown field":   "routes: {}\nbogus: 1",
		"default missing": "defaultRoute: nope\nroutes: {}",
		"empty route":     "routes: {a: {steps: []}}",
		"reject not last": "routes: {a: {steps: [{reject: {code: 486}}, {answer: {}}]}}",
	}
	for name, y := range cases {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, r))
	return w
}

func TestAPI(t *testing.T) {
	cfg, err := ParseConfig([]byte("routes: {agent: {steps: [{answer: {}}]}}"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := s.Handler()

	if w := do(t, h, "GET", "/healthz", ""); w.Code != 200 {
		t.Fatalf("healthz: %d", w.Code)
	}
	if w := do(t, h, "GET", "/calls", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("calls: %d %q", w.Code, w.Body.String())
	}

	// Add a route, then check it is live.
	if w := do(t, h, "POST", "/routes", "busy: {steps: [{reject: {code: 486}}]}"); w.Code != 204 {
		t.Fatalf("routes: %d %s", w.Code, w.Body.String())
	}
	var got Config
	_ = json.Unmarshal(do(t, h, "GET", "/config", "").Body.Bytes(), &got)
	if _, ok := got.Routes["busy"]; !ok || len(got.Routes) != 2 {
		t.Fatalf("after POST /routes: %v", got.Routes)
	}

	// Invalid routes are refused and change nothing.
	if w := do(t, h, "POST", "/routes", "bad: {steps: [{tone: {hz: 9000, ms: 1}}]}"); w.Code != 400 {
		t.Fatalf("invalid route accepted: %d", w.Code)
	}

	// Replace the whole config.
	if w := do(t, h, "PUT", "/config", `{"routes": {"only": {"steps": [{"answer": {}}]}}}`); w.Code != 204 {
		t.Fatalf("put config: %d %s", w.Code, w.Body.String())
	}
	got = Config{}
	_ = json.Unmarshal(do(t, h, "GET", "/config", "").Body.Bytes(), &got)
	if _, ok := got.Routes["only"]; !ok || len(got.Routes) != 1 {
		t.Fatalf("after PUT /config: %v", got.Routes)
	}

	// Reset returns to the starting config.
	if w := do(t, h, "POST", "/reset", ""); w.Code != 204 {
		t.Fatalf("reset: %d", w.Code)
	}
	got = Config{}
	_ = json.Unmarshal(do(t, h, "GET", "/config", "").Body.Bytes(), &got)
	if _, ok := got.Routes["agent"]; !ok || len(got.Routes) != 1 {
		t.Fatalf("after reset: %v", got.Routes)
	}
}
