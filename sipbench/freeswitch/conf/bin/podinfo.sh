#!/bin/sh
# Usage: podinfo.sh NAME [DEFAULT]
# Prints one of the addresses freeswitch-operator gives the pod, from its downward-API file (the path in
# FS_<NAME>_FILE, else /etc/freeswitch-operator/podinfo/<NAME>):
#   external-address      what FreeSWITCH advertises to peers for RTP (ext-rtp-ip)
#   bind-address          what it binds RTP to (rtp-ip), when its address pool gives it an address of its own on the node
# SIP needs neither: it advertises its own address (sip_profiles/trunks.xml).
# Without the file (not on a cluster, or no such address), prints DEFAULT, or else this host's first IPv4 address
# (dual-stack hosts, kind nodes among them, may list IPv6 first).
podinfo=/etc/freeswitch-operator/podinfo
external="${FS_EXTERNAL_ADDRESS_FILE:-$podinfo/external-address}"
bind="${FS_BIND_ADDRESS_FILE:-$podinfo/bind-address}"
case "$1" in
  external-address) f="$external" ;;
  bind-address) f="$bind" ;;
  *) echo "podinfo.sh: unknown name $1" >&2; exit 2 ;;
esac
if [ -s "$f" ]; then
  tr -d ' \t\r\n' < "$f"
elif [ -n "$2" ]; then
  printf '%s' "$2"
else
  hostname -i 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i !~ /:/) {print $i; exit}; print $1}'
fi
