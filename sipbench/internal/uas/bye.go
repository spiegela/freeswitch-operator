package uas

import (
	"context"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo/sip"
)

// A dialog's BYE goes to the far end's current target. A re-INVITE may move that target (a target refresh, RFC 3261
// 12.2), as FreeSWITCH's does when another node takes a call over with `sofia recover`: diago keeps the new
// Contact, but sipgo builds the BYE from the dialog's first INVITE (its Contact as the UAS, its answer's as the UAC),
// so the BYE would go to the node that is gone. After a refresh these send their own BYE to the current Contact,
// through sipgo's WriteBye, which adds the dialog's headers and route set as it does for its own. The dialog's
// stored messages are left alone: sipgo and diago read them from other goroutines.

// HangupClient hangs up a call placed through d.
func HangupClient(ctx context.Context, d *diago.DialogClientSession) error {
	if res := d.InviteResponse; res != nil {
		if c := d.RemoteContact(); c != nil && c != res.Contact() {
			return d.WriteBye(ctx, byeTo(c, d.InviteRequest))
		}
	}
	return d.Hangup(ctx)
}

// hangupServer hangs up a call answered through d.
func hangupServer(ctx context.Context, d *diago.DialogServerSession) error {
	if req := d.InviteRequest; req != nil {
		// A refreshed Contact came in a re-INVITE, so the dialog is confirmed: a BYE, never a rejection.
		if c := d.RemoteContact(); c != nil && c != req.Contact() {
			err := d.WriteBye(ctx, byeTo(c, req))
			if err != nil {
				endDialog(d, req)
			}
			return err
		}
	}
	return d.Hangup(ctx)
}

// endDialog ends a dialog whose BYE failed. Its BYE sent, the dialog is over whatever the answer (RFC 3261 15.1.1),
// but sipgo ends it only on a 200, and diago hangs up a dialog that isn't over once the handler returns: that would
// send a second BYE, to the first Contact, the node the call moved away from. sipgo's own handling of a BYE is the
// one exported way to end it, so it is handed one, built from the INVITE so its CSeq passes, and a transaction that
// sends nothing.
func endDialog(d *diago.DialogServerSession, invite *sip.Request) {
	bye := invite.Clone()
	bye.Method = sip.BYE
	_ = d.ReadBye(bye, silentTx{})
}

// silentTx takes the 200 sipgo answers a BYE with and drops it. ReadBye calls nothing else on it.
type silentTx struct{ sip.ServerTransaction }

func (silentTx) Respond(*sip.Response) error { return nil }

// byeTo is a BYE for the dialog that invite began, to contact: what sipgo's own BYEs hold before WriteBye fills in
// the dialog's headers.
func byeTo(contact *sip.ContactHeader, invite *sip.Request) *sip.Request {
	bye := sip.NewRequest(sip.BYE, *contact.Address.Clone())
	bye.SipVersion = invite.SipVersion
	bye.SetTransport(invite.Transport())
	return bye
}
