#!/bin/sh
# Prints the address FreeSWITCH advertises to peers (ext-sip-ip and ext-rtp-ip).
# On a cluster, freeswitch-operator writes it to a downward-API file. Elsewhere, use this host's own address.
f="${FS_EXTERNAL_ADDRESS_FILE:-/etc/freeswitch-operator/podinfo/external-address}"
if [ -s "$f" ]; then
  tr -d ' \t\r\n' < "$f"
else
  # The first IPv4 address, else the first of any: dual-stack hosts (kind nodes among them) may list IPv6 first.
  hostname -i 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i !~ /:/) {print $i; exit}; print $1}'
fi
