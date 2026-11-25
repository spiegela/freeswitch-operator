#!/usr/bin/env bash
# Functional check of the name Kamailio gives FreeSWITCH (make check-kamailio-name): Kamailio in the pinned image,
# with a bind address of its own (peers on one Docker network, the FreeSWITCH nodes on another) and its pod name on
# the nodes' side, as start.sh runs it. A node stand-in sends it a new request toward a peer, then an in-dialog one
# routed to Kamailio's name. The peer must get both: the first with Kamailio's name in its Record-Route (so the
# dialog's route back runs through the name, not the node's address), the second with the route set consumed
# (Kamailio took its name as its own).
#   hack/check-kamailio-name.sh <rendered config dir> <kamailio image>
# KEEP=1 leaves the containers and networks behind, to look at.
set -euo pipefail
CFG_DIR="$1"
IMAGE="$2"
work=""
peers=fso-name-peers
nodes=fso-name-nodes
name=proxy-kamailio-0.proxy-kamailio.telephony.svc.cluster.local
public=203.0.113.10
# The port the rendered config listens on.
port=$(sed -n "s/^#!define SIP_PORT //p" "$CFG_DIR/one-set.cfg")
cleanup() {
  docker rm -f fso-kam fso-peer >/dev/null 2>&1 || true
  docker network rm "$peers" "$nodes" >/dev/null 2>&1 || true
  [ -z "$work" ] || rm -rf "$work"
}
[ -n "${KEEP:-}" ] || trap cleanup EXIT
cleanup
work=$(mktemp -d)
docker network create --subnet 172.30.0.0/24 "$peers" >/dev/null
docker network create --subnet 172.31.0.0/24 "$nodes" >/dev/null
printf '1 sip:172.31.0.20:5060 8 0 duid=edge-0\n' > "$work/dispatcher.list"

# Kamailio: the bind address on the peers' network, the pod's own address and name (a network alias, which Docker's
# DNS answers) on the nodes'.
docker create --name fso-kam --network "$peers" --ip 172.30.0.10 \
  -v "$CFG_DIR:/cfg:ro" -v "$work/dispatcher.list:/etc/freeswitch-operator/dispatcher/dispatcher.list:ro" \
  "$IMAGE" -m 128 -M 16 -f /cfg/one-set.cfg \
  -A PUBLIC_ADDR=$public -A PRIVATE_ADDR=172.31.0.10 -A BIND_ADDR=172.30.0.10 -A PRIVATE_NAME=$name >/dev/null
docker network connect --ip 172.31.0.10 --alias "$name" "$nodes" fso-kam
docker start fso-kam >/dev/null
# The peer prints whatever Kamailio sends it (one sender: busybox's nc keeps to the first; its stdin stays open).
docker run -d --name fso-peer --network "$peers" --ip 172.30.0.30 busybox:1.37 \
  sh -c 'sleep 10 | timeout 10 nc -u -l -p 5060' >/dev/null
sleep 3

send() {
  docker run --rm --network "$nodes" --ip 172.31.0.20 busybox:1.37 \
    sh -c "printf '$1' | nc -u -w 1 -p 5060 172.31.0.10 $port" >/dev/null 2>&1 || true
}
hdrs='From: <sip:edge@172.31.0.20>;tag=f1\r\nCall-ID: name-1\r\nMax-Forwards: 70\r\nContact: <sip:edge@172.31.0.20:5060>\r\n'
# A new request from a node: Kamailio records its route on both sides.
send "INVITE sip:peer@172.30.0.30:5060 SIP/2.0\r\nVia: SIP/2.0/UDP 172.31.0.20:5060;branch=z9hG4bK-name-1\r\n${hdrs}To: <sip:peer@172.30.0.30>\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n"
# In the dialog, the route set that Record-Route left, as a node that recovered the call sends it.
send "BYE sip:peer@172.30.0.30:5060 SIP/2.0\r\nVia: SIP/2.0/UDP 172.31.0.20:5060;branch=z9hG4bK-name-2\r\nRoute: <sip:$name:$port;lr;r2=on>,<sip:$public:$port;lr;r2=on>\r\n${hdrs}To: <sip:peer@172.30.0.30>;tag=t1\r\nCSeq: 2 BYE\r\nContent-Length: 0\r\n\r\n"
docker wait fso-peer >/dev/null
got=$(docker logs fso-peer 2>&1 | tr -d '\r')
invite=$(printf '%s\n' "$got" | sed -n '/^INVITE /,/^$/p')
bye=$(printf '%s\n' "$got" | sed -n '/^BYE /,/^$/p')
rc=0
if printf '%s\n' "$invite" | grep -q "^Record-Route: <sip:$name:$port;[^>]*lr" &&
  printf '%s\n' "$invite" | grep -q "^Record-Route: <sip:$public:$port;[^>]*lr"; then
  echo "new request: Record-Route names $name: ok"
else
  echo "new request: no Record-Route through $name and $public; the peer got:"; printf '%s\n' "${invite:-<nothing>}"; rc=1
fi
if [[ -n $bye ]] && ! printf '%s\n' "$bye" | grep -q '^Route:'; then
  echo "in-dialog request routed to $name: relayed, route set consumed: ok"
else
  echo "in-dialog request routed to $name: not relayed as Kamailio's own; the peer got:"; printf '%s\n' "${bye:-<nothing>}"; rc=1
fi
if [[ $rc != 0 ]]; then echo "Kamailio's log:"; docker logs fso-kam 2>&1 | tail -n 30; fi
[[ $rc == 0 ]] && echo "pod name functional check: ok" || echo "pod name functional check: FAILED"
exit $rc
