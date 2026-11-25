#!/usr/bin/env python3
"""Writes the edge's Grafana dashboards: config/grafana/{kamailio,freeswitch,freeswitch-operator}.json.

The dashboards are generated, not edited in Grafana: change this file, run it (no arguments needed, or `make
dashboards`), and commit both. `make check-dashboards` (--check) fails if the JSON doesn't match what this file
writes.

    python3 hack/dashboards/dashboards.py [--check | <output directory>]

They read the series that docs/metrics.md describes, scraped the way deploy/kind/monitoring does it: `job` is
kamailio, freeswitch or freeswitch-operator, and Kamailio's and FreeSWITCH's series carry namespace, pod and fleet
(the KamailioCluster's or FreeswitchCluster's name, from the pod label app.kubernetes.io/instance). Pod CPU,
memory and restarts come from cAdvisor and kube-state-metrics, which any Kubernetes monitoring stack scrapes.
Each dashboard picks its Prometheus data source from a variable, and links to the other two with the variables
it has.

  Kamailio             the proxy: requests and replies, INVITE outcomes, transactions, what it refuses or loses,
                       shared memory, and the operator's view of its dispatcher list
  FreeSWITCH           the fleet: nodes taking calls, sessions against max-sessions and sessions-per-second, calls
                       and failures by SIP profile, profile and node state, the barge-in relay
  FreeSWITCH operator  the fleets as the operator runs them (members, conditions, rolls and drains), the dispatcher
                       list, address pools, and the controller itself
"""
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
OUT = ROOT / "config/grafana"
DS = {"type": "prometheus", "uid": "${datasource}"}
TAG = "freeswitch-operator"
OK, WARN, BAD = "green", "orange", "red"


class Dashboard:
    """Panels laid out in lines on Grafana's 24-column grid, under rows."""

    def __init__(self):
        self.panels = []
        self.y = 0
        self.ids = 0
        self.collapsed = None  # the collapsed row that takes the next panels, if any

    def nid(self):
        self.ids += 1
        return self.ids

    def row(self, title, collapsed=False):
        r = {"type": "row", "title": title, "collapsed": collapsed, "id": self.nid(), "panels": [],
             "gridPos": {"h": 1, "w": 24, "x": 0, "y": self.y}}
        self.panels.append(r)
        self.collapsed = r if collapsed else None
        self.y += 1

    def line(self, *ps):
        """Lays panels out left to right on one line, then moves below them."""
        x = 0
        for p in ps:
            p["id"] = self.nid()
            p["gridPos"].update(x=x, y=self.y)
            x += p["gridPos"]["w"]
            (self.collapsed["panels"] if self.collapsed else self.panels).append(p)
        self.y += max(p["gridPos"]["h"] for p in ps)


def targets(qs, instant=False, table=False):
    out = []
    for i, (expr, legend) in enumerate(qs):
        t = {"datasource": DS, "refId": chr(65 + i), "expr": expr, "legendFormat": legend}
        if instant:
            t.update(instant=True, range=False)
        if table:
            t["format"] = "table"
        out.append(t)
    return out


def steps(*pairs):
    """Threshold steps: steps(OK, (WARN, 0.7), (BAD, 0.9))."""
    first, *rest = pairs
    return {"mode": "absolute", "steps": [{"color": first, "value": None}] + [{"color": c, "value": v} for c, v in rest]}


def ts(title, qs, unit="short", w=8, h=8, desc="", stack=False, bars=False, overrides=(), min_=None, max_=None,
       decimals=None, legend="list", no_value=None):
    custom = {"lineWidth": 1, "fillOpacity": 70 if bars else (25 if stack else 10), "showPoints": "never",
              "spanNulls": False, "stacking": {"mode": "normal" if stack else "none", "group": "A"}}
    if bars:
        custom.update(drawStyle="bars", lineWidth=0)
    defaults = {"unit": unit, "custom": custom}
    if min_ is not None:
        defaults["min"] = min_
    if max_ is not None:
        defaults["max"] = max_
    if decimals is not None:
        defaults["decimals"] = decimals
    if no_value is not None:
        defaults["noValue"] = no_value
    lg = {"displayMode": legend, "placement": "bottom", "showLegend": True}
    if legend == "table":
        lg.update(calcs=["lastNotNull", "max"], placement="right")
    return {"type": "timeseries", "title": title, "datasource": DS, "description": desc,
            "gridPos": {"h": h, "w": w}, "targets": targets(qs),
            "fieldConfig": {"defaults": defaults, "overrides": list(overrides)},
            "options": {"legend": lg, "tooltip": {"mode": "multi", "sort": "desc"}}}


def stat(title, qs, unit="short", w=4, h=4, desc="", th=None, mappings=(), decimals=None, graph=False, text="auto"):
    defaults = {"unit": unit, "color": {"mode": "thresholds"}, "thresholds": th or steps("text"),
                "mappings": list(mappings)}
    if decimals is not None:
        defaults["decimals"] = decimals
    return {"type": "stat", "title": title, "datasource": DS, "description": desc,
            "gridPos": {"h": h, "w": w}, "targets": targets(qs, instant=not graph),
            "fieldConfig": {"defaults": defaults, "overrides": []},
            "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                        "colorMode": "value", "graphMode": "area" if graph else "none", "justifyMode": "auto",
                        "textMode": text, "orientation": "auto", "wideLayout": True, "showPercentChange": False}}


def bargauge(title, qs, unit="short", w=8, h=8, desc="", decimals=0, th=None):
    return {"type": "bargauge", "title": title, "datasource": DS, "description": desc,
            "gridPos": {"h": h, "w": w}, "targets": targets(qs, instant=True),
            "fieldConfig": {"defaults": {"unit": unit, "decimals": decimals, "min": 0,
                                         "color": {"mode": "thresholds"} if th else {"mode": "fixed", "fixedColor": "#5794F2"},
                                         "thresholds": th or steps("#5794F2")}, "overrides": []},
            "options": {"orientation": "horizontal", "displayMode": "gradient", "showUnfilled": True,
                        "valueMode": "color", "namePlacement": "left", "sizing": "manual", "minVizHeight": 16,
                        "maxVizHeight": 24, "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}}}


def mapping(*states):
    """Value mappings for a state: mapping((0, "Down", BAD), (1, "Up", OK))."""
    return [{"type": "value", "options": {str(v): {"text": t, "color": c, "index": i}}} for i, (v, t, c) in enumerate(states)]


UP_DOWN = mapping((0, "Down", BAD), (1, "Up", OK))


def timeline(title, qs, maps, w=8, h=8, desc="", overrides=()):
    return {"type": "state-timeline", "title": title, "datasource": DS, "description": desc,
            "gridPos": {"h": h, "w": w}, "targets": targets(qs),
            # A fixed base color: with thresholds, the timeline would show threshold ranges instead of the mappings.
            "fieldConfig": {"defaults": {"color": {"mode": "fixed", "fixedColor": "#8e8e8e"},
                                         "mappings": maps, "custom": {"fillOpacity": 80, "lineWidth": 0}},
                            "overrides": list(overrides)},
            "options": {"showValue": "never", "mergeValues": True, "rowHeight": 0.8, "alignValue": "left",
                        "legend": {"showLegend": False}, "tooltip": {"mode": "single"}}}


def table(title, qs, columns, w=24, h=6, desc="", overrides=()):
    """Instant queries merged into one row per label set; columns names each query's value column (by refId)."""
    rename = {f"Value #{chr(65 + i)}": c for i, c in enumerate(columns)}
    return {"type": "table", "title": title, "datasource": DS, "description": desc,
            "gridPos": {"h": h, "w": w}, "targets": targets(qs, instant=True, table=True),
            "transformations": [{"id": "merge", "options": {}},
                                {"id": "organize", "options": {"excludeByName": {"Time": True}, "renameByName": rename}}],
            "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}},
                            "overrides": list(overrides)},
            "options": {"showHeader": True, "cellHeight": "sm", "footer": {"show": False}}}


def by_name(name, *props):
    return {"matcher": {"id": "byName", "options": name}, "properties": list(props)}


def by_regex(rx, *props):
    return {"matcher": {"id": "byRegexp", "options": rx}, "properties": list(props)}


def dashed():
    return [{"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [10, 10]}}, {"id": "custom.fillOpacity", "value": 0}]


def fixed(color):
    return {"id": "color", "value": {"mode": "fixed", "fixedColor": color}}


def cell_color(th):
    return [{"id": "custom.cellOptions", "value": {"type": "color-background", "mode": "basic"}},
            {"id": "thresholds", "value": th}, {"id": "color", "value": {"mode": "thresholds"}}]


def var_ds():
    return {"type": "datasource", "name": "datasource", "label": "Data source", "query": "prometheus",
            "current": {}, "hide": 0, "refresh": 1, "regex": "", "options": []}


def var_query(name, label, query, multi=True, all_value=None, hide=0):
    v = {"type": "query", "name": name, "label": label, "datasource": DS,
         "query": {"query": query, "refId": f"{name}-var", "qryType": 1}, "definition": query,
         "refresh": 2, "sort": 1, "hide": hide, "multi": multi, "includeAll": multi, "current": {}, "options": [],
         "regex": ""}
    if all_value is not None:
        v["allValue"] = all_value
    return v


def write(d, uid, title, desc, tags, variables, out):
    dash = {
        "uid": uid, "title": title, "description": desc, "tags": [TAG] + tags,
        "editable": True, "graphTooltip": 1, "refresh": "30s", "schemaVersion": 41, "version": 1,
        "time": {"from": "now-6h", "to": "now"}, "timepicker": {}, "timezone": "browser",
        "templating": {"list": variables}, "annotations": {"list": []},
        "links": [{"type": "dashboards", "tags": [TAG], "title": "Edge", "asDropdown": False, "includeVars": True,
                   "keepTime": True, "icon": "external link", "targetBlank": False, "tooltip": "", "url": ""}],
        "panels": d.panels,
    }
    path = out / f"{uid.removeprefix('edge-')}.json"
    text = json.dumps(dash, indent=1) + "\n"
    if CHECK:
        if not path.exists() or path.read_text() != text:
            STALE.append(path)
        return
    path.write_text(text)
    print(f"{path}: {sum(1 + len(p.get('panels', [])) for p in d.panels)} panels")


def pod_resources(d, container, sel):
    """CPU, memory and restarts of one container in the selected pods (cAdvisor and kube-state-metrics)."""
    c = f'namespace=~"$namespace", pod=~"{sel}", container="{container}"'
    d.line(
        ts("CPU by pod", [(f'sum by (pod) (rate(container_cpu_usage_seconds_total{{{c}}}[$__rate_interval]))', "{{pod}}"),
                          (f'max(kube_pod_container_resource_limits{{{c}, resource="cpu"}})', "limit"),
                          (f'max(kube_pod_container_resource_requests{{{c}, resource="cpu"}})', "request")],
           unit="none", decimals=2, desc=f"Cores the {container} container uses, against its request and limit (when set).",
           overrides=[by_name("limit", *dashed(), fixed(BAD)), by_name("request", *dashed(), fixed("#8e8e8e"))]),
        ts("Memory by pod (working set)",
           [(f'sum by (pod) (container_memory_working_set_bytes{{{c}}})', "{{pod}}"),
            (f'max(kube_pod_container_resource_limits{{{c}, resource="memory"}})', "limit"),
            (f'max(kube_pod_container_resource_requests{{{c}, resource="memory"}})', "request")],
           unit="bytes", desc="The working set is what the kernel counts against the limit: at the limit, the "
                              "container is OOM-killed.",
           overrides=[by_name("limit", *dashed(), fixed(BAD)), by_name("request", *dashed(), fixed("#8e8e8e"))]),
        ts("Container restarts", [(f'sum by (pod, container) (increase(kube_pod_container_status_restarts_total'
                                   f'{{namespace=~"$namespace", pod=~"{sel}"}}[$__rate_interval]))', "{{pod}} {{container}}")],
           unit="none", bars=True, decimals=0, desc="Restarts of every container in the selected pods."),
    )


# ====================================================================================================== Kamailio ======
def kamailio(out):
    d = Dashboard()
    K = 'job="kamailio", namespace=~"$namespace", fleet=~"$fleet", pod=~"$pod"'

    def k(name, extra=""):
        return f'{name}{{{K}{", " + extra if extra else ""}}}'

    def r(name, w="$__rate_interval"):
        return f'sum(rate({k(name)}[{w}]))'

    def rp(name, w="$__rate_interval"):
        return f'sum by (pod) (rate({k(name)}[{w}]))'

    inv_final = [f'kamailio_core_rcv_replies_{c}xx_invite' for c in "23456"]
    d.line(
        stat("Pods scraped", [(f'sum(up{{{K}}})', "")], desc="Kamailio pods whose metrics answered the last scrape.",
             th=steps(BAD, (OK, 1))),
        stat("INVITEs", [(f'{r("kamailio_core_rcv_requests_invite", "5m")}', "")], unit="reqps", decimals=2,
             desc="New INVITEs received per second, last 5 minutes."),
        stat("INVITEs answered (ASR)",
             [(f'sum(increase({k("kamailio_core_rcv_replies_2xx_invite")}[$__range])) / '
               f'sum(increase({{__name__=~"{"|".join(inv_final)}", {K}}}[$__range]))', "")],
             unit="percentunit", decimals=1, th=steps(BAD, (WARN, 0.5), (OK, 0.8)),
             desc="Of the final replies to INVITE that Kamailio received from the nodes over the selected range, "
                  "the share that were 2xx: the answer-seizure ratio as the proxy sees it. A 487 (caller "
                  "cancelled) counts as unanswered, as it does for carriers. The 408s Kamailio sends itself when "
                  "no node answers aren't replies it received: see Requests lost."),
        stat("Transactions in progress", [(f'sum({k("kamailio_tmx_active_transactions")})', "")],
             desc="tm transactions not yet completed, across the selected pods."),
        stat("Shared memory used (worst pod)",
             [(f'max({k("kamailio_shmem_real_used_size")} / {k("kamailio_shmem_total_size")})', "")],
             unit="percentunit", decimals=1, th=steps(OK, (WARN, 0.7), (BAD, 0.85)),
             desc="Shared memory in use, fragmentation included, of what Kamailio was started with (-m). When it "
                  "runs out Kamailio can't create transactions and starts refusing requests."),
        stat("Requests lost", [(f'sum(increase({k("kamailio_silent_drops_total")}[$__range])) or vector(0)', "")],
             decimals=0, th=steps(OK, (BAD, 1)),
             desc="Requests the caller got a 408 for over the selected range, because every node tried stayed "
                  "silent (the edge's kamailio_silent_drops_total)."),
    )

    d.row("Requests and replies")
    methods = ["invite", "ack", "bye", "cancel", "options"]
    d.line(
        ts("Requests received by method",
           [(r(f"kamailio_core_rcv_requests_{m}"), m.upper()) for m in methods] +
           [(f'{r("kamailio_core_rcv_requests")} - ' + " - ".join(r(f"kamailio_core_rcv_requests_{m}") for m in methods),
             "other")],
           unit="reqps", stack=True, desc="Every request Kamailio received, from callers and from the nodes. OPTIONS "
                                          "include the dispatcher's own keepalive replies' counterparts from the nodes."),
        ts("Final replies to INVITE, by class", [(r(n), f"{c}xx") for c, n in zip("23456", inv_final)], unit="reqps", stack=True,
           overrides=[by_name("2xx", fixed(OK)), by_name("3xx", fixed("#8AB8FF")), by_name("4xx", fixed(WARN)),
                      by_name("5xx", fixed(BAD)), by_name("6xx", fixed("purple"))],
           desc="Final replies Kamailio received for INVITEs, from the nodes: how calls set up through the proxy "
                "ended."),
        ts("Replies received, by class", [(r(f"kamailio_core_rcv_replies_{c}xx"), f"{c}xx") for c in "123456"],
           unit="reqps", stack=True, desc="All replies, to every method, provisional included."),
    )
    codes = [400, 401, 403, 404, 407, 408, 480, 481, 483, 484, 486, 487, 488, 489, 491]
    d.line(
        bargauge("4xx replies by code (over the range)",
                 [(f'sum(increase({k("kamailio_core_rcv_replies_4xx")}[$__range]))', "all 4xx")] +
                 [(f'sum(increase({k(f"kamailio_core_rcv_replies_{c}")}[$__range])) > 0', str(c)) for c in codes], w=12, h=8,
                 desc="Which 4xx the nodes answered with: all of them, then each code seen in the range. 486/487 are a callee busy and a caller who hung up first; "
                      "404/484 a number no route matches; 408 no answer in time; 488 no common codec; 401/407/403 "
                      "authentication."),
        ts("Forwarded and dropped", [
            (r("kamailio_core_fwd_requests"), "requests forwarded"), (r("kamailio_core_fwd_replies"), "replies forwarded"),
            (r("kamailio_core_drop_requests"), "requests dropped"), (r("kamailio_core_drop_replies"), "replies dropped"),
        ], unit="reqps", w=12, h=8, overrides=[by_regex(".*dropped", fixed(BAD))],
           desc="Messages Kamailio sent on statelessly, and those its script dropped (drop or exit without a reply)."),
    )

    d.row("Transactions")
    d.line(
        ts("Transactions in progress", [
            (f'sum by (pod) ({k("kamailio_tmx_active_transactions")})', "{{pod}} active"),
            (f'sum by (pod) ({k("kamailio_tmx_inuse_transactions")})', "{{pod}} in use"),
        ], unit="none", desc="Active: not yet completed. In use: still in memory, completed ones waiting out their "
                             "timers included. In use climbing while active stays flat is transactions not being "
                             "freed."),
        ts("New transactions", [(r("kamailio_tmx_UAS_transactions"), "server (UAS): requests received"),
                                (r("kamailio_tmx_UAC_transactions"), "client (UAC): requests sent to nodes")],
           unit="reqps", desc="Kamailio forwards statefully: each request it relays opens a server transaction toward "
                              "the caller and a client transaction toward a node; failover to another node opens "
                              "another client transaction."),
        ts("Completed transactions, by final reply", [(r(f"kamailio_tmx_{c}xx_transactions"), f"{c}xx") for c in "23456"],
           unit="reqps", stack=True,
           overrides=[by_name("2xx", fixed(OK)), by_name("3xx", fixed("#8AB8FF")), by_name("4xx", fixed(WARN)),
                      by_name("5xx", fixed(BAD)), by_name("6xx", fixed("purple"))]),
    )
    d.line(
        ts("Replies handled by tm", [
            (r("kamailio_tmx_rpl_received"), "received"), (r("kamailio_tmx_rpl_relayed"), "relayed"),
            (r("kamailio_tmx_rpl_absorbed"), "absorbed"), (r("kamailio_tmx_rpl_generated"), "generated locally"),
        ], unit="reqps", w=12, h=7, desc="Absorbed: replies tm kept back (retransmissions, and the losers of a "
                                          "failover). Generated: replies Kamailio created itself, such as the 408 "
                                          "when no node answered."),
        ts("Replies Kamailio sends itself (stateless)", [
            (r(f"kamailio_sl_{c}xx_replies"), f"{c}xx") for c in "12456"] + [(r("kamailio_sl_failures"), "failed to send")],
           unit="reqps", w=12, h=7, stack=True,
           overrides=[by_name("failed to send", fixed(BAD), {"id": "custom.stacking", "value": {"mode": "none"}})],
           desc="Replies from the sl module: Kamailio's own answers outside a transaction (keepalive OPTIONS, a "
                "refusal before relaying)."),
    )

    d.row("Refused and lost")
    d.line(
        ts("Refused before any node saw it", [
            (f'sum by (reason) (rate({k("kamailio_rejected_requests_total")}[$__rate_interval]))', "{{reason}}")],
           unit="reqps", stack=True, no_value="None",
           desc="The edge's kamailio_rejected_requests_total. untrusted_source: dropped unanswered, the source isn't "
                "in spec.trustedPeers (scanners, mostly). sanity: the sanity module's own 4xx. uri_scheme: 416. "
                "display_name: a From or To a node would discard."),
        ts("Nodes that stayed silent", [
            (f'sum by (cause) (rate({k("kamailio_silent_drops_total")}[$__rate_interval]))', "408 to the caller: {{cause}}"),
            (f'sum by (pod) (rate({k("kamailio_node_silent_timeouts_total")}[$__rate_interval]))', "a node timed out ({{pod}})"),
        ], unit="reqps", overrides=[by_regex(".*: nodes", fixed(BAD))], no_value="None",
           desc="A node that doesn't answer in 5 s is tried no further for that request; the next node is. When "
                "every node tried stays silent the caller gets a 408. cause=nodes: no node answered anything (the "
                "fleet may be down); cause=request: the nodes are up and discarded this one request."),
        ts("Malformed and failed", [
            (r("kamailio_core_bad_URIs_rcvd"), "bad Request-URI"), (r("kamailio_core_bad_msg_hdr"), "bad header"),
            (r("kamailio_core_unsupported_methods"), "unsupported method"),
            (r("kamailio_core_err_requests"), "request errors"), (r("kamailio_core_err_replies"), "reply errors"),
            (r("kamailio_dns_failed_dns_request"), "DNS failed"), (r("kamailio_dns_slow_dns_request"), "DNS slow"),
        ], unit="reqps", desc="Messages Kamailio couldn't parse or route, and its DNS lookups that failed or took long."),
    )

    d.row("Memory and transport")
    d.line(
        ts("Shared memory used, by pod", [
            (f'sum by (pod) ({k("kamailio_shmem_real_used_size")}) / sum by (pod) ({k("kamailio_shmem_total_size")})', "{{pod}}"),
            (f'sum by (pod) ({k("kamailio_shmem_max_used_size")}) / sum by (pod) ({k("kamailio_shmem_total_size")})', "{{pod}} peak"),
        ], unit="percentunit", min_=0, max_=1, overrides=[by_regex(".* peak", *dashed())],
           desc="Shared memory holds transactions, the dispatcher list and the htable. Peak is the most used since "
                "start. Steady growth without traffic to match is a leak."),
        ts("Shared memory fragments, by pod", [(f'sum by (pod) ({k("kamailio_shmem_fragments")})', "{{pod}}")],
           unit="none", desc="Free fragments. A rising count with flat usage is fragmentation: large allocations can "
                             "fail with memory to spare."),
        ts("TCP", [
            (f'sum({k("kamailio_tcp_current_opened_connections")})', "connections open"),
            (f'sum({k("kamailio_tcp_current_write_queue_size")})', "bytes queued to send"),
            (f'sum(rate({{__name__=~"kamailio_tcp_(connect_failed|con_reset|con_timeout|send_timeout|sendq_full|local_reject)", {K}}}[$__rate_interval]))',
             "errors/s"),
        ], unit="none", overrides=[by_name("errors/s", fixed(BAD))],
           desc="SIP over TCP, when callers or nodes use it (UDP is the default). Errors: connects that failed, "
                "resets, timeouts, a full send queue, connections refused locally."),
    )

    d.row("Dispatcher (from the operator)")
    O = 'kind="KamailioCluster", namespace=~"$namespace", name=~"$fleet"'
    d.line(
        ts("Destinations, by set", [(f'sum by (name, set) (freeswitch_operator_dispatcher_destinations{{{O}}})',
                                     "{{name}} set {{set}}")], unit="none", decimals=0,
           desc="Nodes in the dispatcher list the operator last wrote. Draining nodes leave it."),
        ts("Dispatcher list updates and reloads", [
            (f'sum by (name) (increase(freeswitch_operator_dispatcher_list_updates_total{{{O}}}[$__rate_interval]))', "{{name}} list written"),
            (f'sum by (name, result) (increase(freeswitch_operator_dispatcher_reloads_total{{{O}}}[$__rate_interval]))', "{{name}} reload {{result}}"),
        ], unit="none", bars=True, decimals=0, overrides=[by_regex(".*reload error", fixed(BAD)),
                                                           by_regex(".*reload not_loaded", fixed(WARN))],
           desc="Each change to the fleet rewrites the list and asks every Kamailio to reload it. not_loaded: a "
                "Kamailio answered but still held the old list; retried."),
        timeline("Reload pending", [(f'max by (name) (freeswitch_operator_dispatcher_pending_reload{{{O}}})', "{{name}}")],
                 mapping((0, "Loaded", OK), (1, "Pending", WARN)),
                 desc="Pending: the list changed and not every Kamailio is confirmed to hold it. Calls may go to a "
                      "node that is leaving, or miss a new one."),
    )

    d.row("Pods")
    pod_resources(d, "kamailio", "$pod")

    variables = [
        var_ds(),
        var_query("namespace", "Namespace", 'label_values(up{job="kamailio"}, namespace)'),
        var_query("fleet", "KamailioCluster", 'label_values(up{job="kamailio", namespace=~"$namespace"}, fleet)', all_value=".*"),
        var_query("pod", "Pod", 'label_values(up{job="kamailio", namespace=~"$namespace", fleet=~"$fleet"}, pod)'),
    ]
    write(d, "edge-kamailio", "Kamailio", "The edge's SIP proxy: requests and replies, INVITE outcomes, transactions, "
          "what it refuses or loses, memory, and its dispatcher list.", ["kamailio", "sip"], variables, out)


# ==================================================================================================== FreeSWITCH ======
def freeswitch(out):
    d = Dashboard()
    F = 'job="freeswitch", namespace=~"$namespace", fleet=~"$fleet", pod=~"$pod"'

    def f(name, extra=""):
        return f'{name}{{{F}{", " + extra if extra else ""}}}'

    calls, failed = "freeswitch_sofia_calls_total", "freeswitch_sofia_failed_calls_total"
    d.line(
        stat("Nodes taking calls",
             [(f'count(({f("freeswitch_ready")} == 1) unless on (pod) ({f("freeswitch_inbound_paused")} == 1)) or vector(0)', "taking calls"),
              (f'count({f("freeswitch_up")})', "scraped")],
             th=steps("text"), text="value_and_name",
             desc="Nodes up and ready with inbound calls not paused (a draining node pauses them), of the nodes "
                  "scraped."),
        stat("Sessions", [(f'sum({f("freeswitch_sessions_active")})', "")], decimals=0,
             desc="Call legs up now. A call bridged through the edge is two sessions."),
        stat("Busiest node, of max-sessions",
             [(f'max({f("freeswitch_sessions_active")} / {f("freeswitch_sessions_limit")})', "")],
             unit="percentunit", decimals=1, th=steps(OK, (WARN, 0.6), (BAD, 0.8)),
             desc="Sessions on the busiest node against its max-sessions. Beyond the limit FreeSWITCH refuses "
                  "new calls."),
        stat("New sessions", [(f'sum(rate({f("freeswitch_sessions_total")}[5m]))', "")], unit="none", decimals=2,
             desc="New sessions per second across the fleet, last 5 minutes. Each node refuses sessions beyond its "
                  "sessions-per-second."),
        stat("Calls failed", [(f'sum(increase({f(failed)}[$__range])) / sum(increase({f(calls)}[$__range]))', "")],
             unit="percentunit", decimals=1, th=steps(OK, (WARN, 0.05), (BAD, 0.2)),
             desc="Of the calls through every SIP profile over the selected range, the share that failed. A failure "
                  "is any call that ended before it was answered for a reason other than the caller hanging up "
                  "(FreeSWITCH's own failed-calls count)."),
        stat("SIP profiles down", [(f'count({f("freeswitch_sofia_profile_up")} == 0) or vector(0)', "")], decimals=0,
             th=steps(OK, (BAD, 1)), desc="SIP profiles listed in a state other than RUNNING, across the nodes."),
    )

    d.row("Load")
    d.line(
        ts("Sessions by node", [(f'sum by (pod) ({f("freeswitch_sessions_active")})', "{{pod}}")], unit="none", decimals=0,
           desc="Call legs up on each node. Its max-sessions is in the Nodes table; the busiest node's share of it "
                "is at the top."),
        ts("New sessions per second by node", [
            (f'sum by (pod) (rate({f("freeswitch_sessions_total")}[$__rate_interval]))', "{{pod}}")],
           unit="none", decimals=2,
           desc="Session creation rate (calls per second, counting each leg). A burst over the node's "
                "sessions-per-second (in the Nodes table) is refused even with sessions to spare."),
        ts("Calls and channels", [(f'sum({f("freeswitch_calls")})', "bridged calls"),
                                  (f'sum({f("freeswitch_channels")})', "channels")],
           unit="none", decimals=0,
           desc="Channels are legs, bridged or not; a bridged call is two. Channels well above twice the bridged "
                "calls are legs ringing, in early media or not yet bridged."),
    )

    d.row("SIP profiles")
    d.line(
        ts("Calls by profile and direction",
           [(f'sum by (profile, direction) (rate({f(calls)}[$__rate_interval])) * 60', "{{profile}} {{direction}}")],
           unit="cpm", desc="On the edge, trunks carries calls from Kamailio (in) and backend the bridged leg (out)."),
        ts("Failed share by profile and direction",
           [(f'sum by (profile, direction) (rate({f(failed)}[$__rate_interval])) / '
             f'sum by (profile, direction) (rate({f(calls)}[$__rate_interval]))', "{{profile}} {{direction}}")],
           unit="percentunit", min_=0, max_=1,
           desc="Failures out on the backend profile mean the backend refused or didn't answer; failures in on "
                "trunks are calls the node refused itself (no route, over a limit, paused)."),
        timeline("Profiles running", [(f'min by (pod, profile) ({f("freeswitch_sofia_profile_up")})', "{{pod}} {{profile}}")],
                 mapping((0, "Not running", BAD), (1, "Running", OK)),
                 desc="A SIP profile that isn't RUNNING takes no calls on its port, though the node may be up."),
    )

    d.row("Nodes")
    d.line(
        table("Nodes", [
            (f'max by (pod, version) ({f("freeswitch_info")})', ""),
            (f'max by (pod) ({f("freeswitch_uptime_seconds")})', ""),
            (f'max by (pod) ({f("freeswitch_ready")} * (1 - {f("freeswitch_inbound_paused")}))', ""),
            (f'max by (pod) ({f("freeswitch_sessions_active")})', ""),
            (f'max by (pod) ({f("freeswitch_sessions_peak")})', ""),
            (f'max by (pod) ({f("freeswitch_sessions_limit")})', ""),
            (f'max by (pod) ({f("freeswitch_session_rate_limit")})', ""),
        ], ["info", "Uptime", "Taking calls", "Sessions", "Peak", "max-sessions", "sessions-per-second"], w=24, h=5,
            overrides=[by_name("info", {"id": "custom.hidden", "value": True}),
                       by_name("Uptime", {"id": "unit", "value": "s"}, {"id": "decimals", "value": 0}),
                       by_name("Taking calls", {"id": "mappings", "value": mapping((0, "No", BAD), (1, "Yes", OK))},
                               *cell_color(steps(BAD, (OK, 1))))],
            desc="Each node now: its FreeSWITCH version, how long it has run, whether it takes calls, and its "
                 "limits."),
    )
    d.line(
        timeline("Node state", [
            (f'min by (pod) ({f("freeswitch_up")})', "{{pod}} event socket"),
            (f'min by (pod) ({f("freeswitch_ready")})', "{{pod}} ready"),
            (f'max by (pod) ({f("freeswitch_inbound_paused")})', "{{pod}} inbound"),
        ], mapping((0, "No", BAD), (1, "Yes", OK)), w=24, h=7,
            overrides=[by_regex(".* inbound", {"id": "mappings", "value": mapping((0, "Open", OK), (1, "Paused (draining)", WARN))})],
            desc="Event socket: the exporter reached FreeSWITCH. Ready: FreeSWITCH says it is (before any SIP "
                 "profile runs). Inbound paused: the operator is draining the node, so it finishes its calls and "
                 "takes no new ones."),
    )

    d.row("Barge-in relay", collapsed=True)
    d.line(
        timeline("Relay subscribed", [(f'min by (pod) ({f("freeswitch_barge_in_relay_connected")})', "{{pod}}")],
                 mapping((0, "Not subscribed", BAD), (1, "Subscribed", OK)), w=8,
                 desc="The exporter's barge-in relay is subscribed to FreeSWITCH's voice-detector events "
                      "(docs/barge-in.md). Only with barge-in at the edge."),
        ts("Voice detections", [(f'sum by (party, event, result) (rate({f("freeswitch_barge_in_detections_total")}[$__rate_interval]))',
                                 "{{party}} {{event}} {{result}}")], unit="none", w=16,
           overrides=[by_regex(".* failed", fixed(BAD)), by_regex(".* no_target", fixed(WARN))],
           desc="Speech started and stopped, by party. relayed: sent to the agent as SIP INFO. no_target: no agent "
                "leg to send it on. failed: the INFO couldn't be sent."),
    )

    d.row("Pods")
    pod_resources(d, "freeswitch", "$pod")

    variables = [
        var_ds(),
        var_query("namespace", "Namespace", 'label_values(up{job="freeswitch"}, namespace)'),
        var_query("fleet", "FreeswitchCluster", 'label_values(up{job="freeswitch", namespace=~"$namespace"}, fleet)', all_value=".*"),
        var_query("pod", "Pod", 'label_values(up{job="freeswitch", namespace=~"$namespace", fleet=~"$fleet"}, pod)'),
    ]
    write(d, "edge-freeswitch", "FreeSWITCH", "The edge's FreeSWITCH fleet: load against its limits, calls and "
          "failures by SIP profile, and each node's state.", ["freeswitch", "sip"], variables, out)


# ======================================================================================================= operator =====
def operator(out):
    d = Dashboard()
    O = 'namespace=~"$namespace", name=~"$fleet"'
    J = 'job="freeswitch-operator"'
    P = 'pool=~"$pool"'

    def o(name, extra=""):
        return f'{name}{{{O}{", " + extra if extra else ""}}}'

    d.line(
        stat("Operator", [(f'max(up{{{J}}}) or vector(0)', "")], mappings=UP_DOWN, th=steps(BAD, (OK, 1)),
             desc="The operator's metrics endpoint answered the last scrape. Calls go on without the operator, but "
                  "nothing reacts to a node leaving or a change to a fleet."),
        stat("Fleets ready", [(f'sum({o("freeswitch_operator_fleet_condition", "condition=" + chr(34) + "Ready" + chr(34))})', "ready"),
                              (f'count({o("freeswitch_operator_fleet_desired_replicas")})', "fleets")],
             text="value_and_name", desc="Fleets whose Ready condition is True, of all the selected fleets."),
        stat("Members taking calls", [(f'sum({o("freeswitch_operator_fleet_members", "state=" + chr(34) + "Active" + chr(34))})', "active"),
                                      (f'sum({o("freeswitch_operator_fleet_desired_replicas")})', "desired")],
             text="value_and_name", desc="Active members across the selected fleets, against the replicas their specs "
                                         "ask for."),
        stat("Rolling", [(f'sum({o("freeswitch_operator_fleet_outdated_members")}) or vector(0)', "")], decimals=0,
             th=steps(OK, (WARN, 1)), desc="Members not yet on the latest pod template: above zero while a roll is "
                                           "under way."),
        stat("Draining", [(f'sum({o("freeswitch_operator_fleet_members", "state=" + chr(34) + "Draining" + chr(34))}) or vector(0)', "")],
             decimals=0, th=steps(OK, (WARN, 1)), desc="Members finishing their calls before they go."),
        stat("Dispatcher reload pending", [(f'sum({o("freeswitch_operator_dispatcher_pending_reload")}) or vector(0)', "")],
             decimals=0, th=steps(OK, (WARN, 1)),
             desc="KamailioClusters whose changed dispatcher list not every Kamailio is confirmed to hold."),
    )

    d.row("Fleets")
    cond = [f'max by (kind, namespace, name) ({o("freeswitch_operator_fleet_condition", "condition=" + chr(34) + c + chr(34))})'
            for c in ["Ready", "Available", "Progressing", "Degraded"]]
    state = [f'sum by (kind, namespace, name) ({o("freeswitch_operator_fleet_members", "state=" + chr(34) + s + chr(34))})'
             for s in ["Active", "Starting", "Draining"]]
    yes_no = mapping((0, "No", "transparent"), (1, "Yes", "transparent"))
    good = cell_color(steps(BAD, (OK, 1)))
    bad = cell_color(steps(OK, (BAD, 1)))
    d.line(
        table("Fleets", [(f'max by (kind, namespace, name) ({o("freeswitch_operator_fleet_desired_replicas")})', "")] +
              [(q, "") for q in state] +
              [(f'max by (kind, namespace, name) ({o("freeswitch_operator_fleet_outdated_members")})', "")] +
              [(q, "") for q in cond],
              ["Desired", "Active", "Starting", "Draining", "Outdated", "Ready", "Available", "Progressing", "Degraded"],
              h=7, overrides=[by_name(c, {"id": "mappings", "value": yes_no}, *(bad if c == "Degraded" else good))
                              for c in ["Ready", "Available", "Degraded"]] +
                             [by_name("Progressing", {"id": "mappings", "value": yes_no})],
              desc="Every fleet the operator runs, now: KamailioClusters and FreeswitchClusters."),
    )
    d.line(
        ts("Members by state", [(f'sum by (name, state) ({o("freeswitch_operator_fleet_members")})', "{{name}} {{state}}"),
                                (f'sum by (name) ({o("freeswitch_operator_fleet_desired_replicas")})', "{{name}} desired")],
           unit="none", decimals=0, overrides=[by_regex(".* desired", *dashed()), by_regex(".* Draining", fixed(WARN)),
                                               by_regex(".* Starting", fixed("#8AB8FF"))],
           desc="Starting: not ready yet. Active: takes calls. Draining: terminating, finishing its calls."),
        timeline("Conditions", [(f'max by (name, condition) ({o("freeswitch_operator_fleet_condition")})', "{{name}} {{condition}}")],
                 mapping((0, "False", BAD), (1, "True", OK)), w=16,
                 overrides=[by_regex(".* (Degraded|SetConflict)", {"id": "mappings", "value": mapping((0, "False", OK), (1, "True", BAD))}),
                            by_regex(".* Progressing", {"id": "mappings", "value": mapping((0, "False", "#8e8e8e"), (1, "True", "#8AB8FF"))})],
                 desc="Each fleet's status conditions. Degraded: fewer members take calls than asked for. "
                      "SetConflict (KamailioCluster): two fleets claim the same dispatcher set."),
    )

    d.row("Rolls and drains")
    d.line(
        ts("Rolls", [(f'sum by (name) ({o("freeswitch_operator_fleet_outdated_members")})', "{{name}} outdated"),
                     (f'sum by (name) (increase({o("freeswitch_operator_fleet_pod_replacements_total")}[$__rate_interval]))', "{{name}} replaced")],
           unit="none", decimals=0, overrides=[by_regex(".* replaced", {"id": "custom.drawStyle", "value": "bars"},
                                                         {"id": "custom.fillOpacity", "value": 70})],
           desc="Outdated: members not on the latest pod template. Replaced: pods the operator deleted to move them "
                "to it; each drains first."),
        ts("Drains in progress", [
            (f'time() - max by (name, pod) ({o("freeswitch_operator_fleet_drain_started_timestamp_seconds")})', "{{name}} {{pod}}"),
            (f'max by (name) ({o("freeswitch_operator_fleet_max_drain_seconds")}) and on (name) '
             f'count by (name) ({o("freeswitch_operator_fleet_drain_started_timestamp_seconds")})', "{{name}} limit")],
           unit="s", overrides=[by_regex(".* limit", *dashed(), fixed(BAD))],
           desc="How long each draining pod has drained, against its fleet's drain.maxDrainSeconds. At the limit "
                "the pod goes with whatever calls it still has."),
        bargauge("Drains over the range", [
            (f'sum by (name) (increase({o("freeswitch_operator_fleet_drain_duration_seconds_count")}[$__range]))', "{{name}} drains"),
            (f'histogram_quantile(0.5, sum by (name, le) (increase({o("freeswitch_operator_fleet_drain_duration_seconds_bucket")}[$__range])))', "{{name}} p50"),
            (f'histogram_quantile(0.95, sum by (name, le) (increase({o("freeswitch_operator_fleet_drain_duration_seconds_bucket")}[$__range])))', "{{name}} p95"),
        ], unit="s", decimals=0, desc="Drains that finished in the selected range and how long they took. A "
                                      "Kamailio drain takes seconds; a FreeSWITCH drain waits for its calls. (The "
                                      "count is shown in seconds' units; read it as a number.)"),
    )

    d.row("Dispatcher")
    K = 'kind="KamailioCluster"'
    d.line(
        ts("Destinations by set", [(f'sum by (name, set) ({o("freeswitch_operator_dispatcher_destinations", K)})', "{{name}} set {{set}}")],
           unit="none", decimals=0, desc="Nodes in each KamailioCluster's dispatcher list, by set."),
        ts("List writes and reloads", [
            (f'sum by (name) (increase({o("freeswitch_operator_dispatcher_list_updates_total", K)}[$__rate_interval]))', "{{name}} list written"),
            (f'sum by (name, result) (increase({o("freeswitch_operator_dispatcher_reloads_total", K)}[$__rate_interval]))', "{{name}} reload {{result}}"),
            (f'sum by (name, result) (increase({o("freeswitch_operator_dispatcher_set_inactive_total", K)}[$__rate_interval]))', "{{name}} mark inactive {{result}}"),
        ], unit="none", bars=True, decimals=0, overrides=[by_regex(".* error", fixed(BAD)), by_regex(".* not_loaded", fixed(WARN))],
           desc="The operator writes the list when a fleet changes, asks every Kamailio to reload it, and marks a "
                "leaving node inactive first so no new call goes to it."),
        timeline("Reload pending", [(f'max by (name) ({o("freeswitch_operator_dispatcher_pending_reload", K)})', "{{name}}")],
                 mapping((0, "Loaded", OK), (1, "Pending", WARN)),
                 desc="Pending: the list changed and not every Kamailio is confirmed to hold it yet."),
    )

    d.row("Address pools")
    d.line(
        ts("Addresses by state", [(f'sum by (pool, state) (freeswitch_operator_address_pool_addresses{{{P}}})', "{{pool}} {{state}}")],
           unit="none", decimals=0, stack=True,
           overrides=[by_regex(".* Free", fixed(OK)), by_regex(".* Bound", fixed("#5794F2")), by_regex(".* Idle", fixed("#8e8e8e")),
                      by_regex(".* (Conflict|Error)", fixed(BAD))],
           desc="Bound: on a node running the pool's pods. Free and Idle: spare. Conflict or Error: the provider "
                "disagrees with the pool; see the SIPAddressPool's status."),
        ts("Nodes and addresses waiting", [
            (f'sum by (pool) (freeswitch_operator_address_pool_nodes_waiting{{{P}}})', "{{pool}} nodes without an address"),
            (f'sum by (pool) (freeswitch_operator_address_pool_addresses_awaiting_agent{{{P}}})', "{{pool}} addresses awaiting the agent"),
        ], unit="none", decimals=0,
           desc="Nodes running a pod that uses the pool with none of its addresses, and bound addresses whose node's "
                "address agent hasn't reported them on the node's interface. Either keeps the node's pods from "
                "advertising a public address."),
        ts("Provider", [
            (f'sum by (pool, call, result) (increase(freeswitch_operator_address_pool_provider_calls_total{{{P}}}[$__rate_interval]))', "{{pool}} {{call}} {{result}}"),
            (f'time() - max by (pool) (freeswitch_operator_address_pool_last_observed_timestamp_seconds{{{P}}})', "{{pool}} since last read"),
        ], unit="none", decimals=0,
           overrides=[by_regex(".* error", fixed(BAD)),
                      by_regex(".* since last read", {"id": "unit", "value": "s"}, {"id": "custom.axisPlacement", "value": "right"}, *dashed())],
           desc="Calls to the cloud provider (observe, bind, unbind, prune), and how long since the pool last read "
                "it. It rereads every 5 minutes."),
    )

    d.row("Controller")
    d.line(
        ts("Reconciles by controller and result",
           [(f'sum by (controller, result) (rate(controller_runtime_reconcile_total{{{J}}}[$__rate_interval]))', "{{controller}} {{result}}")],
           unit="ops", overrides=[by_regex(".* error", fixed(BAD))]),
        ts("Reconcile time, p99", [(f'histogram_quantile(0.99, sum by (controller, le) '
                                    f'(rate(controller_runtime_reconcile_time_seconds_bucket{{{J}}}[$__rate_interval])))', "{{controller}}")],
           unit="s"),
        ts("Errors", [(f'sum by (controller) (rate(controller_runtime_reconcile_errors_total{{{J}}}[$__rate_interval]))', "{{controller}} errors"),
                      (f'sum by (controller) (rate(controller_runtime_terminal_reconcile_errors_total{{{J}}}[$__rate_interval]))', "{{controller}} terminal"),
                      (f'sum by (code) (rate(rest_client_requests_total{{{J}, code!~"2.."}}[$__rate_interval]))', "API server {{code}}")],
           unit="ops", desc="Reconciles that returned an error (retried with backoff), terminal errors (not retried), "
                            "and Kubernetes API requests that didn't succeed (409 conflicts are routine)."),
    )
    d.line(
        ts("Work queue depth", [(f'sum by (name) (workqueue_depth{{{J}}})', "{{name}}")], unit="none", decimals=0,
           desc="Objects waiting for a worker. Steadily above zero means the controller can't keep up."),
        ts("Time in queue, p99", [(f'histogram_quantile(0.99, sum by (name, le) (rate(workqueue_queue_duration_seconds_bucket{{{J}}}[$__rate_interval])))', "{{name}}")],
           unit="s", desc="How long an object waits before a reconcile picks it up."),
        ts("Process", [(f'sum(rate(process_cpu_seconds_total{{{J}}}[$__rate_interval]))', "CPU (cores)"),
                       (f'sum(process_resident_memory_bytes{{{J}}})', "memory"),
                       (f'sum(go_goroutines{{{J}}})', "goroutines")],
           unit="none", overrides=[by_name("memory", {"id": "unit", "value": "bytes"}, {"id": "custom.axisPlacement", "value": "right"}),
                                   by_name("goroutines", {"id": "custom.axisPlacement", "value": "hidden"})]),
    )

    variables = [
        var_ds(),
        var_query("namespace", "Namespace", "label_values(freeswitch_operator_fleet_desired_replicas, namespace)"),
        var_query("fleet", "Fleet", 'label_values(freeswitch_operator_fleet_desired_replicas{namespace=~"$namespace"}, name)', all_value=".*"),
        var_query("pool", "Address pool", "label_values(freeswitch_operator_address_pool_addresses, pool)", all_value=".*"),
    ]
    write(d, "edge-freeswitch-operator", "FreeSWITCH operator", "The fleets as the operator runs them (members, "
          "conditions, rolls and drains), the dispatcher list, address pools, and the controller itself.",
          ["operator"], variables, out)


CHECK = sys.argv[1:] == ["--check"]
STALE = []

if __name__ == "__main__":
    out = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 and not CHECK else OUT
    out.mkdir(parents=True, exist_ok=True)
    kamailio(out)
    freeswitch(out)
    operator(out)
    for path in STALE:
        print(f"{path.relative_to(ROOT)} doesn't match hack/dashboards/dashboards.py: run make dashboards", file=sys.stderr)
    sys.exit(1 if STALE else 0)
