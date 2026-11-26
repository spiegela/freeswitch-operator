# The FreeSWITCH fleet

A `FreeswitchCluster` runs one fleet of FreeSWITCH nodes: one pod per node on the host network, kept in Kamailio's
dispatcher list while it can take calls, drained before it goes away, and rolled one node at a time when anything
changes.

## The `FreeswitchCluster` resource

```yaml
apiVersion: sip.instruments.com/v1alpha1
kind: FreeswitchCluster
metadata:
  name: edge
  namespace: telephony
spec:
  replicas: 3                       # one pod per node; keep one spare for updates
  image: ghcr.io/spiegela/freeswitch@sha256:…   # any image with freeswitch and fs_cli; this one adds mod_pgsql
  nodeSelector:                     # pin to the edge node group
    node-role.example.com/sip-edge: "true"
  tolerations: []
  env: []                           # read by the configuration (vars.xml), e.g. call tracking's CORE_DB_DSN
  sip:
    port: 5060                      # the profile Kamailio dispatches to
  rtp:
    portRangeStart: 16384           # must match the node security group; with a pool's bind address both legs of a
    portRangeEnd: 32768             # call draw from that one address, so size it for twice the calls a node takes
  externalAddress:
    source: NodeExternalIP          # NodeExternalIP | AddressPool | None
    # addressPoolRef: {name: edge-media}   # with AddressPool: a SIPAddressPool (address-pools.md)
  config:
    mountPath: /usr/local/freeswitch/conf   # point FreeSWITCH here with -conf
    configMapRefs:                  # each ConfigMap's keys become files in its directory; an edit rolls the fleet
      - {name: edge-freeswitch-conf, path: "."}
      - {name: edge-freeswitch-profiles, path: sip_profiles}
  drain:
    maxDrainSeconds: 7200           # upper bound on waiting for live calls to finish
  # liveness: {}                    # opt-in hang detection (below)
  dispatcher:
    setID: 1                        # Kamailio dispatcher set for this fleet
    configMapName: edge-dispatcher  # the operator writes dispatcher.list here
    jsonrpcURL: ""                  # optional, for a Kamailio you run yourself (below)
  metrics:                          # optional: a Prometheus exporter beside FreeSWITCH in each pod (metrics.md)
    exporterImage: ghcr.io/spiegela/freeswitch-exporter@sha256:…
    port: 9282
  failover:                         # optional: health monitoring and failover to a spare node (below)
    mode: Detect                    # Detect (report only) | Automatic
    signalsRequired: 2              # independent signals that must fail together: 1 or 2
    failAfterSeconds: 15            # ...for this long before a member is Failed
    notReadySeconds: 20             # how long unready counts as a blip
    sipProbe: {periodSeconds: 5, timeoutSeconds: 2, failureThreshold: 3}
status:
  readyReplicas: 2
  members:                          # one entry per pod
    - pod: edge-0
      node: ip-10-0-1-23
      address: 10.0.1.23            # what Kamailio dispatches to (the node's private address)
      externalAddress: 203.0.113.10 # what peers see
      state: Active                 # Starting | Active | Draining
      health:                       # Healthy | Suspect | Failed, and the signals failing now
        state: Healthy
  lastFailover: {}                  # pod, node, trigger, reason, startedAt, completedAt, replacementNode
  currentRevision: edge-7c9d…
  updateRevision: edge-7c9d…
  conditions: []
```

[`config/samples`](../config/samples/) has a complete example, and [`deploy/kind/freeswitchcluster.yaml`](../deploy/kind/freeswitchcluster.yaml)
is the one the kind edge runs.

## Pods, service and disruption budget

The operator renders a StatefulSet with:

- `hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet`, so the pod uses the node's network but can still
  resolve cluster services.
- A required pod anti-affinity on the hostname, so there is never more than one FreeSWITCH per node (ports clash
  otherwise).
- `updateStrategy: OnDelete`, so the operator, not the StatefulSet controller, decides when a pod is replaced.
- `terminationGracePeriodSeconds` set from `drain.maxDrainSeconds`, so a draining pod isn't killed early.
- A `preStop` hook, a startup probe and a readiness probe from scripts the operator ships in a ConfigMap, so any
  image with `freeswitch` and `fs_cli` works:
  - **Startup** passes once FreeSWITCH is ready with a SIP profile running on `sip.port`, the port Kamailio sends
    to. (FreeSWITCH calls itself ready even with no profile loaded.) A pod that doesn't get there within two
    minutes is restarted. That costs nothing: it has never been ready, so it holds no calls.
  - **Readiness** adds "not draining" and, when required, "knows its external address". It keeps re-checking the
    profile, so a profile that stops later takes the node out of dispatch without touching its calls.
  - **Liveness is opt-in** (`spec.liveness`). A failure restarts the container and drops every call on the node,
    and `fs_cli` can be slow under load, so a false positive at peak would do more harm than the hang it guards
    against. A crash needs no probe: the process exits and the container restarts. With `liveness: {}` the probe
    fails only when FreeSWITCH doesn't answer `fs_cli status` at all (not ready, paused and draining all count as
    alive). It checks every 30 s with a 15 s timeout and restarts after 10 misses in a row, about five minutes of
    silence. `fs_cli` gives up two seconds before the probe does, so a slow answer is a counted miss, never a
    killed probe.
- A downward-API volume of the pod's annotations, through which the operator hands the pod its external address
  ([address pools](address-pools.md)).

It also creates a headless Service selecting the pods (readiness decides membership) and a PodDisruptionBudget
with `maxUnavailable: 1`.

## Dispatcher membership

**The headless Service's EndpointSlices are the source of truth: a ready endpoint takes new calls.** The operator
watches them and:

1. Renders Kamailio's `dispatcher.list` (one line per ready node, set `dispatcher.setID`) into the ConfigMap named
   by `dispatcher.configMapName`. A `KamailioCluster` that dispatches to the fleet reads it and drives its own pods
   ([Kamailio](kamailio.md)); a Kamailio you run yourself mounts it.
2. For a Kamailio you run yourself, if `dispatcher.jsonrpcURL` is set, calls Kamailio's JSON-RPC:
   `dispatcher.set_state` to take a draining node out at once, then `dispatcher.reload` once the new list should be
   on disk (`dispatcher.reloadDelaySeconds`, a guess at the kubelet's sync time). After each reload it reads the
   list back (`dispatcher.list`) and reloads again every 10 s until Kamailio holds the one it wrote. Kamailio's
   reload makes every destination active again, so the operator sets the ones Kamailio held inactive before it
   (failed probes, or a drain) inactive again after; the probes bring each back once it answers. When the URL
   names a Service in the cluster, each call goes to every ready Kamailio pod behind it, not just the one the
   Service would pick, so every Kamailio hears of each change. A Kamailio that starts later reads the current list
   from disk.

Kamailio's own OPTIONS probing (`ds_ping_interval`) stays on as a backstop. FreeSWITCH answers new calls with `503`
while paused, so the dispatcher fails over on a `503` in the gap between "not ready" and "Kamailio knows".

## Drains and rolling updates

**One mechanism serves both: a pod is drained by deleting it, and its `preStop` hook does the waiting.**

- `preStop` marks the pod as draining (readiness fails at once, so it leaves dispatch), runs `fsctl pause inbound`,
  and waits until FreeSWITCH holds no sessions (its own count, from `status`) or `maxDrainSeconds` passes. Not
  `show calls count`: that reads the core db, where a database core db can keep rows for calls long gone (a
  statement PostgreSQL refuses rolls back the others batched with it; [call tracking](call-tracking.md#prepare-the-database)
  prepares the database so it refuses none). If FreeSWITCH is still starting, it
  first waits (up to 60 s) for it to be ready: FreeSWITCH loses a SIGTERM that arrives while it starts, and the pod
  would then sit `Terminating` for its whole grace period.
- **Rolling update:** when the pod template changes, the operator replaces one pod at a time, and only while every
  other pod is ready and none is terminating: delete, wait for the replacement to pass readiness, move on. A
  replacement that never gets its SIP profile up stops the roll there, with the rest of the fleet still serving.
- **Node drain, autoscaler consolidation or node expiry:** eviction respects the PodDisruptionBudget, and the same
  `preStop` drain runs under the same long grace period.
- **Keep a spare.** With one pod per node, an update without a spare runs a node short for its whole length.

## Health and failover

**The operator watches every member through three independent signals, and can fence a Failed member's node and
start its pod on a spare.** Every fleet gets the health report, with `spec.failover` or without; acting on it is
`spec.failover.mode`.

| Signal | Fails when | Observer |
| --- | --- | --- |
| `NodeLost` | the node's Ready condition isn't True (the node lifecycle controller sets it Unknown once the kubelet stops renewing its lease, about 40 to 50 s), or the Node is gone | the control plane |
| `NotReady` | a pod that has been ready is unready for `notReadySeconds` (20) on a node that isn't lost: FreeSWITCH fails the kubelet's own checks, or keeps restarting | the kubelet |
| `SIPUnreachable` | `failureThreshold` (3) SIP OPTIONS in a row go unanswered, sent every `periodSeconds` (5) to the member's SIP port over the dispatcher transport | the operator |

- **Any SIP response counts as an answer.** A 403 from an ACL or a 503 while paused still shows a live SIP stack.
  The operator probes itself rather than reading Kamailio's dispatcher state, since a member that goes unready
  leaves the dispatcher list and Kamailio stops probing it. So the operator's pods must reach the edge nodes' SIP
  port, as Kamailio's do.
- **Only a pod that has been ready counts as `NotReady`** (the operator marks it `sip.instruments.com/first-ready`),
  and readiness doesn't count while the node is lost (that would be the node's loss twice).
- **A signal failing for every member that has been ready, when two or more have, is the observer's problem**
  (the operator's network, the control plane), not every member's at once: it is listed but not counted, and
  `FreeswitchHealthSignalBlind` warns. A member that has never been ready (starting, or crash-looping on a bad
  configuration) is left out of that test, so it can't hide another member's real failure.

A member with no failing signal is `Healthy`; one with any is `Suspect`; one with `signalsRequired` (2) counted
signals failing together for `failAfterSeconds` (15) is `Failed`. With 2, a node whose kubelet is lost but whose
FreeSWITCH still answers SIP stays Suspect, as does a member Kamailio still reaches but the operator can't. Each
member's verdict and signals are in `status.members[].health`, the fleet's `MembersHealthy` condition sums them up,
transitions are events on the fleet, and [metrics and alerts](metrics.md) follow them.

**Failover.** `spec.failover.mode` is `Detect` (the default: report, and act only when asked) or `Automatic`. A
person can fail over any member in either mode by annotating its pod `sip.instruments.com/fail-over=true` (any
other value, `false` included, does nothing). In `Automatic`, the operator fails over a `Failed` member on its own,
with these limits:

- One member at a time: the next waits until the last failover's replacement is ready, however long that takes. A
  replacement that never becomes ready most likely has nowhere to run, and failing another member over would only
  fence another node. `FreeswitchFailoverStuck` fires after 5 minutes; add a spare, or fail a member over by hand.
- Never a pod that has never been ready: a configuration or image that never worked fails the same way on any node.
- Never a pod that is shutting down (a drain, a rolling update, a scale-down) unless its node is lost: it is going
  anyway, and its node is likely sound.
- The earliest Failed first.

A failover:

1. Fences the member's node: the label `sip.instruments.com/fenced=true`, which marks it fenced, the taint
   `sip.instruments.com/fenced:NoSchedule`, which keeps other workloads off it, and the annotation
   `sip.instruments.com/fenced-by` saying which member's failover, when, and why.
2. Deletes the pod at once (no grace period, no drain: its calls are lost anyway). Its endpoint goes, so the
   dispatcher list drops it and Kamailio takes it out of rotation at once, as for a drain.
3. The StatefulSet starts the pod again. Every fleet's pods have a node affinity rule against the label, so it
   lands on the spare whatever the fleet's `tolerations` (a toleration with no key and `operator: Exists` would
   tolerate the taint).
4. Every `SIPAddressPool` that may fence takes its addresses off the fenced node and treats the node as failed
   ([address pools](address-pools.md)). The replacement gets an address on the spare as any replaced pod does: the
   failed member's own, freed by the fence, unless the spare holds an idle address of its own.

`status.lastFailover` records the failover until the replacement is ready (or the fleet is scaled down past it). It
is written before the node is fenced, and only over the copy of the fleet the decision was made from (one written
since may hold a failover still under way), so a failover is never started twice. If fencing or deleting fails, or
the operator restarts part way through, the next reconcile finishes the recorded failover, whatever the pod's health
by then. **The operator never unfences:** once someone has found out what happened to the node and it is sound,
removing the label and the taint makes it a spare again (the label is what the operator reads):

```bash
kubectl taint node <node> sip.instruments.com/fenced- && kubectl label node <node> sip.instruments.com/fenced-
```

The operator needs `patch` on nodes for the label and the taint.

Fencing a node takes every pool address off it, whatever fleet uses it: a Kamailio pod on a fenced node loses its
address too.

**Still to come: recovering the failed member's calls.** Nothing runs `sofia recover` on the replacement yet, so a
failed member's calls are lost (FreeSWITCH ends them at its media timeout, and nothing tells the backend); new calls
go elsewhere, within Kamailio's 5 s failover at first. [Call tracking](call-tracking.md#failover-what-tracking-is-for-and-whats-left)
has what recovery needs.

## Configuration from ConfigMaps

The operator assembles FreeSWITCH's configuration directory from ConfigMaps (applied from git by Flux, say). Each
reference names a directory under `config.mountPath` (`.` for the top, `autoload_configs`, `sip_profiles`, ...).
The operator lays the ConfigMaps' keys out there as one read-only projected volume, so a whole tree like
FreeSWITCH's own fits.

- **An edit rolls the fleet; it never lands in a running pod.** Pods never mount the ConfigMaps themselves: the
  kubelet would push an edit into every running pod within a minute, ahead of any drain. Instead the operator
  copies each one into an immutable ConfigMap named by a hash of its contents (`<cluster>-config-<hash>`), and each
  revision of the pod template mounts its own copies. An edit makes a new template, which is rolled through one
  drained node at a time, while pods not yet replaced keep the files they started with. Copies that no template or
  pod uses any more are deleted.
- **A bad reference stops everything.** A missing or empty ConfigMap, or two claiming the same path, marks the
  fleet `Degraded`, leaves the running pods alone and pauses any roll in progress. An empty one is nearly always an
  accident, and rolling it out could remove a whole directory, such as every SIP profile. To ship an empty
  directory on purpose, drop the reference.

sipbench's edge configuration ([`sipbench/freeswitch/conf`](../sipbench/freeswitch/conf/)) is the working example,
with [`sipbench/freeswitch/kustomization.yaml`](../sipbench/freeswitch/kustomization.yaml) turning it into
ConfigMaps.
