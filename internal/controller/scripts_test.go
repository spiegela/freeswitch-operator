package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sofiaTwoProfiles = "                     Name\t   Type\t                                      Data\tState\n" +
	"=================================================================================================\n" +
	"                  backend\tprofile\t             sip:mod_sofia@172.18.0.6:5080\tRUNNING (0)\n" +
	"                   trunks\tprofile\t             sip:mod_sofia@172.18.0.6:5060\tRUNNING (0)\n" +
	"=================================================================================================\n" +
	"2 profiles 0 aliases\n"

const fsReady = "UP 0 years, 0 days, 0 hours, 1 minute, 2 seconds\nFreeSWITCH (Version 1.10.10) is ready\n"

// runScript runs one of the operator's probe scripts with a fake fs_cli that prints status and sofia for
// "status" and "sofia status".
func runScript(t *testing.T, name, status, sofia string, env ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	for file, body := range map[string]string{startupScriptName: startupScript, readyScriptName: readyScript, liveScriptName: liveScript} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	for file, body := range map[string]string{"status.out": status, "sofia.out": sofia} {
		if err := os.WriteFile(filepath.Join(bin, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The command is fs_cli's last argument; FAKE_ARGS, when set, collects every call's arguments.
	fake := "#!/bin/sh\nfor a; do last=$a; done\n[ -n \"$FAKE_ARGS\" ] && echo \"$*\" >>\"$FAKE_ARGS\"\ncase \"$last\" in\n  status) cat '" + bin + "/status.out' ;;\n  'sofia status') cat '" + bin + "/sofia.out' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "fs_cli"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", filepath.Join(dir, name))
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func TestStartupScript(t *testing.T) {
	cases := []struct {
		name, status, sofia, port string
		want                      int
		says                      string
	}{
		{"profile on the SIP port", fsReady, sofiaTwoProfiles, "5060", 0, ""},
		{"profile on another port only", fsReady, sofiaTwoProfiles, "5070", 1, "no SIP profile running on port 5070"},
		{"port is a suffix of another", fsReady, sofiaTwoProfiles, "060", 1, "no SIP profile"},
		{"no profiles (empty sip_profiles)", fsReady, "0 profiles 0 aliases\n", "5060", 1, "no SIP profile"},
		{"profile not running", fsReady, strings.ReplaceAll(sofiaTwoProfiles, "RUNNING (0)", "DOWN"), "5060", 1, "no SIP profile"},
		{"FreeSWITCH not ready", "FreeSWITCH is starting\n", sofiaTwoProfiles, "5060", 1, "not ready"},
		{"fs_cli silent", "", "", "5060", 1, "not ready"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := runScript(t, startupScriptName, c.status, c.sofia, "FS_SIP_PORT="+c.port)
			if code != c.want || !strings.Contains(out, c.says) {
				t.Fatalf("exit %d (want %d), output %q (want %q)", code, c.want, out, c.says)
			}
		})
	}
}

func TestReadyScriptRunsTheStartupChecks(t *testing.T) {
	if code, out := runScript(t, readyScriptName, fsReady, sofiaTwoProfiles, "FS_SIP_PORT=5060"); code != 0 {
		t.Fatalf("healthy node not ready: %d %q", code, out)
	}
	if code, out := runScript(t, readyScriptName, fsReady, "0 profiles 0 aliases\n", "FS_SIP_PORT=5060"); code == 0 || !strings.Contains(out, "no SIP profile") {
		t.Fatalf("a node without its profile must not be ready: %d %q", code, out)
	}
	if _, err := os.Stat(externalAddrFile); err == nil {
		t.Skip(externalAddrFile + " exists here")
	}
	if code, out := runScript(t, readyScriptName, fsReady, sofiaTwoProfiles, "FS_SIP_PORT=5060", "FS_REQUIRE_EXTERNAL_ADDRESS=true"); code == 0 || !strings.Contains(out, "external address") {
		t.Fatalf("a required external address must be checked first: %d %q", code, out)
	}
}

func TestDrainWaitsForAStartingFreeSWITCH(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "draining")
	script := strings.ReplaceAll(drainScript, drainMarker, marker)
	if err := os.WriteFile(filepath.Join(dir, "drain.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Not ready for the first two status calls, then ready; after the pause, one call leg, then none. The core db
	// keeps a row for a call long gone, which the drain must not wait for. Every command is logged.
	fake := `#!/bin/sh
d='` + dir + `'
echo "$*" >> "$d/log"
case "$*" in
  *"-x status")
    n=$(cat "$d/n" 2>/dev/null || echo 0); echo $((n + 1)) > "$d/n"
    if [ "$n" -lt 2 ]; then exit 1; fi
    echo "FreeSWITCH (Version 1.10.10) is ready"
    if [ "$n" -eq 3 ]; then legs=1; else legs=0; fi
    echo "$legs session(s) - peak 2, last 5min 2 " ;;
  *"show calls count"*) echo "1 total." ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "fs_cli"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", filepath.Join(dir, "drain.sh"))
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "FS_MAX_DRAIN_SECONDS=30"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("drain: %v %s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the drain must mark the pod as draining first")
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	calls := strings.Split(strings.TrimSpace(string(log)), "\n")
	var status, pauseAt int
	for i, c := range calls {
		if strings.HasSuffix(c, "-x status") {
			status++
		}
		if strings.Contains(c, "fsctl pause inbound") {
			pauseAt = i
		}
	}
	if pauseAt != 3 || status != 5 {
		t.Fatalf("want three status checks, the pause, then two more (one call leg, then none); got %q", calls)
	}
	if !strings.Contains(string(out), "no live calls") {
		t.Fatalf("output %q", out)
	}
}

func TestLiveScript(t *testing.T) {
	cases := []struct {
		name, status string
		want         int
	}{
		{"ready", fsReady, 0},
		{"up but not ready", "UP 0 years, 0 days, 0 hours, 0 minutes, 3 seconds\nFreeSWITCH (Version 1.10.10) is starting\n", 0},
		{"paused", "UP 0 years, 0 days, 2 hours, 0 minutes, 3 seconds\nFreeSWITCH (Version 1.10.10) is ready (paused)\n", 0},
		{"no answer", "", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := runScript(t, liveScriptName, c.status, "")
			if code != c.want {
				t.Fatalf("exit %d (want %d), output %q", code, c.want, out)
			}
		})
	}
}

func TestLiveScriptKeepsUnderTheProbeTimeout(t *testing.T) {
	for timeout, want := range map[string]string{"15": "-t 13000", "5": "-t 3000", "3": "-t 2000"} {
		args := filepath.Join(t.TempDir(), "args")
		if code, out := runScript(t, liveScriptName, fsReady, "", "FS_LIVENESS_TIMEOUT_SECONDS="+timeout, "FAKE_ARGS="+args); code != 0 {
			t.Fatalf("timeout %s: exit %d %q", timeout, code, out)
		}
		got, err := os.ReadFile(args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), want) {
			t.Fatalf("timeout %s: fs_cli called with %q, want %q", timeout, got, want)
		}
	}
}

func TestAddressWaitScript(t *testing.T) {
	f := filepath.Join(t.TempDir(), "external-address")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", addressWaitScript)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "FS_EXTERNAL_ADDRESS_FILE=" + f}
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		t.Fatalf("returned with an empty file: %q", out.String())
	case <-time.After(1500 * time.Millisecond):
	}
	if err := os.WriteFile(f, []byte("203.0.113.10"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("still waiting after the address appeared")
	}
	if !strings.Contains(out.String(), "waiting for this pod's external address") || !strings.Contains(out.String(), "external address 203.0.113.10") {
		t.Errorf("output %q", out.String())
	}
}

// sofiaConf is what `xml_locate configuration configuration name sofia.conf` prints for a configuration with the
// given profile names.
func sofiaConf(names ...string) string {
	var b strings.Builder
	b.WriteString("<configuration name=\"sofia.conf\" description=\"sofia Endpoint\">\n  <profiles>\n")
	for _, n := range names {
		b.WriteString("    <profile name=\"" + n + "\">\n      <aliases>\n        <alias name=\"x\"/>\n      </aliases>\n" +
			"      <gateways>\n        <gateway name=\"carrier\"/>\n      </gateways>\n    </profile>\n")
	}
	b.WriteString("  </profiles>\n</configuration>\n")
	return b.String()
}

// sofiaStatus is `sofia status` with the given profiles running, each with its plain name as an alias.
func sofiaStatus(names ...string) string {
	var b strings.Builder
	b.WriteString("                     Name\t   Type\t                                      Data\tState\n")
	for _, n := range names {
		b.WriteString("  " + n + "\tprofile\t   sip:mod_sofia@10.0.0.5:5060\tRUNNING (0)\n")
		plain, _, _ := strings.Cut(n, "-")
		b.WriteString("  " + plain + "\talias\t   " + n + "\tALIASED\n")
	}
	return b.String()
}

type recoverRun struct {
	result, log string
	recovered   bool // whether sofia recover ran
}

// runRecover runs the recover script with a fake fs_cli. statuses are what `sofia status` prints, one per call,
// the last repeating, so a profile can start while the script waits.
func runRecover(t *testing.T, address, status, conf string, statuses ...string) recoverRun {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, recoverScriptName), recoverScript, 0o755)
	write(filepath.Join(bin, "status.out"), status, 0o644)
	write(filepath.Join(bin, "conf.out"), conf, 0o644)
	for i, s := range statuses {
		write(filepath.Join(bin, "sofia."+strconv.Itoa(i)), s, 0o644)
	}
	addrFile := filepath.Join(dir, "external-address")
	if address != "" {
		write(addrFile, address+"\n", 0o644)
	}
	fake := `#!/bin/sh
for a; do last=$a; done
b='` + bin + `'
case "$last" in
  status) cat "$b/status.out" ;;
  'xml_locate configuration configuration name sofia.conf') cat "$b/conf.out" ;;
  'sofia status')
    n=$(cat "$b/n" 2>/dev/null || echo 0)
    [ -e "$b/sofia.$n" ] && echo $((n + 1)) > "$b/n" || n=$((n - 1))
    cat "$b/sofia.$n" ;;
  'sofia recover') touch "$b/recovered"; echo "Recovered 4 call(s)" ;;
esac
`
	write(filepath.Join(bin, "fs_cli"), fake, 0o755)
	result, logFile := filepath.Join(dir, "recovered"), filepath.Join(dir, "log")
	cmd := exec.Command("/bin/sh", filepath.Join(dir, recoverScriptName))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FS_RECOVERY_WAIT_SECONDS=3",
		"FS_EXTERNAL_ADDRESS_FILE=" + addrFile, "FS_RECOVERED_FILE=" + result, "FS_RECOVERY_LOG=" + logFile}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recover.sh must always succeed (a failed postStart hook kills the container): %v: %s", err, out)
	}
	r := recoverRun{}
	b, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	r.result = strings.TrimSpace(string(b))
	if b, err := os.ReadFile(logFile); err == nil {
		r.log = string(b)
	}
	_, err = os.Stat(filepath.Join(bin, "recovered"))
	r.recovered = err == nil
	return r
}

func TestRecoverScript(t *testing.T) {
	const addr = "203.0.113.10"
	named := []string{"backend-203-0-113-10", "trunks-203-0-113-10"}
	for _, tc := range []struct {
		name, address, status, conf string
		statuses                    []string
		recovers                    bool
		want                        string
	}{
		{"every profile running and named", addr, fsReady, sofiaConf(named...),
			[]string{sofiaStatus(named...)}, true, "Recovered 4 call(s)"},
		{"waits for the last profile to start", addr, fsReady, sofiaConf(named...),
			[]string{sofiaStatus(named[1]), sofiaStatus(named[1]), sofiaStatus(named...)}, true, "Recovered 4 call(s)"},
		{"a profile that never starts", addr, fsReady, sofiaConf(named...),
			[]string{sofiaStatus(named[1])}, false, "skipped: SIP profiles not all running after 3s (missing backend-203-0-113-10); calls not recovered"},
		{"FreeSWITCH never ready", addr, "", sofiaConf(named...),
			[]string{sofiaStatus(named...)}, false, "(FreeSWITCH not ready)"},
		{"a profile under a shared name", addr, fsReady, sofiaConf("backend", named[1]),
			[]string{sofiaStatus("backend", named[1])}, false, "skipped: SIP profile backend is not named for this pod's address"},
		{"another address's name", addr, fsReady, sofiaConf("trunks-203-0-113-11"),
			[]string{sofiaStatus("trunks-203-0-113-11")}, false, "is not named for this pod's address (NAME-203-0-113-10)"},
		{"no address", "", fsReady, sofiaConf(named...),
			[]string{sofiaStatus(named...)}, false, "skipped: this pod has no external address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRecover(t, tc.address, tc.status, tc.conf, tc.statuses...)
			if r.recovered != tc.recovers {
				t.Errorf("sofia recover ran: %v, want %v (result %q)", r.recovered, tc.recovers, r.result)
			}
			if !strings.Contains(r.result, tc.want) {
				t.Errorf("result %q, want it to hold %q", r.result, tc.want)
			}
			if want := "freeswitch-operator: recovery: " + r.result + "\n"; r.log != want {
				t.Errorf("log %q, want %q", r.log, want)
			}
		})
	}
}

// The recovery waits as long for FreeSWITCH as the startup probe does.
func TestRecoveryWaitsAsLongAsTheStartupProbe(t *testing.T) {
	if want := strconv.Itoa(int(startupGraceSeconds)); recoveryWaitSeconds != want {
		t.Errorf("recoveryWaitSeconds is %s, startupGraceSeconds %s", recoveryWaitSeconds, want)
	}
}
