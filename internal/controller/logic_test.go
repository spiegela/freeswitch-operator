package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	sipv1alpha1 "git.instruments.com/ci/freeswitch-operator/api/v1alpha1"
)

func endpoint(addr, pod, node string, ready, terminating bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses:  []string{addr},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ready), Terminating: ptr.To(terminating)},
		TargetRef:  &corev1.ObjectReference{Kind: "Pod", Name: pod},
		NodeName:   ptr.To(node),
	}
}

func TestReadyDestinationsKeepsOnlyReadyNonTerminating(t *testing.T) {
	slices := []discoveryv1.EndpointSlice{{
		Endpoints: []discoveryv1.Endpoint{
			endpoint("10.0.1.30", "edge-2", "n3", true, false),
			endpoint("10.0.1.10", "edge-0", "n1", true, false),
			endpoint("10.0.1.20", "edge-1", "n2", false, true),  // draining
			endpoint("10.0.1.40", "edge-3", "n4", false, false), // starting
		},
	}, {
		// A duplicate address in a second slice is listed once.
		Endpoints: []discoveryv1.Endpoint{endpoint("10.0.1.10", "edge-0", "n1", true, false)},
	}}
	got := readyDestinations(slices, 5060, "udp")
	want := []destination{
		{URI: "sip:10.0.1.10:5060", Pod: "edge-0", Node: "n1"},
		{URI: "sip:10.0.1.30:5060", Pod: "edge-2", Node: "n3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSIPURI(t *testing.T) {
	cases := map[string]string{
		sipURI("10.0.0.1", 5060, "udp"): "sip:10.0.0.1:5060",
		sipURI("10.0.0.1", 5080, "tcp"): "sip:10.0.0.1:5080;transport=tcp",
		sipURI("fd00::1", 5060, ""):     "sip:[fd00::1]:5060",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestRenderAndParseDispatcherList(t *testing.T) {
	dests := []destination{
		{URI: "sip:10.0.1.10:5060", Pod: "edge-0", Node: "n1"},
		{URI: "sip:10.0.1.30:5060", Pod: "edge-2"},
	}
	list := renderDispatcherList(7, dests)
	if !strings.Contains(list, "7 sip:10.0.1.10:5060 0 0 duid=edge-0;node=n1\n") {
		t.Fatalf("missing first destination:\n%s", list)
	}
	if !strings.Contains(list, "7 sip:10.0.1.30:5060 0 0 duid=edge-2\n") {
		t.Fatalf("missing second destination:\n%s", list)
	}
	uris := parseDispatcherURIs(list, 7)
	if !reflect.DeepEqual(uris, []string{"sip:10.0.1.10:5060", "sip:10.0.1.30:5060"}) {
		t.Fatalf("parsed %v", uris)
	}
	if got := parseDispatcherURIs(list, 1); len(got) != 0 {
		t.Fatalf("other set parsed %v", got)
	}
	if renderDispatcherList(7, dests) != list {
		t.Fatal("rendering is not deterministic")
	}
	empty := renderDispatcherList(1, nil)
	if len(parseDispatcherURIs(empty, 1)) != 0 {
		t.Fatal("empty list should have no destinations")
	}
}

func TestRemovedURIs(t *testing.T) {
	got := removedURIs([]string{"a", "b", "c"}, []string{"b", "d"})
	if !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("got %v", got)
	}
}

func pod(name, rev string, ready, terminating bool) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: rev}}}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestNextPodToReplace(t *testing.T) {
	tests := []struct {
		name        string
		pods        []corev1.Pod
		desired     int32
		wantReplace string
		wantWaiting bool
	}{
		{"nothing outdated", []corev1.Pod{pod("e-0", "new", true, false), pod("e-1", "new", true, false)}, 2, "", false},
		{"highest ordinal first", []corev1.Pod{pod("e-0", "old", true, false), pod("e-1", "old", true, false), pod("e-2", "new", true, false)}, 3, "e-1", false},
		{"unready outdated first", []corev1.Pod{pod("e-0", "old", false, false), pod("e-1", "old", true, false)}, 2, "e-0", false},
		{"wait while one drains", []corev1.Pod{pod("e-0", "old", true, false), pod("e-1", "new", false, true)}, 2, "", true},
		{"wait for an unready updated pod", []corev1.Pod{pod("e-0", "old", true, false), pod("e-1", "new", false, false)}, 2, "", true},
		{"wait for replica count", []corev1.Pod{pod("e-0", "old", true, false)}, 2, "", true},
		{"ordinal 10 beats 9", []corev1.Pod{pod("e-9", "old", true, false), pod("e-10", "old", true, false)}, 2, "e-10", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := nextPodToReplace(tc.pods, "new", tc.desired)
			got := ""
			if d.Replace != nil {
				got = d.Replace.Name
			}
			if got != tc.wantReplace {
				t.Fatalf("replace %q, want %q (waiting %q)", got, tc.wantReplace, d.Waiting)
			}
			if (d.Waiting != "") != tc.wantWaiting {
				t.Fatalf("waiting %q, want waiting=%v", d.Waiting, tc.wantWaiting)
			}
		})
	}
	if d := nextPodToReplace([]corev1.Pod{pod("e-0", "old", true, false)}, "", 1); d.Replace != nil || d.Outdated != 0 {
		t.Fatal("no update revision should mean no rollout")
	}
}

func TestMemberState(t *testing.T) {
	if s := memberState(ptr.To(pod("a", "r", true, false))); s != sipv1alpha1.MemberActive {
		t.Errorf("ready pod: %s", s)
	}
	if s := memberState(ptr.To(pod("a", "r", false, false))); s != sipv1alpha1.MemberStarting {
		t.Errorf("unready pod: %s", s)
	}
	if s := memberState(ptr.To(pod("a", "r", true, true))); s != sipv1alpha1.MemberDraining {
		t.Errorf("terminating pod: %s", s)
	}
}

func testCluster() *sipv1alpha1.FreeswitchCluster {
	return &sipv1alpha1.FreeswitchCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "telephony"},
		Spec: sipv1alpha1.FreeswitchClusterSpec{
			Image:  "example/freeswitch:1",
			Config: sipv1alpha1.ConfigSpec{ConfigMapRefs: []corev1.LocalObjectReference{{Name: "edge-config"}}},
		},
	}
}

func TestResolveDefaults(t *testing.T) {
	cfg := resolve(testCluster())
	if cfg.replicas != 2 || cfg.sipPort != 5060 || cfg.rtpStart != 16384 || cfg.rtpEnd != 32768 || cfg.maxDrainSeconds != 7200 {
		t.Fatalf("defaults: %+v", cfg)
	}
	if !cfg.requireAddress || cfg.addressSource != sipv1alpha1.ExternalAddressNodeExternalIP {
		t.Fatalf("address defaults: %+v", cfg)
	}
	fsc := testCluster()
	fsc.Spec.ExternalAddress.Source = sipv1alpha1.ExternalAddressNone
	fsc.Spec.ExternalAddress.Required = ptr.To(true)
	if resolve(fsc).requireAddress {
		t.Fatal("source None must never require an address")
	}
}

func TestBuildStatefulSet(t *testing.T) {
	fsc := testCluster()
	fsc.Spec.Drain.MaxDrainSeconds = 600
	sts := buildStatefulSet(fsc)
	spec := sts.Spec.Template.Spec

	if !spec.HostNetwork || spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Fatal("pods must use the host network and still resolve cluster names")
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Fatal("the operator must own replacement order")
	}
	if *spec.TerminationGracePeriodSeconds != 600+drainGraceMargin {
		t.Fatalf("grace period %d", *spec.TerminationGracePeriodSeconds)
	}
	anti := spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(anti) != 1 || anti[0].TopologyKey != corev1.LabelHostname {
		t.Fatal("one pod per node must be required")
	}
	c := spec.Containers[0]
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || !strings.HasSuffix(c.Lifecycle.PreStop.Exec.Command[1], drainScriptName) {
		t.Fatal("preStop must run the drain script")
	}
	if c.ReadinessProbe == nil || !strings.HasSuffix(c.ReadinessProbe.Exec.Command[1], readyScriptName) {
		t.Fatal("readiness must run the ready script")
	}
	if c.LivenessProbe != nil {
		t.Fatal("no liveness probe in v0")
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["FS_MAX_DRAIN_SECONDS"] != "600" || env["FS_REQUIRE_EXTERNAL_ADDRESS"] != "true" {
		t.Fatalf("env %v", env)
	}
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	for _, path := range []string{scriptsDir, podInfoDir, configDir + "/edge-config"} {
		if _, ok := mounts[path]; !ok {
			t.Errorf("missing mount %s", path)
		}
	}
}

func TestTemplateHashChangesWithImage(t *testing.T) {
	a := buildStatefulSet(testCluster())
	fsc := testCluster()
	fsc.Spec.Image = "example/freeswitch:2"
	b := buildStatefulSet(fsc)
	ha, _ := templateHash(&a.Spec.Template)
	hb, _ := templateHash(&b.Spec.Template)
	ha2, _ := templateHash(&buildStatefulSet(testCluster()).Spec.Template)
	if ha == hb || ha != ha2 {
		t.Fatalf("hash: %s %s %s", ha, hb, ha2)
	}
}

func TestScriptsReferenceSharedPaths(t *testing.T) {
	if !strings.Contains(drainScript, drainMarker) || !strings.Contains(readyScript, drainMarker) {
		t.Fatal("drain and ready scripts must agree on the drain marker")
	}
	if !strings.Contains(readyScript, externalAddrFile) {
		t.Fatal("ready script must check the external address file")
	}
	if !strings.Contains(drainScript, "fsctl pause inbound") || !strings.Contains(drainScript, "show calls count") {
		t.Fatal("drain script must pause inbound calls and wait on the call count")
	}
}

func TestJSONRPCNotifier(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		got = append(got, req)
		if req["method"] == "dispatcher.reload" && len(got) > 2 {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":500,"message":"boom"},"id":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"ok","id":1}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n := NewJSONRPCNotifier(srv.URL)
	if err := n.SetInactive(ctx, 1, "sip:10.0.0.1:5060"); err != nil {
		t.Fatal(err)
	}
	if err := n.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := n.Reload(ctx); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected a JSON-RPC error, got %v", err)
	}
	if got[0]["method"] != "dispatcher.set_state" {
		t.Fatalf("first call %v", got[0])
	}
	params := got[0]["params"].([]any)
	if params[0] != "ip" || params[1] != float64(1) || params[2] != "sip:10.0.0.1:5060" {
		t.Fatalf("set_state params %v", params)
	}
	if got[1]["method"] != "dispatcher.reload" {
		t.Fatalf("second call %v", got[1])
	}
}
