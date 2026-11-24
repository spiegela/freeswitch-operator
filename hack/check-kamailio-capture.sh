#!/usr/bin/env bash
# Functional check of spec.capture (make check-kamailio-capture): Kamailio in the pinned image, with the capture
# config make check-kamailio-cfg renders pointed at a UDP listener on a Docker network. A client sends Kamailio an
# OPTIONS keepalive; the listener must get HEPv3 copies of the request and of Kamailio's answer, with the
# configured capture ID.
#   hack/check-kamailio-capture.sh <rendered config dir> <kamailio image>
# With HEP_OUT set, the captured packets are copied there (internal/calltrace/testdata holds one such capture).
set -euo pipefail
CFG_DIR="$1"
IMAGE="$2"
work=$(mktemp -d)
net=fso-capture-test
docker network rm "$net" >/dev/null 2>&1 || true
docker network create --subnet 172.31.0.0/24 "$net" >/dev/null
touch "$work/dispatcher.list"
sed 's|sip:call-tracer.monitoring.svc.cluster.local:9060|sip:172.31.0.30:9060|' "$CFG_DIR/capture.cfg" > "$work/capture.cfg"
msg='OPTIONS sip:172.31.0.10 SIP/2.0\r\nVia: SIP/2.0/UDP 172.31.0.20:5099;branch=z9hG4bK-capture-1\r\nFrom: <sip:t@172.31.0.20>;tag=1\r\nTo: <sip:172.31.0.10>\r\nCall-ID: capture-1\r\nCSeq: 1 OPTIONS\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n'
docker run -d --rm --name fso-hep --network "$net" --ip 172.31.0.30 -v "$work:/out" busybox:1.37 \
  sh -c 'timeout 10 nc -lu -p 9060 > /out/hep.bin' >/dev/null
docker run -d --rm --name fso-kam --network "$net" --ip 172.31.0.10 \
  -v "$work:/cfg:ro" -v "$work/dispatcher.list:/etc/freeswitch-operator/dispatcher/dispatcher.list:ro" \
  "$IMAGE" -m 128 -M 16 -f /cfg/capture.cfg -A PUBLIC_ADDR=172.31.0.10 -A PRIVATE_ADDR=172.31.0.10 >/dev/null
sleep 3
reply=$(docker run --rm --network "$net" --ip 172.31.0.20 busybox:1.37 \
  sh -c "printf '$msg' | nc -u -w 3 -p 5099 172.31.0.10 5060" 2>/dev/null | head -1 | tr -d '\r' || true)
sleep 1
docker stop fso-kam >/dev/null
docker wait fso-hep >/dev/null 2>&1 || true
rc=0
magic=$(head -c 4 "$work/hep.bin" 2>/dev/null || true)
copies=$(grep -ac 'capture-1' "$work/hep.bin" 2>/dev/null || true)
# chunk type 12 (capture agent ID, 4 bytes) holding 7: 0000 000c 000a 00000007
cid=$(od -An -tx1 -v "$work/hep.bin" | tr -d ' \n' | grep -c '0000000c000a00000007' || true)
echo "reply='${reply:-<none>}' magic='$magic' copies-with-call-id=$copies capture-id-7=$cid"
[[ $reply == "SIP/2.0 "* && $magic == HEP3 && $copies -ge 1 && $cid -ge 1 ]] || rc=1
[[ -n ${HEP_OUT:-} ]] && cp "$work/hep.bin" "$HEP_OUT"
docker network rm "$net" >/dev/null
rm -r "$work"
[[ $rc == 0 ]] && echo "capture functional check: ok" || echo "capture functional check: FAILED"
exit $rc
