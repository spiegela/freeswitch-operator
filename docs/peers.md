# Peers (planned)

None of this is built yet; it is the design for the peer features on the [roadmap](../README.md#roadmap). Today the
Kamailio fleet treats every trusted source the same ([Kamailio](kamailio.md)).

## Per-peer behaviour

Each peer (a carrier, a voice AI provider, a customer PBX) needs its own handling at the edge. A typed peer resource
carries it, and the operator renders it into the Kamailio fleet's config as data, with no route block written per
peer. What peers need in practice, and the Kamailio feature that covers each:

| Peer needs | Kamailio |
| --- | --- |
| Recognise the peer by its source address | `permissions` address table: address to peer tag |
| Several signaling addresses for one peer, each with its own health checks (some peers want none) | A dispatcher set per peer; the operator marks which destinations to probe (below) |
| A fixed Request-URI host (its termination hostname); numbers in E.164 | Per-peer rewrite rules (`dialplan` module) |
| A digest challenge the edge answers itself, rather than passing it back to the caller | `uac_auth()` with the peer's credentials, from a Secret |
| REFER handled at the edge, or passed on to the peer | Per-peer flag, sent to FreeSWITCH in a header (below) |
| Reached on the inside network (a VPN), with no NAT | [Inside network](#peers-on-the-inside-network) |
| Inbound calls only after registering | [Outbound registrations](#outbound-registrations) |

- **Everything reloads live.** Peer data lives in `db_text` tables the operator renders: `permissions` addresses,
  `dialplan` rules, `uac` registrations, and an `htable` for per-peer flags and digest credentials. Each reloads
  over JSON-RPC, so none of it restarts the fleet. `spec.trustedPeers` moves into the `permissions` table too.
  `uac_auth()` would read credentials from AVPs filled from that table, not from the static `credential` parameter.
  The module docs don't fully settle whether that works without the parameter, so sipbench's challenging peer mock
  has to confirm it, and confirm that `db_text` serves each module, before we rely on them.
- **Health checks are per destination.** Kamailio already runs `ds_probing_mode 3`, which probes only the
  destinations that carry the probing flag (`8`). The operator sets that flag on every FreeSWITCH node, and would
  set it on a peer's destinations unless that peer asked for no health checks.
- **Dispatcher set IDs don't collide.** A fleet's `spec.dispatcher.setID` stays the user's choice, limited to 1–99.
  Sets the operator creates (a peer's, or a fleet's inside set) take IDs from 100 up, recorded in the owning
  resource's status.
- **What FreeSWITCH needs to know travels in headers.** Kamailio removes any `X-Edge-*` header a peer sent, so a
  peer can't forge one. It then adds the peer's tag (`X-Edge-Peer`) and the flags FreeSWITCH acts on, such as
  `X-Edge-Refer: edge|pass`. FreeSWITCH's dialplan reads them. With `edge`, it sets `force_transfer_context` and
  places the transfer itself, as sipbench does today. With `pass`, it passes the REFER on to the peer.
- **FreeSWITCH keeps only its own config:** its profiles and the ACL of who may reach it, reloaded with `reloadxml`
  and `reloadacl` rather than a roll.

## Outbound registrations

Some carriers send inbound calls only to an address that has registered for the number, with a username and
password. The registration names one address (its `Contact`), so it belongs on the proxy, whose address is fixed,
not on a FreeSWITCH node that can drain or die. Many peers don't register at all: they recognise the edge by its
source address, or challenge each call. So this is built when the first peer needs it, as a field of that peer.

- Kamailio's `uac` module registers for each configured number and re-registers before expiry. The carrier then
  sends calls to Kamailio, which dispatches them like any other.
- Credentials come from a Kubernetes Secret. The operator renders the registrations table and reloads it over
  JSON-RPC.
- **Registration mode is per peer,** because carriers treat several contacts for one number differently: some try
  each one, some keep only the latest, some reject the second.
  - `single` (the default) registers from one Kamailio pod. When that pod drains, the operator moves the
    registration to another pod, in the order that carrier accepts: register the new contact first and then remove
    the old one, or remove the old one first and accept a short gap. sipbench's registrar mocks and a test call
    decide which order a carrier needs. If the registering pod fails instead of draining, the operator registers from
    another pod as soon as it sees the pod isn't ready. Registrations use a short expiry, so a carrier stops sending
    to a dead contact soon, even if nothing re-registers.
  - `perReplica` registers from every pod, so the carrier can fail over by itself. It's turned on per peer, once
    sipbench and a test call against that carrier show it accepts several contacts.
- A draining Kamailio pod that holds registrations hands them over before it goes.

## Peers on the inside network

Some peers reach the edge over a VPN into the VPC rather than over the internet, with no NAT on either side. For
them, both fleets have to use the nodes' internal addresses, for signaling and media alike. The public address that
FreeSWITCH advertises would send their RTP somewhere they can't reach.

- **FreeSWITCH:** a second SIP profile on the node's private address, advertising that address in SDP (no
  `ext-rtp-ip`). The operator publishes the fleet's members on that profile as a second dispatcher set.
- **Kamailio:** an inside listening address beside the public one. For an inside peer, Kamailio sends from it and
  dispatches to the fleet's inside set. A new listening address restarts the fleet; attaching the address touches
  the node anyway.
- **Network:** the VPN routes to the edge node subnet, and the security group opens SIP and the RTP range to the
  peer's networks. Peers allow-list the subnet, not node addresses, since nodes are replaced.
- **sipbench:** a peer mock on a second network checks that the SDP it receives carries an inside address and that
  audio flows both ways.

This stays in the one Kamailio fleet; an inside peer isn't a reason for a second one.
