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
- in-dialog requests along their route set;
- JSON-RPC over HTTP only on the internal port (`RPC_PORT`, 5070). Keep that port closed in the security group.

Preprocessor values are set with `-A`: `DS_SETID` (the fleet's dispatcher set), `PUBLIC_ADDR` (the node's public
address, advertised in `Via` and `Record-Route`) and `RPC_PORT`.

Check the config with the same image the Deployment uses:

```sh
docker run --rm -v "$PWD/kamailio.cfg:/etc/kamailio/kamailio.cfg:ro" \
  ghcr.io/kamailio/kamailio-ci:6.0-alpine -c -f /etc/kamailio/kamailio.cfg
```

Not yet covered here: TLS, rate limiting of scanners (`pike`), allow-listing peers, and registrations.
