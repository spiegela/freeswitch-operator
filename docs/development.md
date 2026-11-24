# Development

Prerequisites: Go 1.26+, Docker, kind, kubectl. Kubebuilder v4 scaffolded this project; `make` installs the rest
of the tools it needs into `./bin`.

```sh
make test            # unit and envtest-based controller tests
make manifests       # regenerate CRDs and RBAC after changing api/ or markers
make run             # run the controller against your current kubeconfig
make docker-build IMG=<registry>/freeswitch-operator:<tag>
```

sipbench has its own instructions in [`sipbench/README.md`](../sipbench/README.md).

## The edge on kind

[`hack/kind.sh`](../hack/kind.sh) runs the whole edge locally, from [`deploy/kind`](../deploy/kind/):

- A kind cluster with two proxy nodes and three tainted edge nodes.
- The operator, built from this tree.
- A two-node `KamailioCluster` on the proxy nodes.
- A three-node `FreeswitchCluster` running sipbench's FreeSWITCH configuration on
  [`images/freeswitch`](../images/freeswitch/Dockerfile), with the metrics exporter beside it. Its media addresses
  come from a MetalLB `SIPAddressPool` (`edge-media`: MetalLB in L2 mode and the address agent, four addresses at
  the top of kind's Docker network).
- sipbench's mock backend behind it.
- A PostgreSQL holding the edge's core db and call tracking ([`deploy/kind/postgres`](../deploy/kind/postgres/postgres.yaml),
  set up as [call tracking](call-tracking.md) describes). `hack/kind.sh` brings it up before the edge, since
  FreeSWITCH with a DSN won't start until the database answers.
- In the `monitoring` namespace, an OpenTelemetry Collector sending everything over OTLP to a Prometheus that
  evaluates the alert rules (`kubectl -n monitoring port-forward svc/prometheus 9090` to look yourself). Scenarios
  can name counters the edge should move (`metrics:`), and on kind the runner checks them there.

```sh
make kind-up         # create the cluster (about 6 GB of Docker memory)
make kind-deploy     # build and load the images, install MetalLB and the operator, apply deploy/kind, wait for both fleets
make kind-bench      # run sipbench through Kamailio; hack/kind.sh bench <regexp> runs a subset
hack/kind.sh livekit # optional: LiveKit behind the edge's lk numbers, for the livekit scenarios (below)
hack/kind.sh status  # both fleets' members, the combined dispatcher list, and what each Kamailio has loaded
make kind-down
```

`hack/kind.sh livekit` adds, in the `livekit` namespace ([`deploy/kind/livekit`](../deploy/kind/livekit/)):

- Valkey and the LiveKit server.
- Our patched LiveKit SIP ([barge-in](barge-in.md#livekit-sip-needs-a-patch)). The image `livekit-sip:info-dev` is
  built from the `sip-info-to-room` branch of [spiegela/sip](https://github.com/spiegela/sip/tree/sip-info-to-room),
  or set `LIVEKIT_SIP_IMAGE`.
- sipbench's stand-in agent.

The edge sends `lk<number>` there (`LIVEKIT_HOST`). Once it is up, the bench runs the `livekit` scenarios, which
otherwise skip.

### What the bench covers on kind

Through Kamailio the bench passes everything but `codec-g722` (the image has no G.722) and `admission-limit` (it
needs the event socket of one particular node). On kind the runner also acts on the fleet (`SIPBENCH_FLEET`), so
fleet drills run as scenarios:

- **`drain-mid-call`:** cordon the call's node and evict its pod, as `kubectl drain` does. The pod leaves dispatch at
  once, a call placed meanwhile lands on another node, and the drained call keeps audio both ways until it ends.
- **`rolling-update-live-call`:** change the pod template with a call up. The operator replaces one pod at a time
  and, on reaching the call's pod, waits for it to drain; the call keeps audio both ways throughout.
- **`bad-config-stops-roll`:** move the SIP profile off the SIP port in a referenced ConfigMap. The first replacement
  never becomes ready and the roll stays there for 90 s while the rest of the fleet takes calls; putting the config
  back lets it finish.
- **`node-lost-mid-call`:** take a call's node off the network for two minutes and check the rest of the fleet keeps
  taking calls.

These take a few minutes and leave the fleet as they found it (nodes uncordoned, config restored), apart from the
`SIPBENCH_ROLL` variable `rollFleet` sets.

Checked by hand, not by a scenario: JSON-RPC without the token gets `401`; a deleted edge pod shows inactive on both
Kamailios within a second; a change to the rendered Kamailio config rolls the proxies one pod at a time; a draining
Kamailio answers new INVITEs with `503 Draining`.

## The report card

[`sipbench/report-card`](../sipbench/report-card/README.md) records the latest release's bench runs: through Kamailio
on kind, and against one FreeSWITCH in compose. After each push to `main`, the
[report card workflow](../.github/workflows/report-card.yml) runs both benches, regenerates the card and opens a pull
request into `dev` with it. To make one locally, on a cluster that `make kind-up kind-deploy` has set up:

```sh
make report-card-run   # run both benches, save their JSON reports in sipbench/report-card, write the card
make report-card       # rewrite the card from the saved reports (after editing the conformance record, say)
```
