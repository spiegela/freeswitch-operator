# Kamailio: the `KamailioCluster`

A `KamailioCluster` runs the proxy fleet the same way a `FreeswitchCluster` runs the media fleet: a StatefulSet
(`<name>-kamailio`) with one pod per node on the host network, a fixed public address each, the same one-at-a-time
roll, and the operator in charge of its config. `spec.dispatch` names the `FreeswitchCluster`s it sends calls to, in
priority order. Each must set `spec.dispatcher`, whose `setID` becomes that fleet's dispatcher set. See
[`config/samples`](../config/samples/sip_v1alpha1_kamailiocluster.yaml).

## Routing

The operator renders `kamailio.cfg` from a template ([`kamailio.cfg.tmpl`](../internal/controller/kamailio.cfg.tmpl))
into an immutable ConfigMap named by its hash. It routes:

- **A new call from a peer** to the first set in `spec.dispatch` with an active node, round-robin within it
  (`ds_select_routes`). It fails over on `503` or no answer to the set's next node, then to the next set.
- **Requests from a FreeSWITCH node** to the request URI, so peers always see Kamailio's address.
- **In-dialog requests** along their route set. One without Kamailio's route, or a CANCEL that matches nothing, gets
  `481` (a stray ACK is dropped).
- **OPTIONS keepalives from peers** (no user part), answered by Kamailio itself: `200` with an `Allow` header while
  any set has an active node, `503` while none has or while the pod drains, so peers route elsewhere.

**Timers.** Requests it relays wait RFC 3261's 32 s. A new call fails over after 5 s without any answer, and the
dispatcher's probes use the same 5 s. A `503` takes a node out of rotation at once. Silence alone doesn't, since a
node is also silent when it discards a request it can't parse, and one malformed INVITE would otherwise empty the
fleet. So a node that has stopped answering leaves rotation after two missed probes, 10 to 20 s. A `503` from the
last node is passed on as `503`.

## What it refuses

- **Untrusted sources,** with `spec.trustedPeers` set (each a named list of CIDRs). Only those sources and the
  fleet's own FreeSWITCH nodes get any answer. A request from anywhere else, in a dialog or not, is dropped before
  Kamailio answers anything, so a scanner learns nothing and a forged route set can't make Kamailio relay for a
  stranger. Empty trusts every source. It guards signaling only: carriers send media straight to FreeSWITCH, often
  from other addresses (Twilio signals from `/30`s per edge and sends media from `168.86.128.0/18`), so the firewall
  in front of the FreeSWITCH nodes' RTP range must admit those. The list is in the rendered config, so changing it
  rolls the fleet.
- **Headers FreeSWITCH would silently discard:** a From or To with a display name outside quotes holding anything
  but token characters (a comma, `@`, UTF-8), or a bare URI with a comma or question mark, gets `400`.
- **Unsupported extensions:** a request that requires one gets `420`.

It also inserts `Max-Forwards: 70` when missing and takes messages up to 64 KB over UDP and TCP (Kamailio's
defaults stop at 32 KB). Every refusal and silent drop is counted and logged ([metrics](metrics.md)).

`make check-kamailio-cfg` runs `kamailio -c` on rendered cases in the pinned image, in the lint workflow.
`make check-kamailio-trust` checks that a trusted source gets an answer and an untrusted one gets none.

## Membership is live

Each `FreeswitchCluster` publishes its own dispatcher list. The `KamailioCluster` combines its fleets' lists into
`<name>-dispatcher`, which its pods mount as a directory, with every destination flagged for probing (Kamailio's
probes take a dead node out on their own).

When a destination leaves, the operator marks it inactive on every ready Kamailio pod at once, by pod address. Once
`reloadDelaySeconds` have passed, it has each pod reload, reads each pod's list back, and reloads again every 10 s
until every pod holds the combined list (`status.dispatcherReloadedHash`). A fleet that is missing, has no
`spec.dispatcher`, or reuses another's `setID` is left out and named in the `SetConflict` condition.

## What restarts

Membership reloads live. Everything in the rendered config and the pod template restarts: listening ports, loaded
modules and their parameters, the routing script, the order of `spec.dispatch` (the sets are in the routing),
`spec.trustedPeers` and `spec.capture`. A change to one of those makes a new config snapshot and a new template, and the fleet rolls
one pod at a time. That is cheap, because a Kamailio drain is short.

## JSON-RPC is authenticated

Kamailio's JSON-RPC has no authentication of its own, and NetworkPolicy doesn't apply to host-network pods. So the
operator keeps a random token in a Secret (`<name>-jsonrpc`), the pods get it as an environment variable, and the
HTTP route answers `401` to any request without it. The RPC listener binds to the pod's own address only (on the
host network, the node's private address). Keep the port closed in the security group too. A `FreeswitchCluster`'s
`jsonrpcURL` remains for a Kamailio you run yourself, without a token.

## SIP capture

With `spec.capture` set, every pod sends a HEPv3 copy of each SIP message it sends or receives to a collector over
UDP (Kamailio's `siptrace`, mirroring at the network layer), for the [call tracer](call-tracing.md) or Homer. The
address may be a name, resolved as Kamailio sends; copies are never waited on. Give Kamailio its own `captureID`.
It is in the rendered config, so setting or changing it rolls the fleet. `make check-kamailio-capture` checks a
collector gets the copies.

## Drains

Kamailio holds transactions, not calls, so a drain is short (`drain.maxDrainSeconds`, default 40). The `preStop`
hook makes the pod unready and sets a shared variable over JSON-RPC, so new calls from peers get `503` (they retry
another Kamailio) while FreeSWITCH's own requests and in-dialog requests still pass. It then waits until `tm.stats`
shows no open transactions. Calls already up hold no transaction: their in-dialog requests follow the node's
address to the replacement pod. The dispatcher's own probes count as transactions, so with dead destinations a drain
can run to its limit.

## One Kamailio fleet by default

Per-peer behaviour belongs as data in one fleet, not a fleet per peer. Because a `FreeswitchCluster` publishes its
membership as a ConfigMap, more than one `KamailioCluster` can dispatch to the same FreeSWITCH fleet. That is for
isolation: a lane whose bad config or overload can't reach the other, or that upgrades on its own schedule.

## Not yet

- TLS.
- Rate limiting of scanners (`pike`).
- Refusing a source whose requests the nodes keep discarding. It needs known peers exempted, so a carrier isn't cut
  off for a few bad calls.
- Replies from the right address without a pool. Kamailio listens on `0.0.0.0` and advertises the public address,
  so a reply leaves from whatever address the kernel picks, unless its pool gives the pod a `bind-address` to listen
  on ([address pools](address-pools.md)).

Per-peer data and outbound registrations are on the [roadmap](../README.md#roadmap).
