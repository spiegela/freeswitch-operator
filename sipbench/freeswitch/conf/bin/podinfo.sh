#!/bin/sh
# Usage: podinfo.sh NAME [DEFAULT]
# Prints one of the addresses freeswitch-operator gives the pod, from its downward-API file (the path in
# FS_<NAME>_FILE, else /etc/freeswitch-operator/podinfo/<NAME>):
#   external-address      what FreeSWITCH advertises to peers for RTP (ext-rtp-ip)
#   bind-address          what it binds RTP to (rtp-ip), when its address pool gives it an address of its own on the node
#   external-sip-address  what it advertises for SIP (ext-sip-ip): the external address, unless there is a bind
#                         address other than its own (DEFAULT). The external address then reaches only the bind address,
#                         where SIP doesn't listen, so SIP advertises its own address, where Kamailio reaches it.
# Without the file (not on a cluster, or no such address), prints DEFAULT, or else this host's first IPv4 address
# (dual-stack hosts, kind nodes among them, may list IPv6 first).
podinfo=/etc/freeswitch-operator/podinfo
external="${FS_EXTERNAL_ADDRESS_FILE:-$podinfo/external-address}"
bind="${FS_BIND_ADDRESS_FILE:-$podinfo/bind-address}"
case "$1" in
  external-address) f="$external" ;;
  bind-address) f="$bind" ;;
  external-sip-address)
    b=""
    [ -s "$bind" ] && b=$(tr -d ' \t\r\n' < "$bind")
    if [ -n "$b" ] && [ "$b" != "$2" ]; then f=/dev/null; else f="$external"; fi ;;
  *) echo "podinfo.sh: unknown name $1" >&2; exit 2 ;;
esac
if [ -s "$f" ]; then
  tr -d ' \t\r\n' < "$f"
elif [ -n "$2" ]; then
  printf '%s' "$2"
else
  hostname -i 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i !~ /:/) {print $i; exit}; print $1}'
fi
