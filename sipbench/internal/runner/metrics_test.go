package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

func fastMetrics(t *testing.T) {
	t.Helper()
	s, w, p := metricSettle, metricWait, metricPoll
	metricSettle, metricWait, metricPoll = 30*time.Millisecond, 400*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { metricSettle, metricWait, metricPoll = s, w, p })
}

// fakeProm answers instant queries with value(), as a one-series vector (or none when value is negative).
func fakeProm(t *testing.T, value func(q string) float64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "bad(") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
			return
		}
		v := value(q)
		result := "[]"
		if v >= 0 {
			result = fmt.Sprintf(`[{"metric":{},"value":[1700000000.0,"%g"]}]`, v)
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, result)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestMetricChecksPassOnARise(t *testing.T) {
	fastMetrics(t)
	var v atomic.Int64
	v.Store(-1) // no series yet: counts as 0
	r := New(Config{PrometheusURL: fakeProm(t, func(string) float64 { return float64(v.Load()) })})
	ctx := context.Background()
	checks := []scenario.MetricCheck{{Query: "sum(x_total)", Increase: 4}}

	res := r.metricBaselines(ctx, checks)
	if !res[0].Checked || res[0].Before != 0 {
		t.Fatalf("baseline = %+v, want checked from 0", res[0])
	}
	// The counter arrives in steps, as scrapes do.
	go func() {
		for _, x := range []int64{2, 4, 5} {
			time.Sleep(20 * time.Millisecond)
			v.Store(x)
		}
	}()
	if f := r.metricChecks(ctx, res); len(f) != 0 {
		t.Fatalf("failures = %v", f)
	}
	if res[0].After < 4 {
		t.Errorf("after = %v, want at least 4", res[0].After)
	}
}

func TestMetricChecksFailWithoutARise(t *testing.T) {
	fastMetrics(t)
	r := New(Config{PrometheusURL: fakeProm(t, func(string) float64 { return 7 })})
	res := r.metricBaselines(context.Background(), []scenario.MetricCheck{{Query: "sum(x_total)", Increase: 1}})
	f := r.metricChecks(context.Background(), res)
	if len(f) != 1 || !strings.Contains(f[0], "rose by 0 (from 7 to 7), want at least 1") {
		t.Fatalf("failures = %v", f)
	}
}

func TestMetricBaselineWaitsToSettle(t *testing.T) {
	fastMetrics(t)
	var v atomic.Int64
	v.Store(3)
	go func() { time.Sleep(15 * time.Millisecond); v.Store(5) }() // the previous scenario's counts land late
	r := New(Config{PrometheusURL: fakeProm(t, func(string) float64 { return float64(v.Load()) })})
	res := r.metricBaselines(context.Background(), []scenario.MetricCheck{{Query: "sum(x_total)", Increase: 1}})
	if res[0].Before != 5 {
		t.Errorf("baseline = %v, want the settled 5", res[0].Before)
	}
}

func TestMetricChecksNotCheckedWithoutBackend(t *testing.T) {
	r := New(Config{})
	res := r.metricBaselines(context.Background(), []scenario.MetricCheck{{Query: "sum(x_total)", Increase: 1}})
	if res[0].Checked || res[0].Error == "" {
		t.Errorf("result = %+v, want unchecked with a reason", res[0])
	}
	if f := r.metricChecks(context.Background(), res); len(f) != 0 {
		t.Errorf("failures = %v, want none", f)
	}
}

func TestPromQueryErrors(t *testing.T) {
	r := New(Config{PrometheusURL: fakeProm(t, func(string) float64 { return 1 })})
	if _, err := r.promQuery(context.Background(), "bad(x)"); err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("err = %v, want the API's error", err)
	}
}
