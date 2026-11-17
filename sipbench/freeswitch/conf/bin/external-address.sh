#!/bin/sh
# Prints the address FreeSWITCH advertises to peers (ext-sip-ip and ext-rtp-ip).
# On a cluster, freeswitch-operator writes it to a downward-API file. Elsewhere, use this host's own address.
f="${FS_EXTERNAL_ADDRESS_FILE:-/etc/freeswitch-operator/podinfo/external-address}"
if [ -s "$f" ]; then
  tr -d ' \t\r\n' < "$f"
else
  hostname -i 2>/dev/null | awk '{print $1}'
fi
