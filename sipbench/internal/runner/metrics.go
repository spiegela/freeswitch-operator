package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"
)

// MetricResult is what one metric check saw.
type MetricResult struct {
	Query  string  `json:"query"`
	Want   float64 `json:"wantIncrease"`
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	// Checked is false when the run has no metrics backend, or the baseline couldn't be read.
	Checked bool   `json:"checked"`
	Error   string `json:"error,omitempty"`
}

// How long a metric takes to reach the backend: the Collector's 10 s scrape, its batch, the push. A baseline is
// settled when it holds for metricSettle; a rise is waited for up to metricWait.
var (
	metricSettle = 12 * time.Second
	metricWait   = 60 * time.Second
	metricPoll   = 2 * time.Second
)

// metricBaselines reads each check's value before the scenario, once it has stopped moving: a counter the
// previous scenario raised may still be on its way.
func (r *Runner) metricBaselines(ctx context.Context, checks []scenario.MetricCheck) []MetricResult {
	out := make([]MetricResult, len(checks))
	for i, c := range checks {
		out[i] = MetricResult{Query: c.Query, Want: c.Increase}
		if r.cfg.PrometheusURL == "" {
			out[i].Error = "no metrics backend (-prometheus)"
			continue
		}
		v, err := r.settled(ctx, c.Query)
		if err != nil {
			out[i].Error = "baseline: " + err.Error()
			continue
		}
		out[i].Before, out[i].Checked = v, true
	}
	return out
}

func (r *Runner) settled(ctx context.Context, q string) (float64, error) {
	deadline := time.Now().Add(metricWait)
	v, err := r.promQuery(ctx, q)
	if err != nil {
		return 0, err
	}
	since := time.Now()
	for time.Since(since) < metricSettle {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("still changing after %s (last %g)", metricWait, v)
		}
		if err := sleepCtx(ctx, metricPoll); err != nil {
			return 0, err
		}
		next, err := r.promQuery(ctx, q)
		if err != nil {
			return 0, err
		}
		if next != v {
			v, since = next, time.Now()
		}
	}
	return v, nil
}

// metricChecks waits for each checked metric to rise by its increase and returns a failure for each that doesn't.
func (r *Runner) metricChecks(ctx context.Context, results []MetricResult) []string {
	var failures []string
	deadline := time.Now().Add(metricWait)
	for i := range results {
		m := &results[i]
		if !m.Checked {
			continue
		}
		for {
			v, err := r.promQuery(ctx, m.Query)
			if err == nil {
				m.After = v
				if v-m.Before >= m.Want {
					break
				}
			} else {
				m.Error = err.Error()
			}
			if time.Now().After(deadline) || sleepCtx(ctx, metricPoll) != nil {
				failures = append(failures, fmt.Sprintf("metric %s: rose by %g (from %g to %g), want at least %g",
					m.Query, m.After-m.Before, m.Before, m.After, m.Want))
				break
			}
		}
	}
	return failures
}

// promQuery runs an instant query and returns its single number; no result is 0.
func (r *Runner) promQuery(ctx context.Context, q string) (float64, error) {
	u := r.cfg.PrometheusURL + "/api/v1/query?" + url.Values{"query": {q}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Status    string `json:"status"`
		Error     string `json:"error"`
		ErrorType string `json:"errorType"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("query %q: %s: %w", q, resp.Status, err)
	}
	if body.Status != "success" {
		return 0, fmt.Errorf("query %q: %s: %s", q, body.ErrorType, body.Error)
	}
	var sample []any
	switch body.Data.ResultType {
	case "scalar":
		if err := json.Unmarshal(body.Data.Result, &sample); err != nil {
			return 0, err
		}
	case "vector":
		var vec []struct {
			Value []any `json:"value"`
		}
		if err := json.Unmarshal(body.Data.Result, &vec); err != nil {
			return 0, err
		}
		switch len(vec) {
		case 0:
			return 0, nil
		case 1:
			sample = vec[0].Value
		default:
			return 0, fmt.Errorf("query %q: %d series, want one (wrap it in sum())", q, len(vec))
		}
	default:
		return 0, fmt.Errorf("query %q: result type %q, want a vector or scalar", q, body.Data.ResultType)
	}
	if len(sample) != 2 {
		return 0, fmt.Errorf("query %q: malformed sample", q)
	}
	s, ok := sample[1].(string)
	if !ok {
		return 0, fmt.Errorf("query %q: malformed sample value", q)
	}
	return strconv.ParseFloat(s, 64)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
