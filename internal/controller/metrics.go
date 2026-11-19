package controller

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
	"git.instruments.com/ci/freeswitch-operator/internal/addresspool"
)

// The operator's own metrics, served with controller-runtime's (reconcile counts and durations, work queues, the
// Go runtime) on the manager's metrics endpoint. Every series names its fleet with kind (FreeswitchCluster or
// KamailioCluster), namespace and name; a fleet's series go away with the fleet.
//
// Gauges describe what the last reconcile saw. Anything an alert times uses a timestamp (drain starts) or a
// gauge that a failing step keeps re-setting on its retries (a pending reload), so a quiet controller can't leave
// a stale value that looks fine.

const (
	kindFreeswitch = "FreeswitchCluster"
	kindKamailio   = "KamailioCluster"

	reloadOK        = "ok"
	reloadNotLoaded = "not_loaded"
	reloadError     = "error"
)

var fleetLabels = []string{"kind", "namespace", "name"}

// poolLabel names a SIPAddressPool in its series.
const poolLabel = "pool"

func withLabels(extra ...string) []string {
	return append(append([]string{}, fleetLabels...), extra...)
}

var (
	fleetDesiredReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_fleet_desired_replicas",
		Help: "Pods the fleet's spec asks for.",
	}, fleetLabels)
	fleetMembersGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_fleet_members",
		Help: "Pods in the fleet by state: Starting (not ready yet), Active (takes calls) or Draining (terminating).",
	}, withLabels("state"))
	fleetOutdatedMembers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_fleet_outdated_members",
		Help: "Pods not yet on the latest pod template; above zero while a rolling update is under way.",
	}, fleetLabels)
	fleetCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_fleet_condition",
		Help: "1 when the fleet's status condition is True, 0 otherwise: Ready, Available, Progressing, Degraded, and " +
			"SetConflict for a KamailioCluster.",
	}, withLabels("condition"))
	fleetPodReplacements = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "freeswitch_operator_fleet_pod_replacements_total",
		Help: "Pods the operator deleted to move them to the latest pod template (each then drains).",
	}, fleetLabels)
	fleetMaxDrainSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_fleet_max_drain_seconds",
		Help: "The fleet's drain.maxDrainSeconds: the longest a terminating pod waits for its calls or transactions.",
	}, fleetLabels)
	fleetDrainStarted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_fleet_drain_started_timestamp_seconds",
		Help: "When each draining pod started to drain (Unix time); the series exists while the pod drains.",
	}, withLabels("pod"))
	fleetDrainDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "freeswitch_operator_fleet_drain_duration_seconds",
		Help: "How long each drained pod took from deletion until it was gone, as the operator saw it.",
		// A Kamailio drain takes seconds; a FreeSWITCH drain waits for calls, up to hours.
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 14400},
	}, fleetLabels)

	dispatcherListUpdates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "freeswitch_operator_dispatcher_list_updates_total",
		Help: "Times the operator wrote a changed dispatcher list.",
	}, fleetLabels)
	dispatcherDestinations = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_dispatcher_destinations",
		Help: "Destinations in the dispatcher list the operator last wrote, by dispatcher set.",
	}, withLabels("set"))
	dispatcherReloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "freeswitch_operator_dispatcher_reloads_total",
		Help: "Dispatcher reloads asked of Kamailio, by result: ok (every Kamailio holds the list), not_loaded " +
			"(Kamailio answered but still holds an older list; retried in 10 s) or error (retried in 30 s).",
	}, withLabels("result"))
	dispatcherSetInactive = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "freeswitch_operator_dispatcher_set_inactive_total",
		Help: "Leaving destinations marked inactive on Kamailio, by result: ok or error.",
	}, withLabels("result"))
	dispatcherPendingReload = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_dispatcher_pending_reload",
		Help: "1 while the dispatcher list has changed and not every Kamailio is confirmed to hold it, 0 otherwise.",
	}, fleetLabels)
	// A pool's series name it with pool (pools are cluster-scoped) and go away with it.
	poolAddresses = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_address_pool_addresses",
		Help: "The pool's addresses by state: Free, Bound, Idle, Conflict or Error.",
	}, []string{poolLabel, "state"})
	poolNodesWaiting = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_address_pool_nodes_waiting",
		Help: "Nodes running a pod that uses the pool which hold none of its addresses.",
	}, []string{poolLabel})
	poolAwaitingAgent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_address_pool_addresses_awaiting_agent",
		Help: "Bound addresses whose node's address agent doesn't report their bind address on the node's interface " +
			"yet; the node's pods get no address (and wait, when it is required) until it does.",
	}, []string{poolLabel})
	poolProviderCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "freeswitch_operator_address_pool_provider_calls_total",
		Help: "Calls to the pool's provider by call (observe, bind, unbind or prune) and result (ok or error).",
	}, []string{poolLabel, "call", "result"})
	poolLastObserved = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "freeswitch_operator_address_pool_last_observed_timestamp_seconds",
		Help: "When the pool last read its provider successfully (Unix time). It rereads every 5 minutes.",
	}, []string{poolLabel})
)

func init() {
	metrics.Registry.MustRegister(
		fleetDesiredReplicas, fleetMembersGauge, fleetOutdatedMembers, fleetCondition, fleetPodReplacements,
		fleetMaxDrainSeconds, fleetDrainStarted, fleetDrainDuration,
		dispatcherListUpdates, dispatcherDestinations, dispatcherReloads, dispatcherSetInactive, dispatcherPendingReload,
		poolAddresses, poolNodesWaiting, poolAwaitingAgent, poolProviderCalls, poolLastObserved,
	)
}

// fleetRef names one fleet in the metrics.
type fleetRef struct {
	kind      string
	namespace string
	name      string
}

func (f fleetRef) labels(extra ...string) []string {
	return append([]string{f.kind, f.namespace, f.name}, extra...)
}

func (f fleetRef) partial() prometheus.Labels {
	return prometheus.Labels{"kind": f.kind, "namespace": f.namespace, "name": f.name}
}

// fleetObservation is what one reconcile saw of a fleet.
type fleetObservation struct {
	desired         int32
	pods            []corev1.Pod
	outdated        int
	conditions      []metav1.Condition
	maxDrainSeconds int64
	now             time.Time
}

// drains remembers when each draining pod started, by fleet and pod UID, so the drain's length can be observed
// once the pod is gone. It is the operator's own memory: a drain under way when the operator restarts is timed
// from its deletion all the same (the start comes from the pod), but one that ends while no operator runs is
// not observed.
var drains = &drainTracker{byFleet: map[fleetRef]map[types.UID]trackedDrain{}}

type trackedDrain struct {
	pod   string
	start time.Time
}

type drainTracker struct {
	mu      sync.Mutex
	byFleet map[fleetRef]map[types.UID]trackedDrain
}

// drainStart is when a terminating pod's drain began: its deletion, which the API server records as the deletion
// timestamp minus the grace period it granted.
func drainStart(p *corev1.Pod) time.Time {
	t := p.DeletionTimestamp.Time
	if g := p.DeletionGracePeriodSeconds; g != nil {
		t = t.Add(-time.Duration(*g) * time.Second)
	}
	return t
}

// observe updates the fleet's drain series from its current pods, and times each drain that has finished: a
// tracked pod that is no longer listed is gone.
func (d *drainTracker) observe(f fleetRef, pods []corev1.Pod, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tracked := d.byFleet[f]
	if tracked == nil {
		tracked = map[types.UID]trackedDrain{}
		d.byFleet[f] = tracked
	}
	seen := map[types.UID]bool{}
	for i := range pods {
		p := &pods[i]
		seen[p.UID] = true
		if p.DeletionTimestamp == nil {
			continue
		}
		if _, ok := tracked[p.UID]; !ok {
			tracked[p.UID] = trackedDrain{pod: p.Name, start: drainStart(p)}
		}
	}
	for uid, t := range tracked {
		if seen[uid] {
			continue
		}
		fleetDrainDuration.WithLabelValues(f.labels()...).Observe(max(now.Sub(t.start).Seconds(), 0))
		delete(tracked, uid)
	}
	// One series per draining pod name. A replacement reuses a StatefulSet pod's name, so the series is rebuilt
	// from what is still tracked rather than deleted by name.
	fleetDrainStarted.DeletePartialMatch(f.partial())
	for _, t := range tracked {
		fleetDrainStarted.WithLabelValues(f.labels(t.pod)...).Set(float64(t.start.UnixNano()) / 1e9)
	}
}

func (d *drainTracker) forget(f fleetRef) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.byFleet, f)
}

// recordFleet sets a fleet's state series from one reconcile.
func recordFleet(f fleetRef, o fleetObservation) {
	fleetDesiredReplicas.WithLabelValues(f.labels()...).Set(float64(o.desired))
	counts := map[sipv1alpha1.MemberState]int{
		sipv1alpha1.MemberStarting: 0,
		sipv1alpha1.MemberActive:   0,
		sipv1alpha1.MemberDraining: 0,
	}
	for i := range o.pods {
		counts[memberState(&o.pods[i])]++
	}
	for state, n := range counts {
		fleetMembersGauge.WithLabelValues(f.labels(string(state))...).Set(float64(n))
	}
	fleetOutdatedMembers.WithLabelValues(f.labels()...).Set(float64(o.outdated))
	for _, c := range o.conditions {
		v := 0.0
		if c.Status == metav1.ConditionTrue {
			v = 1
		}
		fleetCondition.WithLabelValues(f.labels(c.Type)...).Set(v)
	}
	fleetMaxDrainSeconds.WithLabelValues(f.labels()...).Set(float64(o.maxDrainSeconds))
	drains.observe(f, o.pods, o.now)
}

// recordDestinations replaces the fleet's per-set destination counts.
func recordDestinations(f fleetRef, bySet map[int32]int) {
	dispatcherDestinations.DeletePartialMatch(f.partial())
	for set, n := range bySet {
		dispatcherDestinations.WithLabelValues(f.labels(strconv.Itoa(int(set)))...).Set(float64(n))
	}
}

func recordReload(f fleetRef, err error) {
	switch {
	case err == nil:
		dispatcherReloads.WithLabelValues(f.labels(reloadOK)...).Inc()
	case onlyListNotLoaded(err):
		dispatcherReloads.WithLabelValues(f.labels(reloadNotLoaded)...).Inc()
	default:
		dispatcherReloads.WithLabelValues(f.labels(reloadError)...).Inc()
	}
}

func recordSetInactive(f fleetRef, err error) {
	dispatcherSetInactive.WithLabelValues(f.labels(resultLabel(err))...).Inc()
}

// resultLabel is a call's result label: ok, or error.
func resultLabel(err error) string {
	if err != nil {
		return reloadError
	}
	return reloadOK
}

func recordPendingReload(f fleetRef, pending bool) {
	v := 0.0
	if pending {
		v = 1
	}
	dispatcherPendingReload.WithLabelValues(f.labels()...).Set(v)
}

// forgetFleet removes every series of a fleet that no longer exists. Counters and the drain histogram go too: a
// fleet created again under the same name starts from zero, which rate() and increase() read as a reset.
func forgetFleet(f fleetRef) {
	for _, v := range []interface {
		DeletePartialMatch(prometheus.Labels) int
	}{
		fleetDesiredReplicas, fleetMembersGauge, fleetOutdatedMembers, fleetCondition, fleetPodReplacements,
		fleetMaxDrainSeconds, fleetDrainStarted, dispatcherListUpdates, dispatcherDestinations, dispatcherReloads,
		fleetDrainDuration, dispatcherSetInactive, dispatcherPendingReload,
	} {
		v.DeletePartialMatch(f.partial())
	}
	drains.forget(f)
}

// recordPoolCall counts one provider call.
func recordPoolCall(pool, call string, err error) {
	poolProviderCalls.WithLabelValues(pool, call, resultLabel(err)).Inc()
}

// recordPool sets a pool's state series from one pass.
func recordPool(pool string, addrs []sipv1alpha1.PoolAddress, waiting int) {
	counts := map[sipv1alpha1.PoolAddressState]int{
		sipv1alpha1.PoolAddressFree: 0, sipv1alpha1.PoolAddressBound: 0, sipv1alpha1.PoolAddressIdle: 0,
		sipv1alpha1.PoolAddressConflict: 0, sipv1alpha1.PoolAddressError: 0,
	}
	awaiting := 0
	for i := range addrs {
		counts[addrs[i].State]++
		if addresspool.AwaitingAgent(&addrs[i]) {
			awaiting++
		}
	}
	for state, n := range counts {
		poolAddresses.WithLabelValues(pool, string(state)).Set(float64(n))
	}
	poolNodesWaiting.WithLabelValues(pool).Set(float64(waiting))
	poolAwaitingAgent.WithLabelValues(pool).Set(float64(awaiting))
}

// forgetPool removes every series of a pool that no longer exists.
func forgetPool(pool string) {
	for _, v := range []interface {
		DeletePartialMatch(prometheus.Labels) int
	}{poolAddresses, poolNodesWaiting, poolAwaitingAgent, poolProviderCalls, poolLastObserved} {
		v.DeletePartialMatch(prometheus.Labels{poolLabel: pool})
	}
}
