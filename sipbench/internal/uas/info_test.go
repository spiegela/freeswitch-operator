package uas

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

type fakeTx struct {
	sip.ServerTransaction
	res *sip.Response
}

func (f *fakeTx) Respond(res *sip.Response) error { f.res = res; return nil }

func info(callID, toTag, fromTag, ct, body string) *sip.Request {
	req := sip.NewRequest(sip.INFO, sip.Uri{User: "agent", Host: "mock"})
	req.AppendHeader(sip.NewHeader("Call-ID", callID))
	req.AppendHeader(&sip.FromHeader{Address: sip.Uri{User: "edge", Host: "edge"}, Params: sip.HeaderParams{{K: "tag", V: fromTag}}})
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "agent", Host: "mock"}, Params: sip.HeaderParams{{K: "tag", V: toTag}}})
	req.AppendHeader(sip.NewHeader("Content-Type", ct))
	req.SetBody([]byte(body))
	return req
}

func TestInfoMiddleware(t *testing.T) {
	store := &Store{}
	store.add(Call{ID: sip.DialogIDMake("c1", "ours", "theirs"), BenchCall: "r-a"})
	passed := 0
	h := InfoMiddleware(store, nil)(func(*sip.Request, sip.ServerTransaction) { passed++ })

	tx := &fakeTx{}
	h(info("c1", "ours", "theirs", "application/x-barge-in+json", `{"event":"speech-started"}`), tx)
	if tx.res == nil || tx.res.StatusCode != 200 {
		t.Fatalf("INFO in a known dialog: got %v, want 200", tx.res)
	}
	tx = &fakeTx{}
	h(info("c2", "ours", "theirs", "text/plain", "x"), tx)
	if tx.res == nil || tx.res.StatusCode != 481 {
		t.Fatalf("INFO in an unknown dialog: got %v, want 481", tx.res)
	}
	// Keypad tones and other methods are diago's.
	h(info("c1", "ours", "theirs", dtmfInfo, "Signal=1\r\nDuration=100\r\n"), &fakeTx{})
	h(sip.NewRequest(sip.BYE, sip.Uri{Host: "mock"}), &fakeTx{})
	if passed != 2 {
		t.Errorf("%d requests passed on, want 2 (the keypad INFO and the BYE)", passed)
	}

	got := store.Calls("r-a")[0].Infos
	if len(got) != 1 || got[0].ContentType != "application/x-barge-in+json" || got[0].Body != `{"event":"speech-started"}` {
		t.Errorf("recorded %+v", got)
	}
}
