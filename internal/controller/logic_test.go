package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
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
	eps := []discoveryv1.EndpointSlice{{
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
	got := readyDestinations(eps, 5060, "udp")
	want := []destination{
		{URI: "sip:10.0.1.10:5060", Pod: "edge-0", Node: "n1"},
		{URI: "sip:10.0.1.30:5060", Pod: "edge-2", Node: "n3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A starting pod's address is admitted to Kamailio while it recovers its calls, before it is ready: its own IP once
// the kubelet reports it, its node's internal address from the moment it is placed.
func TestStartingHosts(t *testing.T) {
	node := func(name string, addrs ...corev1.NodeAddress) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Addresses: addrs}}
	}
	internal := func(a string) corev1.NodeAddress { return corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: a} }
	nodes := map[string]*corev1.Node{
		"n1": node("n1", internal("10.0.1.10")),
		"n3": node("n3", internal("10.0.1.30")),
		"n4": node("n4", internal("10.0.1.40"), corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.4"}),
		"n5": node("n5", internal("10.0.1.50")),
	}
	pod := func(name, nodeName string, ready bool, ip string) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{NodeName: nodeName, HostNetwork: true}}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		if ip != "" {
			p.Status.PodIPs = []corev1.PodIP{{IP: ip}}
		}
		return p
	}
	draining := pod("edge-1", "n2", false, "10.0.1.20")
	draining.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	unscheduled := pod("edge-5", "", false, "")
	pods := []corev1.Pod{
		pod("edge-0", "n1", true, "10.0.1.10"),
		draining,
		pod("edge-2", "n3", false, "10.0.1.30"), // started, its IP reported
		pod("edge-3", "n4", false, ""),          // placed, not started: its node's internal address
		unscheduled,
		pod("edge-6", "gone", false, ""), // its node is unknown
	}
	if got, want := startingHosts(pods, nodes), []string{"10.0.1.30", "10.0.1.40"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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

func TestDispatcherHosts(t *testing.T) {
	got := dispatcherHosts([]string{"sip:10.0.0.1:5060", "sip:10.0.0.2;transport=udp", "sip:[2001:db8::1]:5060", "sip:10.0.0.1:5080", "nonsense"})
	if want := []string{"10.0.0.1", "10.0.0.2", "2001:db8::1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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
			Config: sipv1alpha1.ConfigSpec{ConfigMapRefs: []sipv1alpha1.ConfigMapRef{{Name: "edge-config"}}},
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
	sts := buildStatefulSet(fsc, testLayout(t, fsc))
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
	if c.Lifecycle.PostStart != nil {
		t.Fatal("without spec.recovery nothing runs after FreeSWITCH starts")
	}
	if c.ReadinessProbe == nil || !strings.HasSuffix(c.ReadinessProbe.Exec.Command[1], readyScriptName) {
		t.Fatal("readiness must run the ready script")
	}
	// Unready on the first failed check, so a drain takes the pod out at once; with room for the script's retry
	// (three fs_cli calls of at most 1.5 s).
	if p := c.ReadinessProbe; p.FailureThreshold != 1 || p.TimeoutSeconds*1000 < 3*1500 {
		t.Fatalf("readiness probe %+v", p)
	}
	if c.StartupProbe == nil || !strings.HasSuffix(c.StartupProbe.Exec.Command[1], startupScriptName) {
		t.Fatal("startup must run the startup script")
	}
	if got := c.StartupProbe.PeriodSeconds * c.StartupProbe.FailureThreshold; got != startupGraceSeconds {
		t.Fatalf("startup window %ds, want %ds", got, startupGraceSeconds)
	}
	if c.LivenessProbe != nil {
		t.Fatal("no liveness probe unless asked for: a false positive drops every call on the node")
	}
	for _, e := range c.Env {
		if e.Name == "FS_LIVENESS_TIMEOUT_SECONDS" {
			t.Fatal("liveness env without liveness")
		}
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
	for _, path := range []string{scriptsDir, podInfoDir, configDir} {
		if _, ok := mounts[path]; !ok {
			t.Errorf("missing mount %s", path)
		}
	}
}

func TestLivenessWhenAskedFor(t *testing.T) {
	fsc := testCluster()
	fsc.Spec.Liveness = &sipv1alpha1.LivenessSpec{TimeoutSeconds: 20}
	c := buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec.Containers[0]
	p := c.LivenessProbe
	if p == nil || !strings.HasSuffix(p.Exec.Command[1], liveScriptName) {
		t.Fatal("liveness must run the live script")
	}
	if p.PeriodSeconds != 30 || p.TimeoutSeconds != 20 || p.FailureThreshold != 10 || p.SuccessThreshold != 1 {
		t.Fatalf("probe %+v", p)
	}
	found := false
	for _, e := range c.Env {
		found = found || (e.Name == "FS_LIVENESS_TIMEOUT_SECONDS" && e.Value == "20")
	}
	if !found {
		t.Fatalf("live script needs the probe timeout: %v", c.Env)
	}
}

func TestTemplateHashChangesWithImage(t *testing.T) {
	a := buildStatefulSet(testCluster(), testLayout(t, testCluster()))
	fsc := testCluster()
	fsc.Spec.Image = "example/freeswitch:2"
	b := buildStatefulSet(fsc, testLayout(t, fsc))
	ha, _ := templateHash(&a.Spec.Template)
	hb, _ := templateHash(&b.Spec.Template)
	ha2, _ := templateHash(&buildStatefulSet(testCluster(), testLayout(t, testCluster())).Spec.Template)
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
	if !strings.Contains(readyScript, startupScriptName) || !strings.Contains(buildScriptsConfigMap(testCluster()).Data[startupScriptName], "sofia status") {
		t.Fatal("ready script must run the startup checks, shipped beside it")
	}
	if !strings.Contains(drainScript, "fsctl pause inbound") || !strings.Contains(drainScript, `session\(s\) - peak`) {
		t.Fatal("drain script must pause inbound calls and wait on FreeSWITCH's session count")
	}
}

// fakeKamailio answers like Kamailio's jsonrpcs: dispatcher.reload copies the file on disk into memory, every
// destination active, and dispatcher.list shows memory, or HTTP 500 "No Destination Sets" when it holds none.
// dispatcher.set_state "ip" makes a destination inactive.
type fakeKamailio struct {
	calls    []map[string]any
	onDisk   map[int][]string
	loaded   map[int][]string
	inactive map[string]bool // by URI
	fail     string          // a JSON-RPC error message to answer every call with
	busy     bool            // refuse dispatcher.reload as too soon after the last one
}

func (k *fakeKamailio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	k.calls = append(k.calls, req)
	rpcErr := func(msg string) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":500,"message":%q},"id":1}`, msg)
	}
	if k.fail != "" {
		rpcErr(k.fail)
		return
	}
	switch req["method"] {
	case "dispatcher.reload":
		if k.busy {
			rpcErr("Ongoing reload")
			return
		}
		k.loaded, k.inactive = k.onDisk, nil
	case "dispatcher.set_state":
		params, _ := req["params"].([]any)
		if len(params) == 3 && params[0] == "ip" {
			if k.inactive == nil {
				k.inactive = map[string]bool{}
			}
			k.inactive[params[2].(string)] = true
		}
	case "dispatcher.list":
		if len(k.loaded) == 0 {
			rpcErr("No Destination Sets")
			return
		}
		var records []any
		for id, uris := range k.loaded {
			var targets []any
			for _, u := range uris {
				flags := "AP"
				if k.inactive[u] {
					flags = "IP"
				}
				targets = append(targets, map[string]any{"DEST": map[string]any{"URI": u, "FLAGS": flags, "PRIORITY": 0}})
			}
			records = append(records, map[string]any{"SET": map[string]any{"ID": id, "TARGETS": targets}})
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "result": map[string]any{"NRSETS": len(records), "RECORDS": records}, "id": 1})
		_, _ = w.Write(out)
		return
	}
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"ok","id":1}`))
}

func TestJSONRPCNotifier(t *testing.T) {
	k := &fakeKamailio{}
	srv := httptest.NewServer(k)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n := NewJSONRPCNotifier(srv.URL, "")

	if err := n.SetInactive(ctx, 1, "sip:10.0.0.1:5060"); err != nil {
		t.Fatal(err)
	}
	if k.calls[0]["method"] != "dispatcher.set_state" {
		t.Fatalf("first call %v", k.calls[0])
	}
	params := k.calls[0]["params"].([]any)
	if params[0] != "ip" || params[1] != float64(1) || params[2] != "sip:10.0.0.1:5060" {
		t.Fatalf("set_state params %v", params)
	}

	if err := n.Admit(ctx, []string{"10.0.0.3"}); err != nil {
		t.Fatal(err)
	}
	if c := k.calls[len(k.calls)-1]; c["method"] != "htable.seti" || !reflect.DeepEqual(c["params"], []any{kamailioAdmittedTable, "10.0.0.3", float64(1)}) {
		t.Fatalf("admit: %v", c)
	}

	want := []string{"sip:10.0.0.2:5060", "sip:10.0.0.1:5060"}
	if err := n.Reload(ctx, map[int32][]string{1: want}); !errors.Is(err, errListNotLoaded) {
		t.Fatalf("the kubelet hasn't written the file yet, so Kamailio holds nothing: want errListNotLoaded, got %v", err)
	}
	if err := n.Reload(ctx, map[int32][]string{1: nil}); err != nil {
		t.Fatalf("an empty list loaded is an empty list: %v", err)
	}

	k.onDisk = map[int][]string{1: {"sip:10.0.0.1:5060", "sip:10.0.0.2:5060"}, 2: {"sip:10.9.9.9:5060"}}
	if err := n.Reload(ctx, map[int32][]string{1: want}); err != nil {
		t.Fatalf("set 1 holds the same URIs in another order, and other sets don't count: %v", err)
	}
	if got := k.calls[len(k.calls)-2]["method"]; got != "dispatcher.reload" {
		t.Fatalf("reload before list, got %v", got)
	}
	if err := n.Reload(ctx, map[int32][]string{1: want[:1]}); !errors.Is(err, errListNotLoaded) {
		t.Fatalf("one destination too many: want errListNotLoaded, got %v", err)
	}

	k.busy = true
	if err := n.Reload(ctx, map[int32][]string{1: want}); err != nil {
		t.Fatalf("a reload refused as too soon after the last, which loaded the list, is confirmed: %v", err)
	}
	k.onDisk = map[int][]string{1: {"sip:10.0.0.3:5060"}}
	if err := n.Reload(ctx, map[int32][]string{1: {"sip:10.0.0.3:5060"}}); !errors.Is(err, errListNotLoaded) {
		t.Fatalf("a refused reload that left the old list must be retried: want errListNotLoaded, got %v", err)
	}
	k.busy = false

	k.fail = "boom"
	err := n.Reload(ctx, map[int32][]string{1: want})
	if err == nil || !strings.Contains(err.Error(), "kamailio dispatcher.reload: 500 boom") || errors.Is(err, errListNotLoaded) {
		t.Fatalf("expected the JSON-RPC error, got %v", err)
	}
}

// A reload must not put back into rotation a node Kamailio had taken out (its probes failed, or the operator set it
// inactive): Kamailio's own reload makes every destination active again.
func TestJSONRPCNotifierKeepsInactiveDestinationsAcrossReload(t *testing.T) {
	dead, live, gone := "sip:10.0.0.1:5060", "sip:10.0.0.2:5060", "sip:10.0.0.3:5060"
	k := &fakeKamailio{
		onDisk:   map[int][]string{1: {dead, live}},
		loaded:   map[int][]string{1: {dead, live, gone}},
		inactive: map[string]bool{dead: true, gone: true},
	}
	srv := httptest.NewServer(k)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n := NewJSONRPCNotifier(srv.URL, "")

	if err := n.Reload(ctx, map[int32][]string{1: {dead, live}}); err != nil {
		t.Fatal(err)
	}
	if !k.inactive[dead] || k.inactive[live] {
		t.Fatalf("after the reload: inactive %v, want only %s", k.inactive, dead)
	}
	var setStates []any
	for _, c := range k.calls {
		if c["method"] == "dispatcher.set_state" {
			setStates = append(setStates, c["params"])
		}
	}
	if len(setStates) != 1 {
		t.Fatalf("set_state calls %v: want one, for %s only (%s left the list)", setStates, dead, gone)
	}

	// Kamailio refused the reload as too soon after the last: nothing was reset, so nothing is set again.
	k.calls, k.busy = nil, true
	if err := n.Reload(ctx, map[int32][]string{1: {dead, live}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range k.calls {
		if c["method"] == "dispatcher.set_state" {
			t.Fatalf("set_state after a refused reload: %v", c)
		}
	}
}

func TestJSONRPCNotifierSendsTheToken(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer tok" {
			// What the rendered kamailio.cfg answers without the pod's token.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"ok","id":1}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := NewJSONRPCNotifier(srv.URL, "tok").SetInactive(ctx, 1, "sip:10.0.0.1:5060"); err != nil {
		t.Fatal(err)
	}
	err := NewJSONRPCNotifier(srv.URL, "").SetInactive(ctx, 1, "sip:10.0.0.1:5060")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("without the token: %v", err)
	}
	if got[0] != "Bearer tok" || got[1] != "" {
		t.Fatalf("Authorization headers %q", got)
	}
}

func TestBuildStatefulSetExporter(t *testing.T) {
	fsc := &sipv1alpha1.FreeswitchCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "telephony"},
		// No external address, so no address wait: the exporter is the only init container there may be.
		Spec: sipv1alpha1.FreeswitchClusterSpec{Image: "freeswitch:test", ExternalAddress: sipv1alpha1.ExternalAddressSpec{Source: sipv1alpha1.ExternalAddressNone}},
	}
	if n := len(buildStatefulSet(fsc, configLayout{mountPath: configDir}).Spec.Template.Spec.InitContainers); n != 0 {
		t.Fatalf("no spec.metrics: %d init containers, want none", n)
	}

	fsc.Spec.Metrics = &sipv1alpha1.FreeswitchMetricsSpec{
		ExporterImage: "freeswitch-exporter:test",
		EventSocket: sipv1alpha1.EventSocketSpec{PasswordSecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "esl"}, Key: "password",
		}},
	}
	spec := buildStatefulSet(fsc, configLayout{mountPath: configDir}).Spec.Template.Spec
	if len(spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want the exporter", len(spec.InitContainers))
	}
	c := spec.InitContainers[0]
	if c.Name != "exporter" || c.Image != "freeswitch-exporter:test" {
		t.Errorf("exporter container %q image %q", c.Name, c.Image)
	}
	if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("the exporter must be a native sidecar (restartPolicy Always), so it outlives FreeSWITCH's drain")
	}
	if len(c.Ports) != 1 || c.Ports[0].Name != "metrics" || c.Ports[0].ContainerPort != 9282 || c.Ports[0].HostPort != 9282 {
		t.Errorf("ports = %+v, want metrics on 9282", c.Ports)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if env["ESL_PORT"].Value != "8021" || env["METRICS_PORT"].Value != "9282" {
		t.Errorf("env = %+v", c.Env)
	}
	if env["POD_IP"].ValueFrom == nil || env["POD_IP"].ValueFrom.FieldRef.FieldPath != "status.podIP" {
		t.Error("the exporter must listen on the pod's own address")
	}
	if ref := env["ESL_PASSWORD"].ValueFrom; ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "esl" {
		t.Error("ESL_PASSWORD must come from the Secret")
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsNonRoot == nil || !*c.SecurityContext.RunAsNonRoot {
		t.Error("the exporter runs as non-root")
	}
}

func TestAddressWaitOnlyForRequiredAddresses(t *testing.T) {
	waits := func(mutate func(*sipv1alpha1.FreeswitchCluster)) []corev1.Container {
		fsc := testCluster()
		mutate(fsc)
		var out []corev1.Container
		for _, c := range buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec.InitContainers {
			if c.Name == addressWaitContainerName {
				out = append(out, c)
			}
		}
		return out
	}
	pool := func(f *sipv1alpha1.FreeswitchCluster) {
		f.Spec.ExternalAddress = sipv1alpha1.ExternalAddressSpec{
			Source:         sipv1alpha1.ExternalAddressAddressPool,
			AddressPoolRef: &sipv1alpha1.AddressPoolReference{Name: "edge-media"},
		}
	}

	got := waits(pool)
	if len(got) != 1 {
		t.Fatalf("pool-sourced fleet: %d wait containers, want 1", len(got))
	}
	c := got[0]
	if c.Image != testCluster().Spec.Image || c.Command[0] != shell || c.Command[2] != addressWaitScript {
		t.Errorf("wait container runs %v in %s", c.Command, c.Image)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != volumePodInfo || c.VolumeMounts[0].MountPath != podInfoDir {
		t.Errorf("wait container mounts %+v, want the pod-info volume", c.VolumeMounts)
	}

	// It must come before the exporter, so nothing starts until the address is known.
	fsc := testCluster()
	pool(fsc)
	fsc.Spec.Metrics = &sipv1alpha1.FreeswitchMetricsSpec{ExporterImage: "example/exporter:1"}
	inits := buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec.InitContainers
	if len(inits) != 2 || inits[0].Name != addressWaitContainerName {
		t.Errorf("init containers %v, want the wait first", []string{inits[0].Name, inits[len(inits)-1].Name})
	}

	// The node's own address is written after scheduling too.
	if got := waits(func(*sipv1alpha1.FreeswitchCluster) {}); len(got) != 1 {
		t.Error("NodeExternalIP fleet got no wait container")
	}
	if got := waits(func(f *sipv1alpha1.FreeswitchCluster) {
		f.Spec.ExternalAddress.Source = sipv1alpha1.ExternalAddressNone
	}); len(got) != 0 {
		t.Error("fleet with no external address got a wait container")
	}
	if got := waits(func(f *sipv1alpha1.FreeswitchCluster) {
		pool(f)
		f.Spec.ExternalAddress.Required = ptr.To(false)
	}); len(got) != 0 {
		t.Error("fleet with required: false got a wait container")
	}
}

// spec.recovery runs the recover script as the postStart hook, from the scripts ConfigMap.
func TestBuildStatefulSetRecovery(t *testing.T) {
	fsc := testCluster()
	fsc.Spec.Recovery = &sipv1alpha1.RecoverySpec{FenceImage: "exporter:fence", FenceImagePullPolicy: corev1.PullNever}
	c := buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec.Containers[0]
	if h := c.Lifecycle.PostStart; h == nil || h.Exec == nil || !slices.Equal(h.Exec.Command, []string{shell, scriptsDir + "/" + recoverScriptName}) {
		t.Fatalf("postStart %+v, want the recover script", h)
	}
	if c.Lifecycle.PreStop == nil {
		t.Fatal("the drain must stay")
	}
	if got := buildScriptsConfigMap(fsc).Data[recoverScriptName]; got != recoverScript {
		t.Fatal("the scripts ConfigMap must carry the recover script")
	}
	// What the hook did goes on the run volume, which the exporter reads for its freeswitch_recovery_* series.
	if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == volumeRun && m.MountPath == runDir && !m.ReadOnly }) {
		t.Fatalf("mounts %+v, want the run volume writable at %s", c.VolumeMounts, runDir)
	}
	fsc.Spec.Metrics = &sipv1alpha1.FreeswitchMetricsSpec{ExporterImage: "exporter:test"}
	spec := buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec
	if !slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == volumeRun && v.EmptyDir != nil }) {
		t.Fatalf("volumes %+v, want the run volume, an emptyDir", spec.Volumes)
	}
	// The drain reads the pod's fail-over annotation from the pod-info files, to keep the calls of a member a person
	// fails over.
	failOver := corev1.DownwardAPIVolumeFile{Path: "fail-over", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations['" + sipv1alpha1.FailOverAnnotation + "']"}}
	hasFailOver := func(spec corev1.PodSpec) bool {
		i := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == volumePodInfo })
		return i >= 0 && slices.ContainsFunc(spec.Volumes[i].DownwardAPI.Items, func(f corev1.DownwardAPIVolumeFile) bool { return reflect.DeepEqual(f, failOver) })
	}
	if failOverFile != podInfoDir+"/"+failOver.Path || !hasFailOver(spec) {
		t.Fatalf("volumes %+v, want the fail-over annotation at %s", spec.Volumes, failOverFile)
	}
	i := slices.IndexFunc(spec.InitContainers, func(c corev1.Container) bool { return c.Name == exporterContainerName })
	if i < 0 {
		t.Fatal("no exporter")
	}
	e := spec.InitContainers[i]
	if !slices.ContainsFunc(e.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == volumeRun && m.MountPath == runDir && m.ReadOnly }) ||
		!slices.Contains(e.Env, corev1.EnvVar{Name: "RECOVERED_FILE", Value: recoveredFile}) {
		t.Fatalf("exporter mounts %+v env %+v, want the run volume read-only and RECOVERED_FILE", e.VolumeMounts, e.Env)
	}
	// Without recovery there is no run volume, and the exporter reads no file.
	fsc.Spec.Recovery = nil
	spec = buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec
	if slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == volumeRun || v.Name == volumeFenceToken }) || hasFailOver(spec) ||
		slices.ContainsFunc(spec.InitContainers, func(c corev1.Container) bool { return c.Name == fenceContainerName }) {
		t.Fatal("a run volume, the fence or the fail-over file without recovery")
	}
	e = spec.InitContainers[slices.IndexFunc(spec.InitContainers, func(c corev1.Container) bool { return c.Name == exporterContainerName })]
	if len(e.VolumeMounts) != 0 || slices.ContainsFunc(e.Env, func(v corev1.EnvVar) bool { return v.Name == "RECOVERED_FILE" }) {
		t.Fatalf("exporter mounts %+v env %+v without recovery", e.VolumeMounts, e.Env)
	}
}

// With spec.recovery the drain gets its fence check, installed by an init container from fenceImage.
func TestBuildStatefulSetFence(t *testing.T) {
	fsc := testCluster()
	fsc.Spec.Recovery = &sipv1alpha1.RecoverySpec{FenceImage: "exporter:fence", FenceImagePullPolicy: corev1.PullNever}
	spec := buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec
	c := spec.Containers[0]
	// The drain's fence check: copied onto the run volume by the first init container, from fenceImage, and reading
	// the pod's own token and the cluster's CA from a projected volume.
	if f := spec.InitContainers[0]; f.Name != fenceContainerName || f.Image != "exporter:fence" || f.ImagePullPolicy != corev1.PullNever ||
		!slices.Equal(f.Command, []string{"/freeswitch-fence", "install", fenceFile}) ||
		!slices.ContainsFunc(f.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == volumeRun && m.MountPath == runDir && !m.ReadOnly }) {
		t.Fatalf("first init container %+v, want freeswitch-fence installed onto the run volume", f)
	}
	if !strings.HasPrefix(fenceFile, runDir+"/") {
		t.Fatalf("fence at %s, off the run volume", fenceFile)
	}
	tv := spec.Volumes[slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == volumeFenceToken })]
	if p := tv.Projected; p == nil || len(p.Sources) != 2 || p.Sources[0].ServiceAccountToken == nil || p.Sources[0].ServiceAccountToken.Path != "token" ||
		p.Sources[0].ServiceAccountToken.Audience != "" || p.Sources[1].ConfigMap == nil || p.Sources[1].ConfigMap.Name != "kube-root-ca.crt" {
		t.Fatalf("fence token volume %+v", tv)
	}
	if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == volumeFenceToken && m.MountPath == fenceTokenDir && m.ReadOnly
	}) {
		t.Fatalf("mounts %+v, want the fence token at %s", c.VolumeMounts, fenceTokenDir)
	}
	// A fleet stored before fenceImage was required gets no fence, rather than a container without an image that
	// the API server would refuse; its drain then finds no fence and drains as before.
	fsc.Spec.Recovery.FenceImage = ""
	if slices.ContainsFunc(buildStatefulSet(fsc, testLayout(t, fsc)).Spec.Template.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == fenceContainerName }) {
		t.Fatal("a fence without a fenceImage")
	}
}
