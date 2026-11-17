# sipbench

A SIP test bench for an edge: a FreeSWITCH SBC that takes calls from carriers and PBXs ("trunks") and
bridges them to whatever sits behind it ("the backend"). sipbench plays every other party:

- **the far end**: places calls into the edge with chosen headers and codecs, plays and detects tones, sends
  and receives RFC 2833 digits, cancels, and hangs up;
- **the backend**: `mockbackend`, a scriptable user agent server that stands in for the real thing (a SIP
  gateway in front of a voice agent, a PBX, anything) until it exists, and records what each call received;
- **transfer targets**: the runner answers the calls the edge places when the backend asks for a transfer.

Every scenario then checks what each end saw: SIP response codes, audio in both directions, digits, headers,
who hung up, and the codec. Swap the mock for a real backend later and the same scenarios keep running; the
checks that need the mock's records are skipped.

```
 far end (runner)          edge (FreeSWITCH)                 backend (mockbackend)
 ─────────────────  5060   ───────────────────────   5080 →  ─────────────────────
  INVITE + X- hdrs  ─────► trunks ── bridge ── backend ─────►  route by R-URI user
  600 Hz   ◄───────────────────── RTP ─────────────────────►   1000 Hz
                                     ▲  REFER (ended here)  ◄──
 transfer target  ◄── INVITE ── transfers context
 (runner :5090)       1800 Hz
```

## Quick start

Needs Docker with Compose. No accounts, nothing leaves the machine.

```sh
docker compose up -d --build                                   # edge + mock backend
docker compose run --rm --use-aliases sipbench run scenarios   # run every scenario
docker compose run --rm --use-aliases sipbench run -run 'dtmf|transfer' -json reports/run.json -junit reports/junit.xml scenarios
docker compose down
```

`--use-aliases` matters: the edge places transfers to `sipbench:5090`, so the runner must answer to that
name on the compose network.

Unit tests and the integration test:

```sh
go vet ./... && go test ./...           # unit tests, including real calls between the runner and an in-process mock
docker compose up -d --build
docker compose run --rm integration     # go test -tags integration: every scenario as a subtest
docker compose down
```

On a cluster, behind Kamailio and a `FreeswitchCluster`: `make kind-up kind-deploy kind-bench` from the repository
root (see "The edge on kind" in the top-level README). The same configuration and scenarios run there:
[`freeswitch/kustomization.yaml`](freeswitch/kustomization.yaml) turns `freeswitch/conf` into one ConfigMap per
directory, and [`k8s/`](k8s/) holds the mock backend, the runner's transfer Service and the runner Job. Add a file
under `freeswitch/conf` to that kustomization too; `go test ./...` fails until you do. `hack/kind.sh bench <regexp>`
runs only the scenarios whose names match.

Each of `-edge`, `-mock`, `-esl`, `-esl-password`, `-target`, `-caps` and `-run` takes its default from
`SIPBENCH_<FLAG>` (`SIPBENCH_ESL_PASSWORD` for `-esl-password`). A variable set to the empty string counts as set,
so `SIPBENCH_ESL=""` turns the event socket off rather than falling back to `freeswitch:8021`.

Set `SIPBENCH_SIP_DEBUG=true` with `-log-level debug` to log every SIP message the runner sends and receives.

## Results

All scenarios on FreeSWITCH 1.10.10 (`drachtio/drachtio-freeswitch-mrf:0.9.6`, arm64), three consecutive runs:

| Scenario | Area | Result |
|---|---|---|
| basic-two-way | basic | pass |
| backend-hangup | basic | pass |
| cancel | basic | pass |
| echo | basic | pass |
| busy-fallback (486) | failures | pass |
| decline-fallback (603) | failures | pass |
| no-answer-fallback | failures | pass |
| unreachable-fallback | failures | pass |
| header-passthrough | headers | pass |
| blind-transfer | transfers | pass |
| dtmf-both-ways | dtmf | pass |
| codec-pcmu | codecs | pass |
| codec-pcma | codecs | pass |
| codec-g722 | codecs | **skip**: needs `codec:g722`; neither this FreeSWITCH image nor the runner has G.722 |
| admission-limit | admission | pass |
| options | health | pass |

On kind, through Kamailio: the same, except that `admission-limit` skips. It changes one FreeSWITCH node's
limit over the event socket, and through a proxy the runner can't know which node takes the call. `options` is
then answered by Kamailio.

## Bench conventions

Each end plays a frequency only it uses, so hearing it proves the path from that end:

| Tone | Played by |
|---|---|
| 600 Hz | far end (runner caller) |
| 1000 Hz | backend (mock) |
| 1400 Hz | edge fallback |
| 1800 Hz | transfer target (runner) |

Tones are detected with a Goertzel filter on 20 ms frames; a tone counts as heard after 200 ms.

Every call carries `X-Sipbench-Call: <run>-<scenario>-<call>-<n>`. The edge passes X- headers through (which is
itself under test), so the mock and the transfer target can file what they saw under the call that caused it.

## Scenarios

One YAML file per scenario in `scenarios/`. A full example:

```yaml
name: blind-transfer
area: transfers
requires: [mock]                 # skipped unless the run provides every capability listed
setup: []                        # FreeSWITCH API commands over the event socket before ...
teardown: []                     # ... and after
backend:                         # routes added to the mock for this scenario only
  routes:
    transfer:
      steps:
        - answer: {}
        - tone: {hz: 1000, ms: 1000}
        - refer: {to: target}
target:                          # how the runner answers transfers (default: answer, 1800 Hz)
  steps: [{answer: {}}, {tone: {hz: 1800, ms: 3000}}, {wait: {ms: 30000}}]
options: {expectStatus: 200}     # optional OPTIONS check
calls:
  - id: a
    to: transfer                 # the user dialled; the mock picks its route by it
    startMs: 0                   # delay, so calls can overlap
    codecs: [pcmu, pcma]         # offered, in order; telephone-event is always offered
    headers: {X-Conversation-Id: 2b9e4c71}
    cancelAfterMs: 0             # >0: CANCEL if not answered by then
    steps:                       # after answer: tone, digits, wait, echo, hangup
      - tone: {hz: 600, ms: 7000}
      - hangup: {}
    maxDurationMs: 30000
    expect:
      status: 200
      responseHeaders: {}        # on the final response; "" means present with any value
      tonesHeard: [1000, 1800]
      tonesNotHeard: []
      digits: null               # e.g. "4321"
      endedBy: local             # local: the runner hung up; remote: the edge did
      codec: PCMU
      backend:                   # what the mock recorded (needs the mock)
        calls: 1                 # legs that reached it; 0 means none must have
        referAccepted: true
        endedBy: remote
      target:                    # what the transfer target recorded
        answered: true
        tonesHeard: [600]
        headers: {X-Conversation-Id: 2b9e4c71}
```

Capabilities a run provides are set with `-caps` (default `mock,esl,codec:pcmu,codec:pcma`): `mock` when
the backend is mockbackend, `esl` when the runner can reach FreeSWITCH's event socket, `codec:*` for codecs the
edge and runner both support.

## mockbackend

Routes live in `backends/mock.yaml` and are picked by the Request-URI user; unmatched users go to
`defaultRoute`, or get 404. Steps run in order:

| Step | Effect |
|---|---|
| `ring: {ms}` | 180, then wait; `ms: 0` rings until cancelled |
| `reject: {code, reason}` | final failure response; must be last |
| `earlyMedia: {hz, ms}` | 183 with SDP, playing a tone |
| `answer: {}` | 200 OK (after early media if any) |
| `tone: {hz, ms}` | play a tone |
| `digits: {digits, gapMs}` | send RFC 2833 events; put a `wait` first to send them at an offset |
| `wait: {ms}` | pause; the line carries silence |
| `echo: {ms}` | play back what is received |
| `refer: {to}` | REFER; a bare user is sent back to where the call came from, which is the edge |
| `hangup: {}` | BYE |

An answered call is held after its last step until the other end hangs up or `holdMs` passes. Throughout,
the mock records X- headers, the codec, digits and tones heard, who hung up, and REFER outcomes.

HTTP API (default `:8080`):

| | |
|---|---|
| `GET /healthz` | 200 when serving |
| `GET /calls[?benchCall=id]` | call records, oldest first |
| `POST /reset` | forget calls; return to the starting config |
| `GET /config`, `PUT /config` | read or replace the whole config (YAML or JSON) |
| `POST /routes` | add or replace routes (YAML or JSON map of name to route) |

## The edge configuration (`freeswitch/conf`)

A minimal SBC, deliberately small enough to read in one sitting:

- **Profiles.** `trunks` on 5060 faces far ends (and is where a SIP proxy such as Kamailio dispatches on a
  cluster). `backend` on 5080 faces the backend. RFC 2833 on both (payload type 101). Codecs PCMU and PCMA.
  `sip-options-respond-503-on-busy` makes OPTIONS answer 503 when the node is paused or full, so a dispatcher
  probe takes it out of rotation.
- **Dialplan.** `trunks` context: bridge `sofia/backend/<user>@$${backend_host}`, carrying X- headers.
  `inherit_codec` keeps the backend leg on the caller's codec.
- **Fallback.** When the bridge fails (the backend rejects, doesn't answer within `RING_TIMEOUT`, or can't be
  reached), the edge does not pass the failure back. It answers with `X-Fallback-Reason: <originate
  disposition>` (`USER_BUSY`, `CALL_REJECTED`, `NO_ANSWER`, ...), plays a tone and hangs up. A real deployment
  replaces the tone with a fallback number or announcement. Bench only: the user `unreachable` is routed to a
  TEST-NET address that never answers.
- **Transfers.** A REFER from the backend is ended at FreeSWITCH (`force_transfer_context=transfers`). FreeSWITCH
  answers 202, sends NOTIFY, releases the backend, and bridges the caller to
  `sofia/trunks/<refer-to user>@$${transfer_host}`. The trunk never sees a REFER, and the caller's X- headers
  ride the new leg.
- **Admission.** `max-sessions` in `switch.conf.xml` (1000), changed live with `fsctl max_sessions N`; past it,
  new INVITEs get 503. Each bridged call uses two sessions.
- **Calls from the backend** (`backend` context) are refused with 403 until outbound routing is designed.
- **Variables** come from the environment at start through `X-PRE-PROCESS cmd="exec-set"` (`bin/env.sh`):
  `BACKEND_HOST`, `TRANSFER_HOST`, `UNREACHABLE_HOST`, `RING_TIMEOUT`, `FALLBACK_TONE_HZ`, `FALLBACK_TONE_MS`,
  `ESL_LISTEN_IP`.
- **External address.** `ext-sip-ip`/`ext-rtp-ip` on `trunks` come from
  `/etc/freeswitch-operator/podinfo/external-address` (override the path with `FS_EXTERNAL_ADDRESS_FILE`), or this
  host's first IPv4 address when the file is absent (`bin/external-address.sh`; dual-stack hosts may list IPv6 first).
- **Event socket** on 8021 (password `ClueCon`, limited to private ranges by the `bench` ACL), on all addresses
  by default for the compose runner. On a cluster set `ESL_LISTEN_IP=127.0.0.1`: `fs_cli` and the operator's ready
  and drain scripts reach it there. Change the password before exposing it anywhere.

### Image

`drachtio/drachtio-freeswitch-mrf:0.9.6`, pinned by digest in `compose.yaml`. It's a public, multi-arch build of
FreeSWITCH 1.10.10 that runs natively on arm64. The image's own entrypoint rewrites its config, so compose runs
the binary directly against `freeswitch/conf`. The alternatives were worse: the common community image is
amd64-only, and SignalWire's official packages need an account token to download. The image has no G.722 module,
hence the G.722 skip.

## Conformance record

[`conformance/`](conformance/) records, case by case, what the edge should do according to published test plans
and standards (an i3 Forum interop plan, ETSI TS 102 027-2, RFC 4475) plus our own cases and impairment
profiles, and which scenario checks each one. A new scenario needs an entry there; `go test ./...` checks that.

## Known gaps

- **G.722** is untested: it needs a FreeSWITCH build with `mod_g722`, plus a G.722 codec in the runner (diago has
  none; a pure-Go encoder would do).
- **Early media** has a mock step (`earlyMedia`), but the runner doesn't listen before answer, so no scenario
  asserts it yet.
- Transfers requested by the **far end** (REFER from the trunk side) and attended transfers aren't covered.
- FreeSWITCH adds its own `X-FS-Support` header to legs it originates; a production edge may want to strip it.
- UDP only; no TLS/SRTP scenarios yet.
- The runner's caller drops early media and can't send re-INVITEs (hold/unhold).
- `sipgo` logs a harmless `UDP ref went negative` warning when the OPTIONS client closes.
- Each scenario starts by clearing the mock backend's call records, so two runners sharing one mock backend
  clear each other's. Run one at a time until records are kept per run (M2).

## Layout

```
cmd/sipbench        runner CLI: run, validate
cmd/mockbackend     mock backend
internal/runner     places calls, answers transfers, checks expectations, reports (summary, JSON, JUnit)
internal/uas        answering side shared by the mock and transfer targets; call records
internal/script     steps and the media engine (tones, digits, echo; continuous 20 ms RTP)
internal/tone       tone generator and Goertzel detector
internal/scenario   scenario files
internal/mock       mock config and HTTP API
internal/esl        minimal FreeSWITCH event socket client
freeswitch/conf     the edge configuration (freeswitch/kustomization.yaml: the same as ConfigMaps)
backends/mock.yaml  the mock's starting routes
scenarios/          the scenarios
conformance/        the conformance record: cases per source and the scenarios that check them
k8s/                the bench on a cluster: mock backend, transfer Service, runner Job
```
