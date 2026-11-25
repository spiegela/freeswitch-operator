# Call tracing

**The call tracer ([`cmd/call-tracer`](../cmd/call-tracer)) turns each call into one OpenTelemetry trace:** a span
for every SIP transaction on every hop, nested hop by hop, with LiveKit's room and its participants under the hop
that reached LiveKit. It reads copies of the SIP messages the edge's components capture (HEPv3, the format Homer
uses) and, optionally, LiveKit's webhooks, and sends finished spans to any OTLP/HTTP endpoint: Jaeger, Tempo,
VictoriaTraces, an OpenTelemetry Collector. Nothing in the edge waits on it: the copies go over UDP.

```
kamailio    call                         ───────────────────────────────────────────  answered, 62 s
kamailio      INVITE                     ──────  200 after 6.0 s (100 Trying, ACK)
kamailio        INVITE                   ─      no final response: failed over at 5 s
kamailio        INVITE to freeswitch      ────  200
freeswitch        INVITE                   ───  200
livekit             room                    ─────────────────────────────────────────
livekit               sip participant       ───────────────────────────────────────
livekit               agent participant       ─────────────────────────────────────  track published
kamailio      BYE                                                                 ─  200
```

## What goes in

- **Kamailio:** set `spec.capture` on the `KamailioCluster`. Every pod then sends a HEPv3 copy of each SIP message
  it sends or receives (Kamailio's `siptrace`, mirroring at the network layer). Setting it rolls the fleet.

  ```yaml
  spec:
    capture:
      address: call-tracer.monitoring.svc.cluster.local:9060  # host:port; a name resolves as Kamailio sends
      captureID: 1
  ```

- **FreeSWITCH:** sofia's own capture, in the configuration you give the `FreeswitchCluster`. sipbench's
  ([`sipbench/freeswitch/conf`](../sipbench/freeswitch/conf/)) reads it from the environment:
  `SIP_CAPTURE=yes` and `CAPTURE_SERVER=udp:<host>:9060;hep=3;capture_id=2`, which set `capture-server` in
  `sofia.conf.xml` and `sip-capture` on both profiles. Sofia resolves a host name once, when the profiles start.
- **The B2BUA's correlation header.** FreeSWITCH starts a new dialog, with a new Call-ID, for the leg it bridges
  to. The dialplan puts the caller's Call-ID on that leg as `X-CID` (`<action application="set"
  data="sip_h_X-CID=${sip_call_id}"/>`, as sipbench's `dialplan/trunks.xml` does), and the tracer adds a leg
  carrying it to that call. `-correlation-header` names another header.
- **LiveKit (optional):** point a webhook at `http://<call-tracer>:8080/livekit` and give the tracer LiveKit's API
  key and secret in `$LIVEKIT_KEYS` (`key: secret`, LiveKit's own format). Webhooks are checked against the key's
  signature, and those without a valid one are refused. A SIP participant is matched to its call by
  `sip.callIDFull`, the Call-ID of the leg that reached LiveKit SIP, or by an `X-CID` the trunk passes through as
  `sip.h.x-cid`. Everything else in the room (the agent, its tracks) follows the room.

Give each component its own capture ID and name it with `-capture-names 1=kamailio,2=freeswitch`; the names become
the spans' services.

## How a call becomes a trace

- **Trace ID:** the MD5 of the Call-ID the call arrived with (an MD5 is exactly a trace ID's 16 bytes). Anything
  that logs the caller's Call-ID can open the trace: `md5 -s "<Call-ID>"`.
- **A span per SIP transaction per hop:** from the request to its final response, with each provisional response
  and the ACK as events, and retransmissions counted. When both ends of a hop capture the same message, it is
  one span, owned by the end that captured it first (the sender); the tracer learns from that which addresses
  belong to which component, and names a span for the component it went to (`INVITE to freeswitch`).
- **Nesting:** a new INVITE goes under the INVITE that was still open when it started, so failover branches sit
  side by side under the caller's INVITE, and the leg FreeSWITCH bridges sits under the hop that reached it. A
  branch that never answered ends when the next one started. CANCELs go under the INVITE they cancel; BYEs and
  other requests in the dialog under the call.
- **The call span** (`call`) starts a microsecond before the call's first message, so trace lists (Grafana's
  Jaeger search) name the trace and give its duration from it rather than from the INVITE starting with it. It ends
  when the call does (a BYE answered, or the caller's INVITE refused or cancelled). It carries `call.outcome` (answered, refused, cancelled, no_answer), `call.answer_ms`,
  `call.talk_ms`, `call.agent_join_ms` (until an `AGENT` participant joined), `call.hangup` (caller or callee),
  `sip.from.user` and `sip.to.user`.
- **When spans go out:** each transaction's span about two seconds after its final response (so a call's setup is
  visible while it lasts); the rest a minute (`-grace`) after the call ends, for LiveKit's `participant_left` and
  `room_finished` to arrive. A call that never gets a final answer is closed after three idle minutes
  (`-setup-timeout`), and any call after four hours (`-max-call-age`). Spans still open then are marked
  (`sip.no_final_response`, `livekit.left_unseen`).

Keepalives, registrations and requests in dialogs the tracer never saw start are counted and dropped. A component
listening on a wildcard address (Kamailio on `0.0.0.0`) captures its own address as `0.0.0.0`; the tracer matches
that to the other end's copy by port.

## Running it

```sh
docker build --target call-tracer -t <registry>/call-tracer:<tag> .   # or ghcr.io/spiegela/call-tracer
```

```yaml
containers:
  - name: call-tracer
    image: ghcr.io/spiegela/call-tracer:<tag>
    args:
      - -otlp-endpoint=http://victoria-traces:10428/insert/opentelemetry/v1/traces  # or http://collector:4318/v1/traces
      - -capture-names=1=kamailio,2=freeswitch
    env:
      - name: LIVEKIT_KEYS
        valueFrom: {secretKeyRef: {name: livekit-keys, key: keys}}
    ports:
      - {name: hep, containerPort: 9060, protocol: UDP}
      - {name: http, containerPort: 8080}  # /livekit, /metrics, /healthz
```

Run one replica: a call's messages must all reach the same tracer. Behind a Service, the edge reaches it by name.
It keeps calls in memory (`-max-calls`, 20,000 by default) and loses those in progress when it restarts, flushing
them as it stops.

**Metrics** at `/metrics`: `call_tracer_hep_packets_total{result}`, `call_tracer_sip_messages_total{result}`,
`call_tracer_webhooks_total{result}`, `call_tracer_calls_total{end}`, `call_tracer_active_calls`,
`call_tracer_spans_exported_total`, `call_tracer_spans_dropped_total` and `call_tracer_export_errors_total`.

## Checked

- `make check-kamailio-capture` runs Kamailio with `spec.capture` and checks a collector gets HEPv3 copies with the
  configured capture ID. [`internal/calltrace/testdata`](../internal/calltrace/testdata) keeps two of those
  packets, which the decoder's test reads.
- `go test ./cmd/call-tracer` plays a call into a running tracer over UDP and HTTP. With `CALL_TRACER_TEST_OTLP`
  pointing at a real trace store (a VictoriaTraces in docker, say), the trace can be read back there.

## Not yet

- HEPv2, compressed HEP payloads and HEP over TCP.
- Media: RTCP-XR or RTP statistics as span events.
- More than one replica (sharding by Call-ID).
- Phone numbers are span attributes as they appear in From and To. Redact them in your collector if traces leave
  your network.
