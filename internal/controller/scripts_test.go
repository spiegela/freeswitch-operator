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
	for file, body := range map[string]string{startupScriptName: startupScript, readyScriptName: readyScript} {
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
	fake := "#!/bin/sh\ncase \"$2\" in\n  status) cat '" + bin + "/status.out' ;;\n  'sofia status') cat '" + bin + "/sofia.out' ;;\nesac\n"
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
