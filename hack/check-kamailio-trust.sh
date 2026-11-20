#!/usr/bin/env bash
# Functional check of spec.trustedPeers (make check-kamailio-trust): Kamailio in the pinned image, with the
# trusted-peers config make check-kamailio-cfg renders, and a client on a Docker network that sends it an OPTIONS
# keepalive. From a trusted source the client gets an answer (503: there is no FreeSWITCH node); from an untrusted
# one, silence and one log line.
#   hack/check-kamailio-trust.sh <rendered config dir> <kamailio image>
set -euo pipefail
CFG_DIR="$1"
IMAGE="$2"
work=$(mktemp -d)
net=fso-trust-test
docker network rm "$net" >/dev/null 2>&1 || true
docker network create --subnet 172.30.0.0/24 "$net" >/dev/null
touch "$work/dispatcher.list"
# trusted: the docker network; untrusted: the rendered config as is (documentation ranges only)
sed 's|"198.51.100.0/30,203.0.113.8/29"|"172.30.0.0/24"|' "$CFG_DIR/trusted-peers.cfg" > "$work/trusted.cfg"
cp "$CFG_DIR/trusted-peers.cfg" "$work/untrusted.cfg"
msg='OPTIONS sip:172.30.0.10 SIP/2.0\r\nVia: SIP/2.0/UDP 172.30.0.20:5099;branch=z9hG4bK-trust-1\r\nFrom: <sip:t@172.30.0.20>;tag=1\r\nTo: <sip:172.30.0.10>\r\nCall-ID: trust-1\r\nCSeq: 1 OPTIONS\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n'
rc=0
for case in trusted untrusted; do
  docker run -d --rm --name fso-kam --network "$net" --ip 172.30.0.10 \
    -v "$work:/cfg:ro" -v "$work/dispatcher.list:/etc/freeswitch-operator/dispatcher/dispatcher.list:ro" \
    "$IMAGE" -m 128 -M 16 -f "/cfg/$case.cfg" -A PUBLIC_ADDR=172.30.0.10 -A PRIVATE_ADDR=172.30.0.10 >/dev/null
  sleep 3
  reply=$(docker run --rm --network "$net" --ip 172.30.0.20 busybox:1.37 \
    sh -c "printf '$msg' | nc -u -w 3 -p 5099 172.30.0.10 5060" 2>/dev/null | head -1 | tr -d '\r' || true)
  logs=$(docker logs fso-kam 2>&1 | grep -c "dropped OPTIONS from untrusted 172.30.0.20" || true)
  docker stop fso-kam >/dev/null
  echo "$case: reply='${reply:-<none>}' drop-log-lines=$logs"
  if [[ $case == trusted ]]; then [[ $reply == "SIP/2.0 503"* && $logs == 0 ]] || rc=1
  else [[ -z $reply && $logs == 1 ]] || rc=1; fi
done
docker network rm "$net" >/dev/null
rm -r "$work"
[[ $rc == 0 ]] && echo "trusted-peers functional check: ok" || echo "trusted-peers functional check: FAILED"
exit $rc
