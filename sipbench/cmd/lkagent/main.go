// Command lkagent stands in for a LiveKit agent on the bench. At start it sets up LiveKit SIP to take the edge's
// calls into one room: an inbound trunk from private addresses that copies each call's X-Sipbench-Call and
// X-Barge-In-VAD headers into its participant's attributes, and a dispatch rule sending every call on that trunk to
// the room. It replaces any trunk and rule of the same name, so a restart puts back what it needs. Then it joins the
// room, notes every participant and every data packet they send (LiveKit SIP's participant forwards the edge's
// barge-in SIP INFOs as data packets, with info_topics), and serves them by bench call for the runner
// (internal/lkagent). It publishes a voice track of silence and subscribes to the callers' audio, as an agent
// would: LiveKit SIP answers a call only once its participant has subscribed to someone's track, and the edge hears
// no agent speech in silence. It joins as a standard participant, not an agent, since LiveKit
// closes a room with only agents left in it and the agent would have to keep rejoining.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/lkagent"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/uas"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lkagent:", err)
		os.Exit(1)
	}
}

func run() error {
	lkURL := flag.String("url", envOr("LIVEKIT_URL", "ws://livekit-server:7880"), "LiveKit server URL")
	room := flag.String("room", envOr("LKAGENT_ROOM", "sipbench"), "room to take the calls into and sit in")
	name := flag.String("name", envOr("LKAGENT_NAME", "sipbench"),
		"name of the inbound trunk and dispatch rule it sets up")
	allowed := flag.String("allowed-addresses",
		envOr("LKAGENT_ALLOWED_ADDRESSES", "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"),
		"comma-separated addresses or CIDRs the trunk takes calls from")
	httpAddr := flag.String("http", envOr("LKAGENT_HTTP", "0.0.0.0:8080"), "API listen address")
	level := flag.String("log-level", envOr("LKAGENT_LOG_LEVEL", "info"), "debug, info, warn or error")
	flag.Parse()
	key, secret := os.Getenv("LIVEKIT_API_KEY"), os.Getenv("LIVEKIT_API_SECRET")
	if key == "" || secret == "" {
		return errors.New("LIVEKIT_API_KEY and LIVEKIT_API_SECRET are required")
	}

	var lv slog.Level
	if err := lv.UnmarshalText([]byte(*level)); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := &lkagent.Store{}
	var inRoom atomic.Bool
	srv := &http.Server{Addr: *httpAddr, Handler: lkagent.Handler(store, inRoom.Load), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("API stopped", "error", err)
			stop()
		}
	}()
	defer func() { _ = srv.Close() }()

	sipc := lksdk.NewSIPClient(httpURL(*lkURL), key, secret)
	for {
		err := setUp(ctx, sipc, *name, *room, splitList(*allowed))
		if err == nil {
			break
		}
		log.Warn("setting up LiveKit SIP; retrying", "error", err)
		if !sleep(ctx, 3*time.Second) {
			return nil
		}
	}
	log.Info("LiveKit SIP takes calls into the room", "room", *room, "trunkAndRule", *name)

	info := lksdk.ConnectInfo{APIKey: key, APISecret: secret, RoomName: *room, ParticipantIdentity: "sipbench-agent"}
	for ctx.Err() == nil {
		gone := make(chan struct{})
		cb := lksdk.NewRoomCallback()
		cb.OnParticipantConnected = func(p *lksdk.RemoteParticipant) {
			store.Joined(p.Identity(), p.Attributes())
			log.Info("participant joined", "identity", p.Identity(), "benchCall", p.Attributes()[lkagent.BenchCallAttribute])
		}
		cb.OnParticipantDisconnected = func(p *lksdk.RemoteParticipant) {
			store.Left(p.Identity())
			log.Info("participant left", "identity", p.Identity())
		}
		cb.OnAttributesChanged = func(_ map[string]string, p lksdk.Participant) {
			store.Attributes(p.Identity(), p.Attributes())
		}
		cb.OnDataPacket = func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
			pkt, ok := data.(*lksdk.UserDataPacket)
			if !ok {
				return
			}
			var attrs map[string]string
			if params.Sender != nil {
				attrs = params.Sender.Attributes()
			}
			store.Received(params.SenderIdentity, attrs, pkt.Topic, pkt.Payload)
			log.Info("data received", "from", params.SenderIdentity, "topic", pkt.Topic, "payload", string(pkt.Payload))
		}
		cb.OnDisconnected = func() { close(gone) }
		r, err := lksdk.ConnectToRoom(*lkURL, info, cb)
		if err != nil {
			log.Warn("joining the room; retrying", "room", *room, "error", err)
			if !sleep(ctx, 3*time.Second) {
				return nil
			}
			continue
		}
		voice, err := publishSilence(r)
		if err != nil {
			log.Warn("publishing the voice track; rejoining", "error", err)
			r.Disconnect()
			if !sleep(ctx, 3*time.Second) {
				return nil
			}
			continue
		}
		inRoom.Store(true)
		log.Info("in the room", "room", *room)
		for _, p := range r.GetRemoteParticipants() { // already there when the agent joined
			store.Joined(p.Identity(), p.Attributes())
		}
		select {
		case <-ctx.Done():
			r.Disconnect()
		case <-gone:
			log.Warn("left the room; rejoining", "room", *room)
		}
		voice()
		inRoom.Store(false)
	}
	return nil
}

// opusSilence is one 20 ms Opus frame of silence.
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// publishSilence publishes the agent's voice track and keeps writing silence to it until the returned stop is called.
func publishSilence(r *lksdk.Room) (stop func(), err error) {
	track, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	})
	if err != nil {
		return nil, err
	}
	if _, err := r.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "voice"}); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = track.WriteSample(media.Sample{Data: opusSilence, Duration: 20 * time.Millisecond}, nil)
			}
		}
	}()
	return func() { close(done) }, nil
}

// setUp replaces LiveKit SIP's inbound trunk and dispatch rule called name with ones sending calls from allowed into
// room, the call's bench headers copied into its participant's attributes.
func setUp(ctx context.Context, c *lksdk.SIPClient, name, room string, allowed []string) error {
	trunks, err := c.ListSIPInboundTrunk(ctx, &livekit.ListSIPInboundTrunkRequest{})
	if err != nil {
		return fmt.Errorf("listing inbound trunks: %w", err)
	}
	for _, t := range trunks.Items {
		if t.Name == name {
			if _, err := c.DeleteSIPTrunk(ctx, &livekit.DeleteSIPTrunkRequest{SipTrunkId: t.SipTrunkId}); err != nil {
				return fmt.Errorf("deleting trunk %s: %w", t.SipTrunkId, err)
			}
		}
	}
	rules, err := c.ListSIPDispatchRule(ctx, &livekit.ListSIPDispatchRuleRequest{})
	if err != nil {
		return fmt.Errorf("listing dispatch rules: %w", err)
	}
	for _, r := range rules.Items {
		if r.Name == name {
			req := &livekit.DeleteSIPDispatchRuleRequest{SipDispatchRuleId: r.SipDispatchRuleId}
			if _, err := c.DeleteSIPDispatchRule(ctx, req); err != nil {
				return fmt.Errorf("deleting dispatch rule %s: %w", r.SipDispatchRuleId, err)
			}
		}
	}
	trunk, err := c.CreateSIPInboundTrunk(ctx, &livekit.CreateSIPInboundTrunkRequest{Trunk: &livekit.SIPInboundTrunkInfo{
		Name:             name,
		AllowedAddresses: allowed,
		HeadersToAttributes: map[string]string{
			uas.BenchCallHeader: lkagent.BenchCallAttribute,
			"X-Barge-In-VAD":    lkagent.BargeInAttribute,
		},
	}})
	if err != nil {
		return fmt.Errorf("creating the inbound trunk: %w", err)
	}
	_, err = c.CreateSIPDispatchRule(ctx, &livekit.CreateSIPDispatchRuleRequest{DispatchRule: &livekit.SIPDispatchRuleInfo{
		Name:     name,
		TrunkIds: []string{trunk.SipTrunkId},
		Rule: &livekit.SIPDispatchRule{Rule: &livekit.SIPDispatchRule_DispatchRuleDirect{
			DispatchRuleDirect: &livekit.SIPDispatchRuleDirect{RoomName: room},
		}},
	}})
	if err != nil {
		return fmt.Errorf("creating the dispatch rule: %w", err)
	}
	return nil
}

// httpURL is the server API's address for a LiveKit URL given as ws(s)://.
func httpURL(u string) string {
	if rest, ok := strings.CutPrefix(u, "ws"); ok {
		return "http" + rest
	}
	return u
}

func splitList(s string) []string {
	var out []string
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
