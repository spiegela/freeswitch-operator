#!/bin/sh
# Usage: bind-address.sh DEFAULT
# Prints the address FreeSWITCH binds RTP to (rtp-ip). When the pod's address pool gives it an address of its own on
# the node (a secondary private address an Elastic IP translates to), freeswitch-operator writes it to a
# downward-API file; otherwise DEFAULT, FreeSWITCH's own local_ip_v4.
f="${FS_BIND_ADDRESS_FILE:-/etc/freeswitch-operator/podinfo/bind-address}"
if [ -s "$f" ]; then
  tr -d ' \t\r\n' < "$f"
else
  printf '%s' "$1"
fi
