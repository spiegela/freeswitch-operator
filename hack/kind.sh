#!/usr/bin/env bash
# The edge on kind: a KamailioCluster in front of a FreeswitchCluster, both run by this operator, with sipbench
# behind and in front.
#
#   hack/kind.sh up        create the cluster (deploy/kind/kind-config.yaml)
#   hack/kind.sh deploy    build and load the images, install MetalLB, the operator and the address agent, the
#                          edge's SIPAddressPool, apply deploy/kind/monitoring and deploy/kind, wait for both
#                          fleets and for every Kamailio to load the dispatcher list
#   hack/kind.sh bench [re] run sipbench through Kamailio; an optional regexp picks scenarios by name, and
#                          SIPBENCH_REPORT=<file> saves the run's JSON report there (for the report card)
#   hack/kind.sh status    both fleets, their members and the dispatcher list each Kamailio has loaded
#   hack/kind.sh down      delete the cluster
#
# Needs docker, kind and kubectl. Uses the kind cluster "freeswitch-operator" (EDGE_KIND_CLUSTER) and the namespace
# "telephony".
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="${EDGE_KIND_CLUSTER:-freeswitch-operator}"
NS=telephony
PROXY=proxy   # the KamailioCluster
KIND="${KIND:-kind}"
KUBECTL=(kubectl --context "kind-${CLUSTER}")
KUSTOMIZE="${ROOT}/bin/kustomize"

say() { printf '\n== %s\n' "$*"; }

up() {
  if "$KIND" get clusters | grep -qx "$CLUSTER"; then
    echo "kind cluster $CLUSTER already exists"
  else
    "$KIND" create cluster --config "$ROOT/deploy/kind/kind-config.yaml" --name "$CLUSTER" --wait 120s
  fi
}

# The third-party images the manifests pin by digest, as "<kind>\t<image>" lines.
pinned_images() {
  "$KUSTOMIZE" build "$ROOT/deploy/kind" \
    | awk '/^kind: / {kind = $2} /^---/ {kind = ""} $1 ~ /^-?image:$/ && $NF ~ /@sha256:/ {print kind "\t" $NF}' \
    | sort -u
}

# Pull each pinned image by digest inside the nodes that run it, so pods start without waiting on the registry.
# (kind load goes through docker save, which keeps the tag but not the multi-arch digest the manifests name, so
# the kubelet would pull again anyway.) FreeswitchCluster images go to the sip-edge nodes, the rest to sip-proxy.
pull_pinned() {
  local kind image role node
  while IFS=$'\t' read -r kind image; do
    role=sip-proxy
    [ "$kind" = FreeswitchCluster ] && role=sip-edge
    for node in $("${KUBECTL[@]}" get nodes -l "node-role.example.com/${role}=true" -o name); do
      echo "pulling $image on ${node#node/}"
      docker exec "${node#node/}" crictl pull "$image" >/dev/null &
    done
  done < <(pinned_images)
  wait
}

deploy() {
  say "building images"
  make -C "$ROOT" kustomize >/dev/null
  docker build -q -t freeswitch-operator:dev "$ROOT"
  docker build -q --target exporter -t freeswitch-exporter:dev "$ROOT"
  docker build -q -t sipbench:dev "$ROOT/sipbench"
  say "loading images into kind"
  "$KIND" load docker-image --name "$CLUSTER" freeswitch-operator:dev freeswitch-exporter:dev sipbench:dev
  say "pulling pinned images on the nodes"
  pull_pinned

  metallb

  say "installing the operator and the address agent"
  "$KUSTOMIZE" build "$ROOT/deploy/kind/operator" | "${KUBECTL[@]}" apply --server-side --force-conflicts -f -
  # A rebuilt image under the same tag needs a restart to be picked up.
  "${KUBECTL[@]}" -n freeswitch-operator-system rollout restart deployment/freeswitch-operator-controller-manager \
    daemonset/freeswitch-operator-address-agent
  "${KUBECTL[@]}" -n freeswitch-operator-system rollout status deployment/freeswitch-operator-controller-manager --timeout=120s
  "${KUBECTL[@]}" -n freeswitch-operator-system rollout status daemonset/freeswitch-operator-address-agent --timeout=120s
  wait_leader 120
  address_pool

  # M1 ran Kamailio from deploy/examples/kamailio, by hand; it holds the proxy nodes' SIP and RPC ports.
  "${KUBECTL[@]}" -n "$NS" delete deployment/kamailio service/kamailio configmap/kamailio-config \
    --ignore-not-found --wait=true

  say "applying monitoring (OpenTelemetry Collector, Prometheus with the alert rules)"
  "$KUSTOMIZE" build "$ROOT/deploy/kind/monitoring" | "${KUBECTL[@]}" apply --server-side --force-conflicts -f -
  "${KUBECTL[@]}" -n monitoring rollout restart deployment/otel-collector
  "${KUBECTL[@]}" -n monitoring rollout status deployment/prometheus --timeout=180s
  "${KUBECTL[@]}" -n monitoring rollout status deployment/otel-collector --timeout=180s

  say "applying the edge"
  "$KUSTOMIZE" build "$ROOT/deploy/kind" | "${KUBECTL[@]}" apply --server-side --force-conflicts -f -
  "${KUBECTL[@]}" -n "$NS" rollout restart deployment/mockbackend
  "${KUBECTL[@]}" -n "$NS" rollout status deployment/mockbackend --timeout=120s
  say "waiting for the fleet to be Ready and current"
  wait_fleet 600
  say "waiting for the proxies to be Ready and every Kamailio to load the dispatcher list"
  wait_proxy $(($("${KUBECTL[@]}" -n "$NS" get kamailiocluster "$PROXY" \
    -o jsonpath='{.spec.reloadDelaySeconds}') + 180))
  status
}

metallb() {
  say "installing MetalLB (deploy/kind/metallb)"
  "$KUSTOMIZE" build "$ROOT/deploy/kind/metallb" | "${KUBECTL[@]}" apply --server-side --force-conflicts -f -
  "${KUBECTL[@]}" -n metallb-system rollout status deployment/controller --timeout=180s
  "${KUBECTL[@]}" -n metallb-system rollout status daemonset/speaker --timeout=180s
}

# The edge's SIPAddressPool: four addresses (three edge nodes and a spare) at the top of kind's Docker network,
# which Docker hands out from the bottom. kind makes it an IPv4 /16.
pool_addresses() {
  local subnet
  subnet=$(docker network inspect kind -f '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' | grep -v : | head -n 1)
  case "$subnet" in
    *.0.0/16) ;;
    *) echo "kind's network is ${subnet:-unknown}; the pool's addresses assume an IPv4 /16" >&2; return 1 ;;
  esac
  local base="${subnet%.0.0/16}" i
  for i in 200 201 202 203; do echo "$base.255.$i"; done
}

# MetalLB's IPAddressPool and L2Advertisement for the pool's addresses, and the SIPAddressPool the edge draws from.
# The pool's addresses must be requested by name (autoAssign off), and are announced from the edge nodes only.
address_pool() {
  local addrs list cidrs i
  addrs=$(pool_addresses)
  list=$(echo "$addrs" | paste -s -d, -)
  cidrs=$(echo "$addrs" | sed 's|$|/32|' | paste -s -d, -)
  say "address pool edge-media: $list"
  # MetalLB's webhook may take a few seconds after its rollout to answer.
  for i in 1 2 3 4 5 6; do
    if "${KUBECTL[@]}" apply --server-side --force-conflicts -f - <<EOF; then break; fi
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: {name: sip-edge, namespace: metallb-system}
spec:
  addresses: [$cidrs]
  autoAssign: false
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata: {name: sip-edge, namespace: metallb-system}
spec:
  ipAddressPools: [sip-edge]
  nodeSelectors:
    - matchLabels: {node-role.example.com/sip-edge: "true"}
---
apiVersion: sip.instruments.com/v1alpha1
kind: SIPAddressPool
metadata: {name: edge-media}
spec:
  metalLB:
    ipAddressPool: sip-edge
    addresses: [$list]
EOF
    [ "$i" = 6 ] && return 1
    sleep 5
  done
}

# kamailio_rpc <pod> <method> makes one JSON-RPC call to a Kamailio pod from inside it: at the pod's own address,
# the only one JSON-RPC listens on, with the token the pod got from the <proxy>-jsonrpc Secret.
kamailio_rpc() {
  "${KUBECTL[@]}" -n "$NS" exec "$1" -c kamailio -- sh -c 'wget -T 5 -qO- \
    --header "Authorization: Bearer $KAMAILIO_RPC_TOKEN" \
    --post-data "{\"jsonrpc\":\"2.0\",\"method\":\"$0\",\"id\":1}" "http://$POD_IP:$KAMAILIO_RPC_PORT/RPC"' "$2"
}

# The operator asks Kamailio to reload only reloadDelaySeconds after it last wrote the list (so the kubelet has
# synced the mounted file), so a fleet that has just become Ready may not be in Kamailio yet. Wait until the
# KamailioCluster is Ready and current, has confirmed the reload, and every Kamailio pod has loaded exactly the
# destinations in its combined list, or the bench meets 503s.
wait_proxy() {
  local deadline=$((SECONDS + $1)) want have pod behind state
  while :; do
    state=$("${KUBECTL[@]}" -n "$NS" get kamailiocluster "$PROXY" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status} {.status.dispatcherHash} {.status.dispatcherReloadedHash} {.status.updatedReplicas}/{.spec.replicas}')
    want=$("${KUBECTL[@]}" -n "$NS" get configmap "$PROXY-dispatcher" -o jsonpath='{.data.dispatcher\.list}' \
      | awk '!/^#/ && NF { print $2 }' | sort)
    behind=
    set -- "$1" $state
    if [ "${2:-}" != True ] || [ -z "${3:-}" ] || [ "${3:-}" != "${4:-}" ] || [ "${5%/*}" != "${5#*/}" ]; then
      behind=" (KamailioCluster: Ready=${2:-?} hash=${3:-?} reloaded=${4:-?} updated=${5:-?})"
    fi
    for pod in $("${KUBECTL[@]}" -n "$NS" get pods -l "sip.instruments.com/kamailio-cluster=$PROXY" -o name); do
      have=$(kamailio_rpc "$pod" dispatcher.list 2>/dev/null \
        | sed -n 's/.*"URI":[[:space:]]*"\([^"]*\)".*/\1/p' | sort || true)
      [ -n "$want" ] && [ "$have" = "$want" ] || behind="$behind ${pod#pod/}"
    done
    [ -z "$behind" ] && return 0
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "the proxies are not ready with the dispatcher list after ${1}s:${behind}" >&2
      return 1
    fi
    sleep 5
  done
}

# Wait until the new operator pod holds the leader lease; until then the old one's status is all there is.
wait_leader() {
  local deadline=$((SECONDS + $1)) hash pod holder
  # The pod of the Deployment's newest ReplicaSet: the one rollout status just waited for (the old one may still
  # be terminating). Not the newest pod by creationTimestamp: that has one-second resolution, and on a fresh
  # cluster the first pod and its restarted replacement are created in the same second.
  hash=$("${KUBECTL[@]}" -n freeswitch-operator-system get replicasets -l control-plane=controller-manager \
    -o jsonpath='{range .items[*]}{.metadata.annotations.deployment\.kubernetes\.io/revision}{" "}{.metadata.labels.pod-template-hash}{"\n"}{end}' \
    | sort -n | tail -n 1 | cut -d' ' -f2)
  pod=$("${KUBECTL[@]}" -n freeswitch-operator-system get pods -l "control-plane=controller-manager,pod-template-hash=$hash" \
    -o jsonpath='{.items[0].metadata.name}')
  while :; do
    holder=$("${KUBECTL[@]}" -n freeswitch-operator-system get lease d927555f.instruments.com \
      -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
    case "$holder" in "${pod}_"*) return 0 ;; esac
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "operator pod $pod has not taken the leader lease after ${1}s (holder: ${holder:-none})" >&2
      return 1
    fi
    sleep 2
  done
}

# Ready alone can be the status from before the operator saw a new template (a rebuilt operator, a config edit),
# so also wait until both the operator and the StatefulSet controller have caught up and every member runs the
# StatefulSet's update revision.
wait_fleet() {
  local deadline=$((SECONDS + $1)) want have
  sleep 5
  while :; do
    want=$("${KUBECTL[@]}" -n "$NS" get statefulset edge \
      -o jsonpath='{.metadata.generation} {.status.updateRevision} {.spec.replicas} True')
    have=$("${KUBECTL[@]}" -n "$NS" get statefulset edge -o jsonpath='{.status.observedGeneration}')
    have="$have $("${KUBECTL[@]}" -n "$NS" get freeswitchcluster edge \
      -o jsonpath='{.status.updateRevision} {.status.updatedReplicas} {.status.conditions[?(@.type=="Ready")].status}')"
    [ "$have" = "$want" ] && return 0
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "fleet not ready and current after ${1}s (want: generation revision replicas Ready = $want; have: $have)" >&2
      return 1
    fi
    sleep 5
  done
}

status() {
  "${KUBECTL[@]}" get sipaddresspool edge-media \
    -o jsonpath='{range .status.addresses[*]}{.address}{"\t"}{.state}{"\t"}{.node}{"\tready="}{.bindAddressReady}{"\n"}{end}'
  "${KUBECTL[@]}" -n "$NS" get freeswitchcluster edge
  "${KUBECTL[@]}" -n "$NS" get pods -o wide -l app.kubernetes.io/name=freeswitch
  "${KUBECTL[@]}" -n "$NS" get freeswitchcluster edge \
    -o jsonpath='{range .status.members[*]}{.pod}{"\t"}{.node}{"\t"}{.address}{"\t"}{.state}{"\n"}{end}'
  "${KUBECTL[@]}" -n "$NS" get kamailiocluster "$PROXY"
  "${KUBECTL[@]}" -n "$NS" get kamailiocluster "$PROXY" \
    -o jsonpath='{range .status.members[*]}{.pod}{"\t"}{.node}{"\t"}{.address}{"\t"}{.state}{"\n"}{end}'
  echo "-- combined dispatcher list (ConfigMap $PROXY-dispatcher)"
  "${KUBECTL[@]}" -n "$NS" get configmap "$PROXY-dispatcher" -o jsonpath='{.data.dispatcher\.list}'
  echo "-- what each Kamailio has loaded (FLAGS: AP active and probing, IP inactive and probing)"
  local pod
  for pod in $("${KUBECTL[@]}" -n "$NS" get pods -l "sip.instruments.com/kamailio-cluster=$PROXY" -o name); do
    echo "$pod"
    kamailio_rpc "$pod" dispatcher.list \
      | grep -E '"URI"|"FLAGS"' | paste - - | sed 's/[[:space:]]\{2,\}/ /g' || echo "  (no answer)"
  done
}

bench() {
  local job filter="${1:-}" report="${SIPBENCH_REPORT:-}"
  local -a envs=()
  # Foreground: wait for the old runner pods too, or one still terminating would share the sipbench Service (and
  # its transfers) with the new one.
  "${KUBECTL[@]}" -n "$NS" delete job -l app.kubernetes.io/part-of=sipbench --ignore-not-found \
    --cascade=foreground --wait=true >/dev/null
  # An optional regexp picks scenarios by name; passed as SIPBENCH_RUN so kubectl does the quoting. With a report
  # wanted, SIPBENCH_JSON=- has the runner print it after its summary. (set env prints nothing when it changes
  # nothing, hence the plain cat without either.)
  if [ -n "$filter" ]; then envs+=("SIPBENCH_RUN=$filter"); fi
  if [ -n "$report" ]; then envs+=("SIPBENCH_JSON=-"); fi
  local manifest="$ROOT/sipbench/k8s/runner-job.yaml"
  job=$(if [ ${#envs[@]} -gt 0 ]; then
      "${KUBECTL[@]}" set env --local -o yaml -f "$manifest" "${envs[@]}"
    else
      cat "$manifest"
    fi | "${KUBECTL[@]}" -n "$NS" create -f - -o name)
  say "running $job"
  "${KUBECTL[@]}" -n "$NS" wait "$job" --for=jsonpath='{.status.active}'=1 --timeout=120s >/dev/null 2>&1 || true
  # The report is the last thing the runner prints, from a line holding only "{" to the end: it goes to the
  # report file, the rest to the screen.
  if [ -n "$report" ]; then : >"$report"; fi
  "${KUBECTL[@]}" -n "$NS" logs -f "$job" | grep -v -i 'ref went negative' \
    | awk -v out="$report" 'out != "" && /^\{$/ {json = 1} json {print > out; next} {print}' || true
  if [ -n "$report" ]; then
    if [ ! -s "$report" ]; then
      echo "no JSON report in the runner's log" >&2
      return 1
    fi
    say "report written to $report"
  fi
  if "${KUBECTL[@]}" -n "$NS" wait "$job" --for=condition=Complete --timeout=30s >/dev/null 2>&1; then
    return 0
  fi
  echo "bench failed" >&2
  return 1
}

down() { "$KIND" delete cluster --name "$CLUSTER"; }

cmd="${1:-}"
shift || true
case "$cmd" in
  up | deploy | bench | status | down) "$cmd" "$@" ;;
  *) sed -n '2,10p' "$0"; exit 2 ;;
esac
