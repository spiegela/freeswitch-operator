# Barge-in voice detection

When the caller talks over the agent, the agent should stop. Something has to hear the caller: either the agent
behind LiveKit, or the edge. sipbench's edge configuration can do either, chosen per call, so the two can be compared
on real calls.

## The switch: edge or agent, per call

The configuration ([`vars.xml`](../sipbench/freeswitch/conf/vars.xml),
[`dialplan/trunks.xml`](../sipbench/freeswitch/conf/dialplan/trunks.xml)) reads a fleet-wide default from the
`FreeswitchCluster`'s `spec.env`:

| Variable | Default | |
| --- | --- | --- |
| `BARGE_IN_VAD` | `livekit` | `livekit`: the edge detects nothing, and the agent behind LiveKit SIP runs its own. `freeswitch`: the edge cancels echo and detects, as below |
| `BARGE_IN_VAD_ENGINE` | `silero` | with `freeswitch`: `silero` (`mod_vad_silero`) or `webrtc` (`mod_vad_detect`) |
| `BARGE_IN_ECHO_CANCEL` | `1024` | with `freeswitch`: `preprocess`'s `r.echo_cancel` on what the caller sends; `0` for none |

- **Per call:** a caller's `X-Barge-In-VAD: livekit|freeswitch` header overrides the fleet's mode (any other value
  is ignored). Changing the fleet's default changes `spec.env`, which rolls the fleet one drained node at a time.
- **The backend is always told:** the backend leg carries `X-Barge-In-VAD` with the mode in force, so the agent
  knows whether to run its own detector or listen for the edge's. LiveKit SIP can map the header to a participant
  attribute.

## What the edge does with `freeswitch`

Once the call is answered, on the caller's leg: echo cancellation, then a detector on what the caller sends (media
bug `caller`). On the backend leg, a second detector on what the agent sends, which is what the caller hears (media
bug `agent`). That second detector is how the edge knows when we're talking, so the bar for caller speech can be
higher then.

Each detection goes to the agent as a SIP INFO on the backend leg, `Content-Type: application/x-barge-in+json`:

```json
{"event":"speech-started","party":"caller","engine":"silero","probability":0.98,"at":1795229956441}
```

`event` is `speech-started` or `speech-stopped`, `party` is `caller` or `agent`, `probability` is Silero's only,
and `at` (Unix ms) is when the speech started or stopped, as the detector dates it.

**The exporter sends the INFOs.** The image has no scripting module to do it inside FreeSWITCH, so the metrics
exporter beside it ([`internal/fsexporter/relay.go`](../internal/fsexporter/relay.go)) subscribes to the detectors'
events on the event socket and calls `uuid_send_info` on the call's agent leg. (The dialplan names that leg's uuid in
advance.) So detections are relayed only in a fleet that sets `spec.metrics`. Notes:

- `-barge-in-relay=false` (or `BARGE_IN_RELAY=false`) turns the relay off.
- Its metrics are `freeswitch_barge_in_relay_connected` and `freeswitch_barge_in_detections_total{party,event,result}`,
  where `result` is `relayed`, `no_target` or `failed`.
- FreeSWITCH sends an INFO only to a far end that says it takes them (`X-FS-Support: send_info`), so the agent leg
  sets `fs_send_unsupported_info`.
- `uuid_send_info` returns as soon as the INFO is queued, so the relay can't see a `405` from the far end: `relayed`
  counts INFOs sent, not INFOs received.

## LiveKit SIP needs a patch

Upstream LiveKit SIP (`livekit/sip` at v1.24.0) answers any INFO with `405`, and
maps headers to participant attributes only from the initial INVITE. A patch on the `sip-info-to-room` branch of
[spiegela/sip](https://github.com/spiegela/sip/tree/sip-info-to-room) (one commit on v1.24.0) adds a config option
mapping a body content type to a data packet topic:

```yaml
info_topics:
  application/x-barge-in+json: barge-in
```

An INFO of a listed type on an inbound call is published to the call's room as a reliable data packet from the SIP
participant, its body unchanged, and answered `200`. Any other type gets `415`, and a body over 15 KiB gets `413`.
Without the option, INFO is answered `405` as before. The agent gets each detection from its room's data callback on
topic `barge-in`, from the participant `sip_<number>`.

Note: LiveKit SIP answers a call only once its participant has subscribed to someone's track in the room. An agent
that publishes no audio leaves the call ringing until the edge gives up. A real agent publishes its voice; sipbench's
stand-in agent publishes silence.

## How it's tested

- **On kind, against the mock agent** (sipbench's `barge-in-vad-mode`):
  - the default mode, the header override, and audio unaffected both ways with the detectors and echo canceller in
    the path;
  - a caller talking over the agent's tone: the agent gets `caller` start and stop INFOs and no `agent` ones;
  - an agent talking over the caller's tone: `agent` INFOs and no `caller` ones;
  - a `livekit` call: no INFOs at all.

  The talking is sipbench's synthetic speech, which Silero scores 0.97 to 0.99, as it does recorded speech.
- **On kind, through the patched LiveKit SIP** (`hack/kind.sh livekit`, then `barge-in-livekit`): the stand-in agent
  receives the caller's start and stop as data packets on `barge-in`, and sees the call's mode as a participant
  attribute. With the agent detecting, no packets come.
- **The patch itself:** its unit tests, LiveKit SIP's whole unit suite, and an integration test against a real
  LiveKit server in Docker.

## Measurements so far

From kind, on 8 kHz PCMU calls with synthetic speech:

- **Each detector hears only its own leg's incoming audio.** With our speech going out and only a tone coming back,
  Silero stayed under 0.03.
- **Without echo cancellation, we barge ourselves.** With the mock echoing our speech back, Silero put 74% of the
  windows during our speech at 0.5 or above.
- **FreeSWITCH's echo canceller helps, but not alone.** With `r.echo_cancel=1024` that fell to 51% over 8 s: 80 to
  95% in the first three seconds while speexdsp converges, then 0 to 40%. So the higher bar while we're talking (the
  `agent` detector) is needed, not optional. The mock's echo is a clean digital copy; a real speakerphone's is harder.
- **Latency:** the agent hears of a barge-in about 260 ms after the caller starts talking (270 ms through LiveKit),
  and of the stop about 120 ms after they stop. The relay adds about 10 ms. The rest is the detector's own minimums:
  250 ms of speech before it calls a start, 100 ms of silence before it calls a stop. Both can be tuned
  (`barge_in_vad_args`).

## Requirements behind the design

- **The edge must decode the audio of every call that should have barge-in, for the whole call.** It already
  anchors every call's media, but with `inherit_codec` both legs share a codec, so nothing is transcoded today.
  Detection adds a decode and the detector's own work, so capacity has to be sized with detection on every call.
- **Listen only to what the caller sends.** On the caller's leg FreeSWITCH keeps the two directions apart, so the
  detector runs on the read stream only. On a mixed stream there's no telling who is talking.
- **Cancel our own echo**, or the detector hears our agent and we barge ourselves.
- **Rely on drachtio's modules.** `mod_vad_silero` (a Silero neural VAD on ONNX Runtime), `mod_vad_detect` (WebRTC's
  VAD) and `preprocess` (speexdsp echo cancellation) are in drachtio's image, not stock FreeSWITCH. Using them ties
  us to that image or to building them ourselves (see the README's [decisions](../README.md#decisions)).

## Open questions

- Where the patched LiveKit SIP image is published, and whether to offer the change upstream.
- What the agent does in each mode: with `livekit`, its own VAD drives interruptions as today; with `freeswitch`, it
  should turn that off and act on the edge's signal.
- The cost per call (decode, echo cancellation, Silero on two legs) and what it does to sessions per node. Each
  Silero detector creates its own ONNX session at answer (40 to 60 ms on kind) and holds it for the call.
- How the echo canceller does on a real speakerphone's echo, and why its tail setting changed nothing.
- Why a hand-loaded `mod_vad_silero` reported every 60 ms window, while one loaded at startup reports changes only.
- An echo scenario on the bench: the agent talks, the caller echoes it back, and there should be no `caller` INFOs.
  The measurements above suggest it would fail today.
