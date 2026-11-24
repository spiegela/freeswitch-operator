package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

// kamailioConfigCases are rendered by TestKamailioConfigRenders and, with KAMAILIO_CFG_OUT set, written out (with
// the pod scripts, under scripts/) for make check-kamailio-cfg to run through kamailio -c in the pinned image.
var kamailioConfigCases = map[string]kamailioConfigData{
	"two-sets": {Name: "edge", SIPPort: 5060, RPCPort: 5070, MetricsPort: 5071, DispatcherList: kamailioDispatcher, Sets: []int32{2, 1}},
	"one-set":  {Name: "edge", SIPPort: 5080, RPCPort: 5071, MetricsPort: 5072, DispatcherList: kamailioDispatcher, Sets: []int32{1}},
	"no-sets":  {Name: "edge", SIPPort: 5060, RPCPort: 5070, MetricsPort: 5071, DispatcherList: kamailioDispatcher},
	"trusted-peers": {Name: "edge", SIPPort: 5060, RPCPort: 5070, MetricsPort: 5071, DispatcherList: kamailioDispatcher,
		Sets: []int32{1}, TrustedPeers: kamailioTrustedPeers([]sipv1alpha1.TrustedPeer{
			{Name: "carrier", Addresses: []string{"198.51.100.0/30", "203.0.113.8/29"}},
			{Name: "bench", Addresses: []string{"10.244.0.0/16", "2001:db8::/32"}},
		})},
	"capture": {Name: "edge", SIPPort: 5060, RPCPort: 5070, MetricsPort: 5071, DispatcherList: kamailioDispatcher,
		Sets: []int32{1}, Capture: &sipv1alpha1.SIPCaptureSpec{Address: "call-tracer.monitoring.svc.cluster.local:9060", CaptureID: 7}},
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
		"listen=tcp:PRIVATE_ADDR:METRICS_PORT",
		"#!define METRICS_PORT 5071",
		"if (!route(DISPLAY_NAMES))",
		`prom_counter_inc("silent_drops_total", "1", "request")`,
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

func TestKamailioConfigTrustedPeers(t *testing.T) {
	cfg, err := renderKamailioConfig(kamailioConfigCases["trusted-peers"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`loadmodule "ipops.so"`,
		"# Trusted peers: carrier bench",
		`if (!ds_is_from_list() && !route(TRUSTED_PEER)) {`,
		`prom_counter_inc("rejected_requests_total", "1", "untrusted_source");`,
		`if (is_in_subnet("$si", "198.51.100.0/30,203.0.113.8/29")) {`,
		`if (is_in_subnet("$si", "10.244.0.0/16,2001:db8::/32")) {`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
	// The check comes first: an untrusted source gets no answer at all, not even 483 or 416.
	if strings.Index(cfg, "route(TRUSTED_PEER)") > strings.Index(cfg, "mf_process_maxfwd_header") {
		t.Error("the trust check must come before anything that answers")
	}

	open, err := renderKamailioConfig(kamailioConfigCases["one-set"])
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"ipops", "TRUSTED_PEER", "untrusted_source\")"} {
		if strings.Contains(open, unwanted) {
			t.Errorf("with no trusted peers the config has %q", unwanted)
		}
	}
}

func TestKamailioConfigCapture(t *testing.T) {
	cfg, err := renderKamailioConfig(kamailioConfigCases["capture"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`loadmodule "siptrace.so"`,
		`modparam("siptrace", "duplicate_uri", "sip:call-tracer.monitoring.svc.cluster.local:9060")`,
		`modparam("siptrace", "hep_version", 3)`,
		`modparam("siptrace", "hep_capture_id", 7)`,
		`modparam("siptrace", "trace_mode", 1)`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
	d := kamailioConfigCases["capture"]
	d.Capture = &sipv1alpha1.SIPCaptureSpec{Address: "10.0.0.5:9060"}
	if cfg, _ = renderKamailioConfig(d); !strings.Contains(cfg, `modparam("siptrace", "hep_capture_id", 1)`) {
		t.Error("an unset capture ID should be 1")
	}
	none, err := renderKamailioConfig(kamailioConfigCases["one-set"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none, "siptrace") {
		t.Error("without spec.capture the config loads siptrace")
	}
}
