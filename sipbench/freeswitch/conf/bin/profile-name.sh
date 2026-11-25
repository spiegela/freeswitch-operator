#!/bin/sh
# Usage: profile-name.sh NAME
# Prints the name SIP profile NAME runs under on this node: NAME-<external address>, with every character but
# letters and digits made a dash (trunks-203-0-113-10), or NAME alone where the pod has no external address (not on
# a cluster, or a fleet whose externalAddress.source is None). The profile keeps NAME as an alias, so dial strings
# (sofia/NAME/...) and commands (sofia status profile NAME) work as before.
#
# Why: call tracking writes each call leg's profile name into the shared recovery table, and `sofia recover` takes
# over every row another FreeSWITCH wrote under the name of a profile it runs. With one name per address, a node
# that comes up with an address (its own after a restart, or a lost node's after failover) recovers exactly that
# address's calls, and never a live node's. See docs/call-tracking.md at the repository root.
f="${FS_EXTERNAL_ADDRESS_FILE:-/etc/freeswitch-operator/podinfo/external-address}"
if [ -s "$f" ]; then
  printf '%s-%s' "$1" "$(tr -d ' \t\r\n' < "$f" | sed 's/[^A-Za-z0-9]/-/g')"
else
  printf '%s' "$1"
fi
