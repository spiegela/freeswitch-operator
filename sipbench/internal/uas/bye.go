package uas

import (
	"context"

	"github.com/emiago/diago"
)

// A dialog's BYE goes to the far end's current target. A re-INVITE may move that target (a target refresh, RFC 3261
// 12.2), as FreeSWITCH's does when another node takes a call over with `sofia recover`: diago keeps the new
// Contact, but sipgo builds the BYE from the dialog's first INVITE (its Contact as the UAS, its answer's as the UAC),
// so the BYE would go to the node that is gone. These put the current Contact there first.

// HangupClient hangs up a call placed through d.
func HangupClient(ctx context.Context, d *diago.DialogClientSession) error {
	if res := d.InviteResponse; res != nil {
		if c := d.RemoteContact(); c != nil && c != res.Contact() {
			res.ReplaceHeader(c.Clone())
		}
	}
	return d.Hangup(ctx)
}

// hangupServer hangs up a call answered through d.
func hangupServer(ctx context.Context, d *diago.DialogServerSession) error {
	if req := d.InviteRequest; req != nil {
		if c := d.RemoteContact(); c != nil && c != req.Contact() {
			req.ReplaceHeader(c.Clone())
		}
	}
	return d.Hangup(ctx)
}
