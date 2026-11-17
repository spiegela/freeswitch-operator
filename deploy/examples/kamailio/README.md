# Example: Kamailio in front of a FreeswitchCluster

Kamailio holds the edge's fixed signaling addresses and spreads new calls across the FreeSWITCH fleet with its
dispatcher module. The operator keeps the dispatcher list in step with the fleet:

1. It writes `dispatcher.list` into the ConfigMap named by `spec.dispatcher.configMapName`. Kamailio mounts that
   ConfigMap as a directory (not with `subPath`, which never updates).
2. When `spec.dispatcher.jsonrpcURL` is set, it calls `dispatcher.set_state` to take a leaving node out at once, and
   `dispatcher.reload` once the kubelet has had time to write the new file (`reloadDelaySeconds`).

`kamailio.cfg` routes:
- requests from a peer to a FreeSWITCH node, round-robin, failing over when a node answers `503` (paused, draining)
  or doesn't answer;
- requests from a FreeSWITCH node to the request URI, so peers always see Kamailio's address;
- in-dialog requests along their route set; one that doesn't carry Kamailio's route, or a CANCEL that matches no
  transaction, gets `481` (a stray ACK is dropped);
- OPTIONS keepalives from peers (no user part), answered by Kamailio itself whatever host they name: `200` with an
  `Allow` header while any node is active, `503` while none is, so peers route elsewhere;
- JSON-RPC over HTTP only on the internal port (`RPC_PORT`, 5070). Keep that port closed in the security group.

It also inserts `Max-Forwards: 70` when missing and refuses requests that require an extension it doesn't support
(`420`). Transactions wait RFC 3261's 32 s, except a new call sent to a FreeSWITCH node, which fails over to the next
node after `DS_FR_TIMER` (5 s) without any answer. A `503` from the last node is passed on as `503`, the same answer a
peer gets when no node is up (Kamailio would otherwise turn it into `500`). The conformance record
(`sipbench/conformance`) tracks these against the standards.

Preprocessor values are set with `-A`: `DS_SETID` (the fleet's dispatcher set), `PUBLIC_ADDR` (the address advertised
in `Via` and `Record-Route`), `RPC_PORT` and `DS_FR_TIMER`. Always set `PUBLIC_ADDR`: Kamailio listens on `0.0.0.0`, and without
it `Record-Route` carries `0.0.0.0`, so no ACK or BYE can follow a call. The Deployment passes the public address
from `/etc/kamailio/public-address` when one is mounted, and otherwise the node's own (`status.hostIP`).

JSON-RPC is served over HTTP only (`jsonrpcs` `transport` 1). Kamailio's default leaves HTTP off, and
`jsonrpc_dispatch()` then drops each request without an answer.

`deploy/kind` runs this example on a local cluster in front of a `FreeswitchCluster`; see "The edge on kind" in the
top-level README.

Check the config with the same image the Deployment uses:

```sh
docker run --rm -v "$PWD/kamailio.cfg:/etc/kamailio/kamailio.cfg:ro" \
  ghcr.io/kamailio/kamailio-ci:6.0-alpine -c -f /etc/kamailio/kamailio.cfg
```

Not yet covered here: TLS, rate limiting of scanners (`pike`), allow-listing peers, and registrations.

Listening on `0.0.0.0` has one more cost: a reply leaves from whatever address the kernel picks for the destination,
not necessarily the address the request came in on. On kind a pod on Kamailio's own node gets replies from the node's
pod-bridge address. On a node with secondary addresses (the public address pool, M3) a peer could get replies from
the primary address instead of the one it called, which breaks its NAT and allow-lists. M3 should listen on each
address explicitly.
