package controller

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func metricsPod(name, uid string, ready bool, deletedAt *time.Time, grace int64) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if deletedAt != nil {
		p.DeletionTimestamp = &metav1.Time{Time: deletedAt.Add(time.Duration(grace) * time.Second)}
		p.DeletionGracePeriodSeconds = ptr.To(grace)
	}
	return p
}

func TestRecordFleetStateAndDrains(t *testing.T) {
	f := fleetRef{kind: kindFreeswitch, namespace: "metrics-test", name: "edge"}
	defer forgetFleet(f)
	t0 := time.Unix(1_800_000_000, 0)
	deleted := t0.Add(-90 * time.Second)

	pods := []corev1.Pod{
		metricsPod("edge-0", "u0", true, nil, 0),
		metricsPod("edge-1", "u1", false, &deleted, 330),
		metricsPod("edge-2", "u2", false, nil, 0),
	}
	conds := []metav1.Condition{
		{Type: conditionAvailable, Status: metav1.ConditionTrue},
		{Type: conditionReady, Status: metav1.ConditionFalse},
	}
	recordFleet(f, fleetObservation{desired: 3, pods: pods, outdated: 2, conditions: conds, maxDrainSeconds: 300, now: t0})

	l := f.labels
	checks := map[string]float64{
		"desired":     testutil.ToFloat64(fleetDesiredReplicas.WithLabelValues(l()...)),
		"active":      testutil.ToFloat64(fleetMembersGauge.WithLabelValues(l("Active")...)),
		"starting":    testutil.ToFloat64(fleetMembersGauge.WithLabelValues(l("Starting")...)),
		"draining":    testutil.ToFloat64(fleetMembersGauge.WithLabelValues(l("Draining")...)),
		"outdated":    testutil.ToFloat64(fleetOutdatedMembers.WithLabelValues(l()...)),
		"available":   testutil.ToFloat64(fleetCondition.WithLabelValues(l(conditionAvailable)...)),
		"ready":       testutil.ToFloat64(fleetCondition.WithLabelValues(l(conditionReady)...)),
		"maxDrain":    testutil.ToFloat64(fleetMaxDrainSeconds.WithLabelValues(l()...)),
		"drainStart1": testutil.ToFloat64(fleetDrainStarted.WithLabelValues(l("edge-1")...)),
	}
	want := map[string]float64{
		"desired": 3, "active": 1, "starting": 1, "draining": 1, "outdated": 2, "available": 1, "ready": 0,
		"maxDrain": 300, "drainStart1": float64(deleted.Unix()),
	}
	for k, v := range want {
		if checks[k] != v {
			t.Errorf("%s = %v, want %v", k, checks[k], v)
		}
	}

	// edge-1 is gone and a new edge-1 is starting: its drain took from deletion until now.
	t1 := t0.Add(30 * time.Second)
	pods[1] = metricsPod("edge-1", "u1b", false, nil, 0)
	recordFleet(f, fleetObservation{desired: 3, pods: pods, outdated: 1, conditions: conds, maxDrainSeconds: 300, now: t1})
	if n := testutil.CollectAndCount(fleetDrainStarted); n != 0 {
		t.Errorf("drain_started series after the drain = %d, want 0", n)
	}
	count, sum := drainHistogram(t, f)
	if count != 1 || sum != 120 {
		t.Errorf("drain duration count, sum = %d, %v; want 1, 120", count, sum)
	}

	forgetFleet(f)
	for name, c := range map[string]int{
		"desired":     testutil.CollectAndCount(fleetDesiredReplicas),
		"members":     testutil.CollectAndCount(fleetMembersGauge),
		"conditions":  testutil.CollectAndCount(fleetCondition),
		"durations":   testutil.CollectAndCount(fleetDrainDuration),
		"drainStarts": testutil.CollectAndCount(fleetDrainStarted),
	} {
		if c != 0 {
			t.Errorf("%s series after forgetFleet = %d, want 0", name, c)
		}
	}
}

func drainHistogram(t *testing.T, f fleetRef) (uint64, float64) {
	t.Helper()
	var m dto.Metric
	if err := fleetDrainDuration.WithLabelValues(f.labels()...).(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

func TestRecordDispatcherSeries(t *testing.T) {
	f := fleetRef{kind: kindKamailio, namespace: "metrics-test", name: "proxy"}
	defer forgetFleet(f)

	recordDestinations(f, map[int32]int{1: 3, 2: 1})
	recordDestinations(f, map[int32]int{1: 2})
	if got := testutil.ToFloat64(dispatcherDestinations.WithLabelValues(f.labels("1")...)); got != 2 {
		t.Errorf("set 1 destinations = %v, want 2", got)
	}
	if n := testutil.CollectAndCount(dispatcherDestinations); n != 1 {
		t.Errorf("destination series = %d, want 1 (set 2 left)", n)
	}

	recordReload(f, nil)
	recordReload(f, fmt.Errorf("pod a: %w", errListNotLoaded))
	recordReload(f, errors.Join(fmt.Errorf("pod a: %w", errListNotLoaded), errors.New("pod b: connection refused")))
	for result, want := range map[string]float64{reloadOK: 1, reloadNotLoaded: 1, reloadError: 1} {
		if got := testutil.ToFloat64(dispatcherReloads.WithLabelValues(f.labels(result)...)); got != want {
			t.Errorf("reloads{result=%q} = %v, want %v", result, got, want)
		}
	}

	recordSetInactive(f, nil)
	recordSetInactive(f, errors.New("timeout"))
	if got := testutil.ToFloat64(dispatcherSetInactive.WithLabelValues(f.labels(reloadError)...)); got != 1 {
		t.Errorf("set_inactive{result=error} = %v, want 1", got)
	}

	recordPendingReload(f, true)
	if got := testutil.ToFloat64(dispatcherPendingReload.WithLabelValues(f.labels()...)); got != 1 {
		t.Errorf("pending reload = %v, want 1", got)
	}
	recordPendingReload(f, false)
	if got := testutil.ToFloat64(dispatcherPendingReload.WithLabelValues(f.labels()...)); got != 0 {
		t.Errorf("pending reload = %v, want 0", got)
	}
}
