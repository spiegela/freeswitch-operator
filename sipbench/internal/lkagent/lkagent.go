// Package lkagent is the bench's stand-in for a LiveKit agent: it sits in one LiveKit room, notes each participant
// that joins it (LiveKit SIP's participant for each call the edge sends there) and every data packet they send, and
// serves what it noted, by bench call, for the runner to check. A call is told apart by its participant's
// BenchCallAttribute, which LiveKit SIP sets from the call's X-Sipbench-Call header (the inbound trunk's
// headers_to_attributes, which cmd/lkagent sets up).
package lkagent

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"
)

// The participant attributes LiveKit SIP sets from the call's headers, through the trunk cmd/lkagent sets up.
const (
	BenchCallAttribute = "sipbench.call"    // from X-Sipbench-Call
	BargeInAttribute   = "sipbench.bargeIn" // from X-Barge-In-VAD: who detects the caller talking
	keep               = 30 * time.Minute   // how long a participant's record is kept after it leaves
)

// Participant is one participant of the agent's room, and what it sent there.
type Participant struct {
	Identity   string            `json:"identity"`
	Attributes map[string]string `json:"attributes,omitempty"`
	JoinedAt   time.Time         `json:"joinedAt"`
	LeftAt     *time.Time        `json:"leftAt,omitempty"`
	Data       []Data            `json:"data,omitempty"`
}

// Data is one data packet a participant sent.
type Data struct {
	At      time.Time `json:"at"`
	Topic   string    `json:"topic,omitempty"`
	Payload string    `json:"payload"`
}

// Store holds the room's participants, newest last.
type Store struct {
	mu   sync.Mutex
	seen []*Participant
}

// Joined notes a participant joining.
func (s *Store) Joined(identity string, attrs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	s.seen = append(s.seen, &Participant{Identity: identity, Attributes: attrs, JoinedAt: time.Now()})
}

// Attributes notes a participant's attributes changing.
func (s *Store) Attributes(identity string, attrs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.find(identity); p != nil {
		p.Attributes = attrs
	}
}

// Left notes a participant leaving.
func (s *Store) Left(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.find(identity); p != nil {
		now := time.Now()
		p.LeftAt = &now
	}
}

// Received notes a data packet from a participant; one the agent never saw join is noted as joining now.
func (s *Store) Received(identity string, attrs map[string]string, topic string, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(identity)
	if p == nil {
		p = &Participant{Identity: identity, Attributes: attrs, JoinedAt: time.Now()}
		s.seen = append(s.seen, p)
	}
	p.Data = append(p.Data, Data{At: time.Now(), Topic: topic, Payload: string(payload)})
}

// Calls returns copies of the participants a bench call brought into the room, earliest first.
func (s *Store) Calls(benchCall string) []Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Participant
	for _, p := range s.seen {
		if p.Attributes[BenchCallAttribute] != benchCall {
			continue
		}
		c := *p
		c.Data = slices.Clone(p.Data)
		out = append(out, c)
	}
	return out
}

// find returns the latest participant with this identity still in the room. Callers hold mu.
func (s *Store) find(identity string) *Participant {
	for i := len(s.seen) - 1; i >= 0; i-- {
		if p := s.seen[i]; p.Identity == identity && p.LeftAt == nil {
			return p
		}
	}
	return nil
}

// prune drops participants that left more than keep ago. Callers hold mu.
func (s *Store) prune() {
	cut := time.Now().Add(-keep)
	s.seen = slices.DeleteFunc(s.seen, func(p *Participant) bool { return p.LeftAt != nil && p.LeftAt.Before(cut) })
}

// Handler serves GET /calls?benchCall=... (the participants that call brought, as JSON) and GET /healthz, which
// answers 200 once ready() is true: the agent is in its room.
func Handler(s *Store, ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /calls", func(w http.ResponseWriter, r *http.Request) {
		bc := r.URL.Query().Get("benchCall")
		if bc == "" {
			http.Error(w, "benchCall is required", http.StatusBadRequest)
			return
		}
		calls := s.Calls(bc)
		if calls == nil {
			calls = []Participant{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(calls)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not in the room", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
