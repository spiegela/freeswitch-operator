package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runKamailioScript runs one of the Kamailio pod scripts with fake wget and nc on PATH. The fake wget answers
// tm.stats with the next line of stats (one "current" count per call, the last repeating) and records every call's
// arguments; nc exits with ncExit. The drain marker is moved into a temporary directory.
func runKamailioScript(t *testing.T, name string, stats []string, ncExit string, env ...string) (code int, out, calls, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "draining")
	for file, body := range buildKamailioScriptsConfigMap(testKamailioCluster()).Data {
		body = strings.ReplaceAll(body, kamailioDrainMarker, marker)
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "stats"), []byte(strings.Join(stats, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wget := `#!/bin/sh
echo "$*" >>'` + bin + `/calls'
case "$*" in
  *tm.stats*)
    n=$(head -n 1 '` + bin + `/stats')
    [ "$(wc -l <'` + bin + `/stats')" -gt 1 ] && sed -i.bak 1d '` + bin + `/stats'
    [ -z "$n" ] && exit 1
    printf '{"jsonrpc": "2.0", "result": {\n\t\t"current":\t%s,\n\t\t"waiting":\t0\n\t}, "id": 1}\n' "$n" ;;
  *) echo '{"jsonrpc": "2.0", "result": "ok", "id": 1}' ;;
esac
`
	for file, body := range map[string]string{"wget": wget, "nc": "#!/bin/sh\nexit " + ncExit + "\n", "sleep": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, file), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", filepath.Join(dir, name))
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "POD_IP=10.0.0.5", "KAMAILIO_RPC_PORT=5070", "KAMAILIO_RPC_TOKEN=tok"}, env...)
	b, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	c, _ := os.ReadFile(filepath.Join(bin, "calls"))
	return code, string(b), string(c), marker
}

func TestKamailioReadyScript(t *testing.T) {
	if code, out, _, _ := runKamailioScript(t, kamailioReadyScriptName, nil, "0"); code != 0 {
		t.Fatalf("SIP port open: exit %d %q", code, out)
	}
	if code, out, _, _ := runKamailioScript(t, kamailioReadyScriptName, nil, "1", "KAMAILIO_SIP_PORT=5080"); code != 1 || !strings.Contains(out, "5080 not answering") {
		t.Fatalf("SIP port closed: exit %d %q", code, out)
	}
}

func TestKamailioDrainScript(t *testing.T) {
	code, out, calls, marker := runKamailioScript(t, kamailioDrainScriptName, []string{"2", "1", "0"}, "0")
	if code != 0 || !strings.Contains(out, "no open transactions") {
		t.Fatalf("exit %d %q", code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the drain marker makes the pod unready")
	}
	first, _, _ := strings.Cut(calls, "\n")
	for _, want := range []string{"Authorization: Bearer tok", `"method":"pv.shvSet","params":["draining","int",1]`, "http://10.0.0.5:5070/RPC"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first call %q lacks %q", first, want)
		}
	}
	if n := strings.Count(calls, "tm.stats"); n != 3 {
		t.Fatalf("waited through %d tm.stats calls, want 3", n)
	}
}

func TestKamailioDrainScriptStops(t *testing.T) {
	if code, out, _, _ := runKamailioScript(t, kamailioDrainScriptName, []string{"4"}, "0", "KAMAILIO_MAX_DRAIN_SECONDS=0"); code != 0 || !strings.Contains(out, "drain limit of 0s reached with 4 transactions") {
		t.Fatalf("at the limit: exit %d %q", code, out)
	}
	if code, out, _, _ := runKamailioScript(t, kamailioDrainScriptName, []string{""}, "0"); code != 0 || !strings.Contains(out, "not answering") {
		t.Fatalf("Kamailio gone: exit %d %q", code, out)
	}
}
