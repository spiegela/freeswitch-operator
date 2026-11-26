package fsexporter

import (
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// RecoveryCollector reports what the operator's recovery on start (spec.recovery) did the last time FreeSWITCH
// started in this pod, from the one line its hook writes to File: sofia recover's answer, or why it recovered
// nothing ("skipped: ..."). Until this start's recovery has finished (or with no recovery at all) there is no file
// and nothing is reported.
type RecoveryCollector struct {
	File string
}

var (
	descRecoverySkipped = prometheus.NewDesc("freeswitch_recovery_skipped",
		"1 when the last start's recovery recovered nothing because something was wrong (its SIP profiles weren't "+
			"all running in time, or one isn't named for the pod's address), 0 when sofia recover ran.", nil, nil)
	descRecoveredLegs = prometheus.NewDesc("freeswitch_recovery_recovered_legs",
		"Call legs the last start's sofia recover took over (two per bridged call); absent when it was skipped.", nil, nil)
	descRecoveryTime = prometheus.NewDesc("freeswitch_recovery_timestamp_seconds",
		"When the last start's recovery finished, as a Unix time.", nil, nil)
)

// Describe implements prometheus.Collector.
func (c *RecoveryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descRecoverySkipped
	ch <- descRecoveredLegs
	ch <- descRecoveryTime
}

// Collect implements prometheus.Collector.
func (c *RecoveryCollector) Collect(ch chan<- prometheus.Metric) {
	b, err := os.ReadFile(c.File)
	if err != nil {
		return
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		return
	}
	if st, err := os.Stat(c.File); err == nil {
		ch <- prometheus.MustNewConstMetric(descRecoveryTime, prometheus.GaugeValue, float64(st.ModTime().UnixNano())/1e9)
	}
	legs, ok := RecoveredLegs(line)
	skipped := 1.0
	if ok {
		skipped = 0
		ch <- prometheus.MustNewConstMetric(descRecoveredLegs, prometheus.GaugeValue, float64(legs))
	}
	ch <- prometheus.MustNewConstMetric(descRecoverySkipped, prometheus.GaugeValue, skipped)
}

// recoveredAnswer is sofia recover's answer when it ran: "Recovered 2 call(s)". FreeSWITCH counts call legs.
var recoveredAnswer = regexp.MustCompile(`Recovered (\d+) call`)

// RecoveredLegs reads how many call legs sofia recover took over from its answer ("Recovered 2 call(s)", or "No
// calls to recover."), and whether the answer was one of those at all.
func RecoveredLegs(answer string) (int, bool) {
	if strings.Contains(answer, "No calls to recover") {
		return 0, true
	}
	if m := recoveredAnswer.FindStringSubmatch(answer); m != nil {
		n, err := strconv.Atoi(m[1])
		return n, err == nil
	}
	return 0, false
}
