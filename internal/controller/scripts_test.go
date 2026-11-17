package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	// Not ready for the first two status calls, then ready with no calls; every command is logged.
	fake := `#!/bin/sh
d='` + dir + `'
echo "$*" >> "$d/log"
case "$*" in
  *"-x status")
    n=$(cat "$d/n" 2>/dev/null || echo 0); echo $((n + 1)) > "$d/n"
    if [ "$n" -ge 2 ]; then echo "FreeSWITCH is ready"; else exit 1; fi ;;
  *"show calls count"*) echo "0 total." ;;
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
	if status != 3 || pauseAt != 3 {
		t.Fatalf("want three status checks, then the pause; got %q", calls)
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
