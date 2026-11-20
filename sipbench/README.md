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

The [report card](report-card/README.md) records the latest release's runs on kind and in compose, and how much of
the [conformance record](conformance/README.md) the scenarios check. `sipbench report-card` writes it from the two
runs' JSON reports; `make report-card-run` at the repository root runs both benches and writes it, and CI does the
same after every release.

Each of `-edge`, `-mock`, `-esl`, `-esl-password`, `-target`, `-caps`, `-run` and `-json` takes its default from
`SIPBENCH_<FLAG>` (`SIPBENCH_ESL_PASSWORD` for `-esl-password`). `-json -` writes the report to stdout and the
summary to stderr. A variable set to the empty string counts as set,
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

Tones are detected with a Goertzel filter on 20 ms frames; a tone counts as heard after 200 ms. Each end also
records, per tone, the longest it went missing once it had started (`audioGapsMs`), so a far end that plays a tone
throughout can be checked for "audio both ways the whole time" with `maxAudioGapMs`.

Every call carries `X-Sipbench-Call: <run>-<scenario>-<call>-<n>`. The edge passes X- headers through (which is
itself under test), so the mock and the transfer target can file what they saw under the call that caused it.
`<run>` is fresh for every scenario run (8 random hex digits from the runner, then a counter), and the mock keeps
each run's routes and records apart: runners sharing one mock don't see or clear each other's. The mock also
records where each INVITE came from (`source`), which on a cluster is the edge node that sent it.

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
    startAfter: ""               # an action's name: start once that action is done (startMs counts from then)
    expect:
      status: 200
      responseHeaders: {}        # on the final response; "" means present with any value
      tonesHeard: [1000, 1800]
      tonesNotHeard: []
      maxAudioGapMs: 0           # >0: no tone in tonesHeard may go missing longer than this once started
      digits: null               # e.g. "4321"
      endedBy: local             # local: the runner hung up; remote: the edge did
      codec: PCMU
      mediaAddress: fleet        # the answer's SDP c= (audio's, else the session's): an IP, or fleet (see below)
      backend:                   # what the mock recorded (needs the mock)
        calls: 1                 # legs that reached it; 0 means none must have
        referAccepted: true
        otherNodeThan: ""        # another call's id: this one must have come from a different edge node
        endedBy: remote
      target:                    # what the transfer target recorded
        answered: true
        tonesHeard: [600]
        headers: {X-Conversation-Id: 2b9e4c71}
```

Capabilities a run provides are set with `-caps` (default `mock,esl,codec:pcmu,codec:pcma,sip-trace`): `mock` when
the backend is mockbackend, `esl` when the runner can reach FreeSWITCH's event socket, `codec:*` for codecs the
edge and runner both support, `proxy` when the edge address is Kamailio rather than a FreeSWITCH node, `fleet`, added by `-fleet namespace/name` (`SIPBENCH_FLEET`), when the runner
can act on the FreeswitchCluster under test, `netem` when the runner may impair its own traffic with tc (it
needs `NET_ADMIN`; compose and the kind Job provide both), `sip-trace` when the edge address is a SIP node's
own address, not a Service VIP, so a call can keep all its SIP on one socket (see Impairments below), and
`address-pool` when the FreeswitchCluster under test draws its external address from a SIPAddressPool.

### Raw messages: what sipgo won't send

The runner's SIP stack only builds well-formed requests. `raw:` sends hand-written messages to the edge byte for
byte, one exchange after another before the calls: Max-Forwards 0, a missing Call-ID, an unknown scheme or SIP
version, broken Content-Length, a CANCEL or response that matches nothing, legal but tortuous syntax.

```yaml
requires: [proxy, mock]          # proxy: the edge address is Kamailio (the kind runner)
raw:
  - id: invite-udp
    transport: udp               # or tcp
    waitMs: 3000                 # for a final response, or for silence (noResponse)
    message: |                   # LF becomes CRLF; a header-only message gets its closing blank line
      INVITE sip:agent@{{edge}} SIP/2.0
      Via: SIP/2.0/{{transport}} {{local}};branch={{branch}}
      Max-Forwards: 0
      From: <sip:sipbench@{{localHost}}>;tag={{tag}}
      To: <sip:agent@{{edge}}>
      Call-ID: {{callId}}
      CSeq: 1 INVITE
      X-Sipbench-Call: {{bench}}  # so the mock files anything that gets through under this exchange
      Content-Length: {{len}}      # the body's length, computed
    expect:
      status: 483                # the final response; or noResponse: true; or closed: true (tcp)
      provisional: [100]         # must arrive before the final response
      withinMs: 200              # the first response, from sending
      headers: {Unsupported: x}  # on the final response; the value must contain this ("" = present)
      copied: [Via, From, To]    # the first response carries these as sent (Via may gain received/rport)
      backend: {calls: 0}        # what the mock recorded under {{bench}}
```

Placeholders: `{{edge}}`, `{{local}}`, `{{localHost}}`, `{{localPort}}`, `{{transport}}`, `{{branch}}`,
`{{callId}}`, `{{tag}}`, `{{bench}}`, `{{len}}`; any other is an error. Over UDP the message is resent at 500 ms,
1 s and 2 s while nothing has come back, from an unconnected socket, since the edge may answer from another
address than the one it was sent to. An INVITE answered 2xx is acknowledged and hung up (`hangupAfterMs`, default
500), and the BYE must get 200; a non-2xx is acknowledged. Messages are written in our own words, never copied from
the RFC 4475 or ETSI texts.

### Actions: driving the fleet during a scenario

`actions:` is a timeline that runs alongside the calls, one action after another. Each waits for its triggers
(`afterAnswered: <call>`, `afterEnded: <call>`, then `afterMs`) and does one thing:

| Action | Effect |
|---|---|
| `drainNodeOf: <call>` | what `kubectl drain` does to the call's edge node: cordon it, evict the FreeSWITCH pod |
| `uncordon: {}` | uncordon every node this scenario cordoned |
| `rollFleet: {}` | set `SIPBENCH_ROLL` on the fleet's pod template, so the operator rolls every pod |
| `breakConfig: {configMap, key, replace or match, with}` | edit a ConfigMap the fleet reads: the first `replace` (literal text) or the first match of `match` (a Go regular expression; `with` may use `$1`) |
| `restoreConfig: {}` | put back every ConfigMap `breakConfig` edited |
| `waitMember: {of: <call>, state, timeoutMs}` | wait until the call's pod is `Active`, `Draining`, `Starting` or `Gone` |
| `waitFleetCurrent: {timeoutMs}` | wait until every pod runs the latest template and takes calls |
| `expectRollStuck: {forMs, timeoutMs}` | wait for a roll's first replacement, then fail if the roll goes further within `forMs`, more than one pod stops taking calls, or the replacement takes calls |
| `hangup: <call>` | the runner hangs the call up |

A call's pod is found from where the mock received the INVITE: the fleet runs on the host network, so that is a
member's address. Whatever happens, a scenario ends with its cordoned nodes uncordoned and its edited ConfigMaps
put back. Every action but `hangup` needs the `fleet` capability, and a scenario using them must say
`requires: [fleet]`. The runner's access is in `k8s/runner-rbac.yaml`. Results carry a `timeline` of what each
action did and when.

`expect.mediaAddress: fleet` also needs it: the address the edge's answer gives for media must be one of the
fleet's pods' external addresses, as the operator records them in each pod's `sip.instruments.com/external-address`
annotation (a SIPAddressPool's binding, such as an Elastic IP). They are read after the calls, so a pod replaced
during the scenario counts with its replacement's. Every answered call's result carries `mediaAddress`, checked
or not.

### Impairments: a bad network between the carrier and the edge

`impairments:` puts a scenario's calls behind a worse network, in the vocabulary of
[`conformance/impairment.yaml`](conformance/impairment.yaml):

```yaml
requires: [mock, netem, sip-trace]
impairments:
  - {link: carrier-edge, direction: both, traffic: rtp, delay: 40ms, jitter: 3ms, loss: 1%, burst: 2}
  - {link: carrier-edge, direction: both, traffic: all, loss: 100%, at: 5s, for: 2s}     # a 2 s blackhole
  - {link: carrier-edge, direction: from-edge, traffic: sip, drop: first 100 and first 180}   # needs sip-trace
  - {link: carrier-edge, direction: to-edge, traffic: sip, match: ACK, delay: 1s}
```

Two mechanisms, chosen by the knobs:

- **tc netem** for `loss`, `burst`, `delay`, `jitter`, `reorder` and `duplicate`, on the runner's own interface
  toward the edge: the runner is the carrier, so this is the carrier-edge link. A `prio` qdisc sorts packets by
  port into one band per impairment, and a `netem` qdisc hangs off each band while it is on. `traffic: sip` is UDP
  and TCP to or from the edge's SIP port, `rtp` is the runner's RTP ports (it keeps its media to 20480-24575 so
  one u32 mask matches them), `all` is both; DNS, the mock's API and the Kubernetes API are never touched.
  `from-edge` needs what arrives to be delayed too, and tc only shapes what leaves an interface: matching arrivals
  are redirected out of a veth pair (`sbnetem0`/`sbnetem1`) and come back in through its peer. (The ifb device does
  this usually; the Docker Desktop kernel, and so kind's, has none.) `burst` turns `loss` into Gilbert-Elliott
  loss with that mean run length. `at`/`for` turn an impairment on that long after the first call is answered, and
  off again. The runner removes everything when the scenario ends, and clears leftovers before it starts.
- **The runner's SIP layer** for `drop` and `match`: each call has its own UDP socket, and named messages are
  dropped (`drop: first INVITE`, `drop: every ACK`, `drop: first 200 to INVITE`) or held and lost
  (`match: ACK` with `delay` and `loss`) on their way in or out. Ordinals count per call. These need no
  privilege, but every request of the call, in-dialog ones included, must leave from that socket, and that needs
  the `sip-trace` capability, as `sip:` expectations do: the edge address must be a node's own. Behind a Service
  VIP (`kamailio-sip` on kind) the ACK and BYE go to the Kamailio pod's own address (its Record-Route), and from
  the INVITE's socket they collide in conntrack with the VIP's NAT entry and are dropped. So the kind Job's edge
  address is `proxy-kamailio:5060`, the headless Service, which resolves to each Kamailio node's own address, and
  it offers `sip-trace`.

tc needs the `netem` capability and `NET_ADMIN`: compose's runner has `cap_add: [NET_ADMIN]`, and the Job in
`k8s/runner-job.yaml` adds it to the container (it changes the runner pod's network namespace only). The image
runs as a non-root user, so `sipbench` carries `cap_net_admin` as a permitted file capability and hands it to `tc`
and `ip` as an ambient one. Other links (`proxy-node`, `edge-backend`) are part of the vocabulary and need the
`netem:<link>` capability, which no runner offers yet: those scenarios skip. Adding one means running the same
`tc` plan in that pod's network namespace.

With an impairment comes a set of measurements per call, in the report and as expectations:

| Expectation | Checks |
|---|---|
| `minToneCoverage: 0.95` | each tone in `tonesHeard` detected in at least that share of its 20 ms frames |
| `rtpLossPct: {min, max}` | RTP loss the caller measured on what it received, in percent |
| `maxSetupMs: 1200` | INVITE to final response |
| `echoMs: {min, max}` | round trip of an `echo` route's audio |
| `durationMs: {min, max}` | answer to end |
| `sip: [{out: INVITE, min: 2}, {in: 200 INVITE, max: 3, deliveredWithinMs: 1500, oneToTag: true}]` | (needs `sip-trace`) copies of a message on the caller's socket, those the SIP layer dropped included: how many, when the first was seen (`firstMs`, from just before the INVITE), how soon after it one got through, all with one To tag |
| `backend.tonesNotHeard` | tones the backend must not have heard |

`repeat: {count: 50, everyMs: 500, setupP95Ms: 1700}` on a call places it that many times (ids `a-1`, `a-2`, ...),
each with the call's expectations, and checks the setup time p95 across them. Results carry `impairments`, and
each call `setupMs`, `durationMs`, `toneCoverage`, `echoMs`, `rtp` (packets, expected, lost) and, with `sip`
expectations or SIP-layer impairments, `sip`: every message on the caller's socket and what was done to it.

To run the netem scenarios in compose beside another checkout, give the stack its own project and image:
`SIPBENCH_IMAGE=sipbench:netem docker compose -p sipbench-netem run --rm --use-aliases sipbench run -run netem- scenarios`.

### Metrics: counters the edge should move

`metrics:` lists counters a scenario should raise, each a PromQL query giving one number and the least it must rise
by: `{query: 'sum(kamailio_rejected_requests_total{reason="uri_scheme"})', increase: 4}`. No result counts as 0.
The runner queries the Prometheus HTTP API named by `-prometheus` (`SIPBENCH_PROMETHEUS`; on kind the Prometheus
the Collector sends to, see the top-level README). Before the scenario it waits for each value to hold still for
12 s, so a count from the scenario before can't land in this one; after it, it waits up to a minute for the rise.
Without `-prometheus` the checks are reported as not checked and don't fail the scenario. Results carry `metrics`:
each query's value before and after.

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
| `POST /runs/{run}/routes` | add or replace routes for one run's calls only (the runner does this per scenario) |
| `DELETE /runs/{run}` | forget one run's calls and routes (the runner does this when a scenario ends) |
| `POST /reset` | forget every call and run; return to the starting config |
| `GET /config`, `PUT /config` | read or replace the whole config (YAML or JSON) |
| `POST /routes` | add or replace routes for every run (YAML or JSON map of name to route) |

## The edge configuration (`freeswitch/conf`)

A minimal SBC, deliberately small enough to read in one sitting:

- **Profiles.** `trunks` on 5060 faces far ends (and is where a SIP proxy such as Kamailio dispatches on a
  cluster). `backend` on 5080 faces the backend. RFC 2833 on both (payload type 101). Codecs PCMU and PCMA.
  `sip-options-respond-503-on-busy` makes OPTIONS answer 503 when the node is paused or full, so a dispatcher
  probe takes it out of rotation.
- **Dialplan.** `trunks` context: bridge `sofia/backend/<user>@$${backend_host}`, carrying X- headers. A called
  number `lk<number>` is bridged as `<number>` to `$${livekit_host}` instead: LiveKit SIP, where an agent answers
  (the backend when `LIVEKIT_HOST` is unset).
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
  `BACKEND_HOST`, `LIVEKIT_HOST`, `TRANSFER_HOST`, `UNREACHABLE_HOST`, `RING_TIMEOUT`, `FALLBACK_TONE_HZ`,
  `FALLBACK_TONE_MS`, `ESL_LISTEN_IP`.
- **External address.** `ext-rtp-ip` on `trunks` comes from
  `/etc/freeswitch-operator/podinfo/external-address` (override the path with `FS_EXTERNAL_ADDRESS_FILE`), or this
  host's first IPv4 address when the file is absent (`bin/podinfo.sh`; dual-stack hosts may list IPv6 first). With a
  `bind-address` file (`FS_BIND_ADDRESS_FILE`) RTP binds that address (`rtp-ip`). `ext-sip-ip` is always
  `local_ip_v4`, where SIP listens and Kamailio reaches it. FreeSWITCH uses the `ext-` addresses only for peers outside the profile's
  `local-network-acl`, and every call on a cluster comes from Kamailio's private address, so `trunks` sets it to
  `nowhere`, an ACL that matches nothing (`acl.conf.xml`): every caller, sipbench inside the cluster too, is offered
  the external address. With the default (`localnet.auto`) an AWS pool's carriers were offered the private bind
  address, and their media never arrived.
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
- Runners can share a mock backend, but not a transfer target: FreeSWITCH sends transfers to one address
  (`TRANSFER_HOST`, the `sipbench` Service on a cluster), so concurrent runners each need their own. Scenarios that
  change the edge itself (setup commands, fleet actions) disturb whatever else runs on it.
- **Impairments**: tc works on the runner's interface, so one runner runs one impairment scenario at a time (the
  runner runs scenarios one after another). Only the carrier-edge link is impaired; RFC 3550 jitter, MOS (the
  E-model), SSRC and timestamp continuity, and loss as the backend measures it aren't measured yet. SIP-layer
  rules apply to calls, not to `raw:` messages or the OPTIONS check, and can't be timed with `at`/`for`.

## Layout

```
cmd/sipbench        runner CLI: run, validate
cmd/mockbackend     mock backend
internal/runner     places calls, answers transfers, checks expectations, reports (summary, JSON, JUnit)
internal/uas        answering side shared by the mock and transfer targets; call records
internal/script     steps and the media engine (tones, digits, echo; continuous 20 ms RTP)
internal/tone       tone generator and Goertzel detector
internal/scenario   scenario files
internal/fleet      the FreeswitchCluster scenario actions act on (client-go)
internal/rawsip     hand-written SIP messages over UDP and TCP, and a lenient parser for what comes back
internal/netem      impairments: the scenario vocabulary, tc plans, and the SIP layer's drops and holds
internal/mock       mock config and HTTP API
internal/esl        minimal FreeSWITCH event socket client
freeswitch/conf     the edge configuration (freeswitch/kustomization.yaml: the same as ConfigMaps)
backends/mock.yaml  the mock's starting routes
scenarios/          the scenarios
conformance/        the conformance record: cases per source and the scenarios that check them
k8s/                the bench on a cluster: mock backend, transfer Service, runner Job
```
