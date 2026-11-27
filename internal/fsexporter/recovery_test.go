package fsexporter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecoveryCollector(t *testing.T) {
	file := filepath.Join(t.TempDir(), "recovered")
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(&RecoveryCollector{File: file})

	// No file yet: this start's recovery hasn't finished.
	if n, err := testutil.GatherAndCount(reg); err != nil || n != 0 {
		t.Fatalf("before the hook: %d series, err %v", n, err)
	}

	const (
		skippedHelp = "# HELP freeswitch_recovery_skipped 1 when the last start's recovery recovered nothing because " +
			"something was wrong (its SIP profiles weren't all running in time, or one isn't named for the pod's " +
			"address), 0 when sofia recover ran.\n# TYPE freeswitch_recovery_skipped gauge\n"
		legsHelp = "# HELP freeswitch_recovery_recovered_legs Call legs the last start's sofia recover took over " +
			"(two per bridged call); absent when it was skipped.\n# TYPE freeswitch_recovery_recovered_legs gauge\n"
	)
	for _, tc := range []struct{ line, want string }{
		{"+OK Recovered 4 call(s)", legsHelp + "freeswitch_recovery_recovered_legs 4\n" + skippedHelp + "freeswitch_recovery_skipped 0\n"},
		{"No calls to recover.", legsHelp + "freeswitch_recovery_recovered_legs 0\n" + skippedHelp + "freeswitch_recovery_skipped 0\n"},
		{"skipped: SIP profile trunks is not named for this pod's address (NAME-10-0-0-1); calls not recovered",
			skippedHelp + "freeswitch_recovery_skipped 1\n"},
		{"no answer from sofia recover", skippedHelp + "freeswitch_recovery_skipped 1\n"},
	} {
		if err := os.WriteFile(file, []byte(tc.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := testutil.GatherAndCompare(reg, strings.NewReader(tc.want),
			"freeswitch_recovery_skipped", "freeswitch_recovery_recovered_legs"); err != nil {
			t.Errorf("%q: %v", tc.line, err)
		}
		if n, err := testutil.GatherAndCount(reg, "freeswitch_recovery_timestamp_seconds"); err != nil || n != 1 {
			t.Errorf("%q: timestamp series %d, err %v", tc.line, n, err)
		}
	}
}
