# Metrics and alerts

**Every component serves Prometheus metrics. An OpenTelemetry Collector scrapes them and sends them on over OTLP,
and alert rules shipped here watch them.** Nothing in the edge depends on them: they are for people and alerts.

## Kamailio

Each pod serves `/metrics` on `spec.metrics.port` (default 5071), on its own address only and without a token:
Kamailio's own statistics (`kamailio_tm_*`, `kamailio_sl_*`, `kamailio_core_*`, ...) and the edge's counters.

- `kamailio_rejected_requests_total{reason}`: refused before any node saw it. `display_name` (a From or To FreeSWITCH
  would discard), `sanity` (the sanity module's own `4xx`), `uri_scheme` (`416`), `untrusted_source` (dropped
  unanswered, with `spec.trustedPeers` set).
- `kamailio_node_silent_timeouts_total`: a node didn't answer within 5 s.
- `kamailio_silent_drops_total{cause}`: every node tried stayed silent and the caller got `408`.
  - `cause="request"`: a node answered something else meanwhile, so the nodes are up and discarded this request.
    It's a malformed request we don't refuse yet, or an attack.
  - `cause="nodes"`: no node answered anything; the fleet itself may be down.

Each refusal and silent drop is also logged with its source address and Call-ID; neither goes into a label.

## FreeSWITCH

With `spec.metrics` set on a `FreeswitchCluster`, each pod gets an exporter
([`cmd/freeswitch-exporter`](../cmd/freeswitch-exporter)). It reads FreeSWITCH over the event socket at each scrape
and serves `/metrics` on the pod's own address (default port 9282):

- `freeswitch_up`: the socket answered. Nothing else is reported when it doesn't, so a dead node never shows stale
  numbers.
- `freeswitch_ready`, `freeswitch_calls`, `freeswitch_channels`.
- Sessions: active, peak, total, the limit and the rate limit.
- `freeswitch_inbound_paused`: 1 while the node drains.
- Per SIP profile: its state, and its calls and failed calls in each direction.

The exporter runs as a native sidecar, so it keeps reporting while the node drains and stops only after FreeSWITCH
has. It is its own image, versioned apart from the operator: it is part of the pod template, so changing its image
rolls the fleet, which an operator upgrade alone never does. It reads the event socket password from
`spec.metrics.eventSocket.passwordSecretRef`, or uses FreeSWITCH's default as `fs_cli` does. It also relays
[barge-in](barge-in.md) detections.

## The operator

The operator serves controller-runtime's metrics (reconciles, work queues, the Go runtime) on its HTTPS metrics
endpoint (`:8443`, for callers allowed `GET /metrics`: bind the `metrics-reader` ClusterRole), plus its own. Each
names a fleet by `kind`, `namespace` and `name`:

- Fleet: `freeswitch_operator_fleet_members{state}` (Starting, Active, Draining), `_fleet_desired_replicas`,
  `_fleet_outdated_members` (above zero during a roll), `_fleet_condition{condition}`,
  `_fleet_pod_replacements_total`.
- Drains: `_fleet_drain_started_timestamp_seconds{pod}` and `_fleet_max_drain_seconds` (a drain's age against its
  limit), `_fleet_drain_duration_seconds` (how long each drain took).
- Dispatcher: `_dispatcher_destinations{set}`, `_dispatcher_list_updates_total`, `_dispatcher_reloads_total{result}`,
  `_dispatcher_set_inactive_total{result}`, `_dispatcher_pending_reload` (a changed list not every Kamailio is
  confirmed to hold).
- Address pools, named by `pool`: `freeswitch_operator_address_pool_addresses{state}`,
  `_address_pool_nodes_waiting`, `_address_pool_addresses_awaiting_agent` (bound addresses whose node's address agent
  hasn't reported the bind address), `_address_pool_provider_calls_total{call,result}`,
  `_address_pool_last_observed_timestamp_seconds`.

## Alert rules

The rules live in [`config/alerts/edge-alerts.yaml`](../config/alerts/edge-alerts.yaml), in Prometheus's rule-file
format, for Prometheus, Grafana Mimir or Amazon Managed Service for Prometheus. `make check-alerts` checks them and
runs their unit tests with promtool.

- **Critical:** silent drops with the whole fleet silent; FreeSWITCH not answering; a fleet with no member taking
  calls.
- **Warning:**
  - silent drops of single requests (malformed traffic or an attack), repeated silent node timeouts;
  - a SIP profile down, sessions near the limit;
  - a fleet short of members with nothing draining, a roll stalled with nothing draining, a drain past 80% of its
    limit;
  - a dispatcher reload pending for 5 minutes, a dispatch set conflict;
  - the operator gone;
  - a node using an address pool with none of its addresses, a pool address in `Conflict` or `Error`, a pool that
    hasn't read its provider for 15 minutes, a pool address waiting on its node's address agent.
- **Info:** a pool with no spare address.

## Scraping

The scrape config in [`deploy/kind/monitoring`](../deploy/kind/monitoring/collector.yaml) is the reference: pods with
a port named `metrics` in each fleet, and the operator's metrics Service. The rules expect `job` = `kamailio` or
`freeswitch`, and `namespace`, `pod` and `fleet` labels. On kind the Collector sends to a Prometheus with its OTLP
receiver on, which loads the rules. In production, point its OTLP exporter at your backend.
