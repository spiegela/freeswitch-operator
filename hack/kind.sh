#!/usr/bin/env bash
# The edge on kind: Kamailio in front of a FreeswitchCluster run by this operator, with sipbench behind and in front.
#
#   hack/kind.sh up        create the cluster (deploy/kind/kind-config.yaml)
#   hack/kind.sh deploy    build and load the images, install the operator, apply deploy/kind, wait for the fleet
#   hack/kind.sh bench [re] run sipbench through Kamailio; an optional regexp picks scenarios by name
#   hack/kind.sh status    the fleet, its members and the dispatcher list Kamailio reads
#   hack/kind.sh down      delete the cluster
#
# Needs docker, kind and kubectl. Uses the kind cluster "freeswitch-operator" (EDGE_KIND_CLUSTER) and the namespace
# "telephony".
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLUSTER="${EDGE_KIND_CLUSTER:-freeswitch-operator}"
NS=telephony
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
  docker build -q -t sipbench:dev "$ROOT/sipbench"
  say "loading images into kind"
  "$KIND" load docker-image --name "$CLUSTER" freeswitch-operator:dev sipbench:dev
  say "pulling pinned images on the nodes"
  pull_pinned

  say "installing the operator"
  "$KUSTOMIZE" build "$ROOT/deploy/kind/operator" | "${KUBECTL[@]}" apply --server-side --force-conflicts -f -
  # A rebuilt image under the same tag needs a restart to be picked up.
  "${KUBECTL[@]}" -n freeswitch-operator-system rollout restart deployment/freeswitch-operator-controller-manager
  "${KUBECTL[@]}" -n freeswitch-operator-system rollout status deployment/freeswitch-operator-controller-manager --timeout=120s

  say "applying the edge"
  "$KUSTOMIZE" build "$ROOT/deploy/kind" | "${KUBECTL[@]}" apply --server-side --force-conflicts -f -
  "${KUBECTL[@]}" -n "$NS" rollout restart deployment/mockbackend
  "${KUBECTL[@]}" -n "$NS" rollout status deployment/mockbackend --timeout=120s
  "${KUBECTL[@]}" -n "$NS" rollout status deployment/kamailio --timeout=120s
  say "waiting for the fleet to be Ready"
  "${KUBECTL[@]}" -n "$NS" wait freeswitchcluster/edge --for=condition=Ready --timeout=300s
  status
}

status() {
  "${KUBECTL[@]}" -n "$NS" get freeswitchcluster edge
  "${KUBECTL[@]}" -n "$NS" get pods -o wide -l app.kubernetes.io/name=freeswitch
  "${KUBECTL[@]}" -n "$NS" get freeswitchcluster edge \
    -o jsonpath='{range .status.members[*]}{.pod}{"\t"}{.node}{"\t"}{.address}{"\t"}{.state}{"\n"}{end}'
  echo "-- dispatcher list (ConfigMap edge-dispatcher)"
  "${KUBECTL[@]}" -n "$NS" get configmap edge-dispatcher -o jsonpath='{.data.dispatcher\.list}'
  echo "-- what each Kamailio has loaded (FLAGS: AP active and probing, IP inactive and probing)"
  local pod
  for pod in $("${KUBECTL[@]}" -n "$NS" get pods -l app.kubernetes.io/name=kamailio -o name); do
    echo "$pod"
    "${KUBECTL[@]}" -n "$NS" exec "$pod" -- timeout 5 wget -qO- \
      --post-data='{"jsonrpc":"2.0","method":"dispatcher.list","id":1}' http://127.0.0.1:5070/RPC \
      | grep -E '"URI"|"FLAGS"' | paste - - | sed 's/[[:space:]]\{2,\}/ /g' || echo "  (no answer)"
  done
}

bench() {
  local job filter="${1:-}"
  # Foreground: wait for the old runner pods too, or one still terminating would share the sipbench Service (and
  # its transfers) with the new one.
  "${KUBECTL[@]}" -n "$NS" delete job -l app.kubernetes.io/part-of=sipbench --ignore-not-found \
    --cascade=foreground --wait=true >/dev/null
  # An optional regexp picks scenarios by name; passed as SIPBENCH_RUN so kubectl does the quoting. (set env
  # prints nothing when it changes nothing, hence the plain cat without a filter.)
  local manifest="$ROOT/sipbench/k8s/runner-job.yaml"
  job=$(if [ -n "$filter" ]; then
      "${KUBECTL[@]}" set env --local -o yaml -f "$manifest" "SIPBENCH_RUN=$filter"
    else
      cat "$manifest"
    fi | "${KUBECTL[@]}" -n "$NS" create -f - -o name)
  say "running $job"
  "${KUBECTL[@]}" -n "$NS" wait "$job" --for=jsonpath='{.status.active}'=1 --timeout=120s >/dev/null 2>&1 || true
  "${KUBECTL[@]}" -n "$NS" logs -f "$job" | grep -v -i 'ref went negative' || true
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
