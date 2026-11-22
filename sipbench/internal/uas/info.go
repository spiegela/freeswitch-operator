package uas

import (
	"log/slog"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Info is one INFO request a call received.
type Info struct {
	At          time.Time `json:"at"`
	ContentType string    `json:"contentType"`
	Body        string    `json:"body,omitempty"`
}

// dtmfInfo is the INFO body type diago handles itself, as keypad tones.
const dtmfInfo = "application/dtmf-relay"

// InfoMiddleware records the INFO requests diago would refuse (anything but keypad tones) on the store's call
// whose dialog they arrive in, and answers 200 OK; 481 when no call matches. Diago answers every other INFO 406,
// so an end with this keeps whatever the edge tells it mid-call (barge-in detections, say) for the runner to
// check. Each one is logged to log (nil: discarded) with its answer. Pass it to diago.WithServerRequestMiddleware.
func InfoMiddleware(store *Store, log *slog.Logger) func(next sipgo.RequestHandler) sipgo.RequestHandler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sip.Request, tx sip.ServerTransaction) {
			ct := ""
			if h := req.ContentType(); h != nil {
				ct = h.Value()
			}
			if req.Method != sip.INFO || ct == dtmfInfo {
				next(req, tx)
				return
			}
			code, reason := sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist"
			id, err := sip.DialogIDFromRequestUAS(req)
			if err == nil && store.addInfo(id, Info{At: time.Now(), ContentType: ct, Body: string(req.Body())}) {
				code, reason = sip.StatusOK, "OK"
			}
			log.Info("INFO received", "call", id, "contentType", ct, "body", string(req.Body()), "answer", int(code))
			_ = tx.Respond(sip.NewResponseFromRequest(req, code, reason, nil))
		}
	}
}

// addInfo files info under the call whose dialog id is id, and says whether there was one.
func (s *Store) addInfo(id string, info Info) bool {
	s.mu.Lock()
	var found *live
	for _, l := range s.calls {
		if l.call.ID == id { // set once at add, so safe to read without the call's lock
			found = l
		}
	}
	s.mu.Unlock()
	if found == nil {
		return false
	}
	found.update(func(c *Call) { c.Infos = append(c.Infos, info) })
	return true
}
