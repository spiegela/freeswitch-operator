package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kamailioConfigCases are rendered by TestKamailioConfigRenders and, with KAMAILIO_CFG_OUT set, written out (with
// the pod scripts, under scripts/) for make check-kamailio-cfg to run through kamailio -c in the pinned image.
var kamailioConfigCases = map[string]kamailioConfigData{
	"two-sets": {Name: "edge", SIPPort: 5060, RPCPort: 5070, DispatcherList: kamailioDispatcher, Sets: []int32{2, 1}},
	"one-set":  {Name: "edge", SIPPort: 5080, RPCPort: 5071, DispatcherList: kamailioDispatcher, Sets: []int32{1}},
	"no-sets":  {Name: "edge", SIPPort: 5060, RPCPort: 5070, DispatcherList: kamailioDispatcher},
}

func TestKamailioConfigRenders(t *testing.T) {
	out := os.Getenv("KAMAILIO_CFG_OUT")
	if out != "" {
		scripts := filepath.Join(out, "scripts")
		if err := os.MkdirAll(scripts, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range buildKamailioScriptsConfigMap(testKamailioCluster()).Data {
			if err := os.WriteFile(filepath.Join(scripts, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, d := range kamailioConfigCases {
		cfg, err := renderKamailioConfig(d)
		if err != nil {
			t.Fatal(err)
		}
		if out != "" {
			if err := os.WriteFile(filepath.Join(out, name+".cfg"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(cfg, "<no value>") || strings.Contains(cfg, "{{") {
			t.Fatalf("%s: unfilled template value", name)
		}
	}
}

func TestKamailioConfigRouting(t *testing.T) {
	cfg, err := renderKamailioConfig(kamailioConfigCases["two-sets"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ds_select_routes("2=4;1=4", "0")`, // priority order, as spec.dispatch lists them
		`if (ds_is_active("2"))`,
		`if (ds_is_active("1"))`,
		"listen=tcp:PRIVATE_ADDR:RPC_PORT",
		"#!define SIP_PORT 5060",
		`modparam("dispatcher", "ds_probing_mode", 3)`,
		`#!define DISPATCHER_LIST "` + kamailioDispatcher + `"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
	none, err := renderKamailioConfig(kamailioConfigCases["no-sets"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none, "ds_select_routes") || !strings.Contains(none, "spec.dispatch is empty") {
		t.Error("with no sets every new call gets 503")
	}
}
