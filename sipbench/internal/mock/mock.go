// Package mock is the bench's stand-in for whatever sits behind the edge: LiveKit SIP and an agent today, the
// real thing later. It answers each call by running the script of the route its called user names, records what
// it received, and exposes those records over HTTP so the runner can check them.
package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"gopkg.in/yaml.v3"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/script"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/uas"
)

// Config is the mock backend's behaviour.
type Config struct {
	// DetectHz are the tone frequencies the mock listens for on every call.
	DetectHz []float64 `yaml:"detectHz" json:"detectHz"`
	// DefaultRoute answers calls whose user matches no route. Empty means such calls are rejected with 404.
	DefaultRoute string `yaml:"defaultRoute,omitempty" json:"defaultRoute,omitempty"`
	// HoldMs is how long an answered call stays up after its script ends, waiting for the other end.
	HoldMs int `yaml:"holdMs,omitempty" json:"holdMs,omitempty"`
	// Routes maps the called user (the Request-URI user part) to a script.
	Routes map[string]Route `yaml:"routes" json:"routes"`
}

// Route is the script for calls to one user.
type Route struct {
	Steps []script.Step `yaml:"steps" json:"steps"`
}

// Validate checks every route's script.
func (c *Config) Validate() error {
	for name, r := range c.Routes {
		if len(r.Steps) == 0 {
			return fmt.Errorf("route %q has no steps", name)
		}
		if err := script.Validate(r.Steps, true); err != nil {
			return fmt.Errorf("route %q: %w", name, err)
		}
	}
	if c.DefaultRoute != "" {
		if _, ok := c.Routes[c.DefaultRoute]; !ok {
			return fmt.Errorf("defaultRoute %q is not a route", c.DefaultRoute)
		}
	}
	return nil
}

func (c *Config) clone() *Config {
	out := *c
	out.DetectHz = append([]float64(nil), c.DetectHz...)
	out.Routes = make(map[string]Route, len(c.Routes))
	for k, v := range c.Routes {
		out.Routes[k] = Route{Steps: append([]script.Step(nil), v.Steps...)}
	}
	return &out
}

// ParseConfig reads YAML (JSON is valid YAML).
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && err != io.EOF {
		return nil, err
	}
	if c.Routes == nil {
		c.Routes = map[string]Route{}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadConfig reads a config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Server answers calls and serves the control API.
type Server struct {
	log     *slog.Logger
	initial *Config
	store   *uas.Store

	mu  sync.RWMutex
	cfg *Config
}

// NewServer starts from cfg; POST /reset returns to it.
func NewServer(cfg *Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log, initial: cfg.clone(), cfg: cfg.clone(), store: &uas.Store{}}
}

// Store exposes the call records.
func (s *Server) Store() *uas.Store { return s.store }

func (s *Server) config() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// HandleCall answers one call. Pass it to diago's Serve.
func (s *Server) HandleCall(d *diago.DialogServerSession) {
	cfg := s.config()
	user := d.InviteRequest.Recipient.User
	name := user
	route, ok := cfg.Routes[user]
	if !ok && cfg.DefaultRoute != "" {
		name, route, ok = cfg.DefaultRoute, cfg.Routes[cfg.DefaultRoute], true
	}
	steps := route.Steps
	if !ok {
		name = ""
		steps = []script.Step{{Reject: &script.Reject{Code: 404, Reason: "No such route"}}}
	}
	hold := time.Duration(cfg.HoldMs) * time.Millisecond
	uas.Handle(d, name, steps, s.store, uas.Options{DetectHz: cfg.DetectHz, Hold: hold, Log: s.log})
}

// Handler is the control API:
//
//	GET  /healthz              200 when serving
//	GET  /calls[?benchCall=id] call records, oldest first
//	POST /reset                forget calls and return to the starting config
//	GET  /config               the current config
//	PUT  /config               replace the config (YAML or JSON body)
//	POST /routes               add or replace routes (YAML or JSON map of name to route)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /calls", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.store.Calls(r.URL.Query().Get("benchCall")))
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, _ *http.Request) {
		s.store.Reset()
		s.mu.Lock()
		s.cfg = s.initial.clone()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.config())
	})
	mux.HandleFunc("PUT /config", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c, err := ParseConfig(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.cfg = c
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /routes", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var routes map[string]Route
		if err := yaml.Unmarshal(b, &routes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		next := s.cfg.clone()
		for k, v := range routes {
			next.Routes[k] = v
		}
		if err := next.Validate(); err != nil {
			s.mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.cfg = next
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
