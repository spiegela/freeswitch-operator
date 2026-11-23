package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

func testKamailioCluster() *sipv1alpha1.KamailioCluster {
	return &sipv1alpha1.KamailioCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "telephony"},
		Spec: sipv1alpha1.KamailioClusterSpec{
			Image:    "kamailio:test",
			Dispatch: []sipv1alpha1.DispatchTarget{{FreeswitchCluster: "edge"}},
		},
	}
}

func TestResolveKamailioDefaults(t *testing.T) {
	cfg := resolveKamailio(testKamailioCluster())
	if cfg.replicas != 2 || cfg.sipPort != 5060 || cfg.rpcPort != 5070 || cfg.metricsPort != 5071 || cfg.maxDrainSeconds != 40 || cfg.reloadDelaySecs != 75 || !cfg.requireAddress {
		t.Fatalf("defaults %+v", cfg)
	}
	kc := testKamailioCluster()
	kc.Spec.ExternalAddress.Source = sipv1alpha1.ExternalAddressNone
	if resolveKamailio(kc).requireAddress {
		t.Fatal("source None never waits for an address")
	}
}

func TestBuildKamailioStatefulSet(t *testing.T) {
	kc := testKamailioCluster()
	sts := buildKamailioStatefulSet(kc, "proxy-kamailio-cfg-abc", "abc")
	if sts.Name != "proxy-kamailio" || sts.Spec.UpdateStrategy.Type != "OnDelete" {
		t.Fatalf("name %s, strategy %s", sts.Name, sts.Spec.UpdateStrategy.Type)
	}
	if sts.Spec.Selector.MatchLabels[sipv1alpha1.KamailioClusterLabel] != "proxy" || sts.Spec.Selector.MatchLabels[sipv1alpha1.ClusterLabel] != "" {
		t.Fatalf("selector %v: Kamailio pods carry their own label, never the FreeSWITCH one", sts.Spec.Selector.MatchLabels)
	}
	spec := sts.Spec.Template.Spec
	if !spec.HostNetwork || spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace {
		t.Fatal("host network and a shared process namespace")
	}
	if *spec.TerminationGracePeriodSeconds != 50 {
		t.Fatalf("grace %d", *spec.TerminationGracePeriodSeconds)
	}
	c := spec.Containers[0]
	if !strings.HasSuffix(c.Command[1], kamailioStartScriptName) || !strings.HasSuffix(c.ReadinessProbe.Exec.Command[1], kamailioReadyScriptName) || !strings.HasSuffix(c.Lifecycle.PreStop.Exec.Command[1], kamailioDrainScriptName) {
		t.Fatal("start, ready and drain scripts")
	}
	if c.LivenessProbe != nil {
		t.Fatal("no liveness probe")
	}
	var token *corev1.SecretKeySelector
	for _, e := range c.Env {
		if e.Name == "KAMAILIO_RPC_TOKEN" && e.ValueFrom != nil {
			token = e.ValueFrom.SecretKeyRef
		}
	}
	if token == nil || token.Name != "proxy-jsonrpc" || token.Key != kamailioTokenKey {
		t.Fatalf("token from the Secret, got %+v", token)
	}
}

func TestKamailioPodMountsAndPorts(t *testing.T) {
	spec := buildKamailioStatefulSet(testKamailioCluster(), "proxy-kamailio-cfg-abc", "abc").Spec.Template.Spec
	c := spec.Containers[0]
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["config"] != kamailioConfigDir || mounts["dispatcher"] != kamailioDispatcherDir {
		t.Fatalf("mounts %v", mounts)
	}
	for _, v := range spec.Volumes {
		switch v.Name {
		case "config":
			if v.ConfigMap.Name != "proxy-kamailio-cfg-abc" {
				t.Fatalf("config snapshot %s", v.ConfigMap.Name)
			}
		case "dispatcher":
			if v.ConfigMap.Name != "proxy-dispatcher" || v.ConfigMap.Items != nil {
				t.Fatal("the dispatcher list is mounted whole, as a directory, so it updates in place")
			}
		}
	}
	ports := map[string]int32{}
	for _, p := range c.Ports {
		ports[p.Name] = p.HostPort
	}
	if ports["sip-udp"] != 5060 || ports["rpc"] != 5070 || ports["metrics"] != 5071 {
		t.Fatalf("ports %v", ports)
	}
}

func TestKamailioConfigChangeChangesTemplate(t *testing.T) {
	a, _ := templateHash(&buildKamailioStatefulSet(testKamailioCluster(), "proxy-kamailio-cfg-a", "a").Spec.Template)
	b, _ := templateHash(&buildKamailioStatefulSet(testKamailioCluster(), "proxy-kamailio-cfg-b", "b").Spec.Template)
	if a == b {
		t.Fatal("a new config must be a new template, so the fleet rolls")
	}
}

func TestCombineDispatcherLists(t *testing.T) {
	edge := renderDispatcherList(1, []destination{{URI: "sip:10.0.0.1:5060", Pod: "edge-0", Node: "n1"}, {URI: "sip:10.0.0.2:5060", Pod: "edge-1"}})
	// A list holding another set's lines too, as a hand-edited one might: only the fleet's own set is taken.
	overflow := renderDispatcherList(2, []destination{{URI: "sip:10.0.1.1:5080;transport=tcp"}}) + "1 sip:10.9.9.9:5060 0 0 0\n"
	content, uris := combineDispatcherLists([]dispatchSet{
		{cluster: "overflow", setID: 2, list: overflow},
		{cluster: "edge", setID: 1, list: edge},
		{cluster: "new", setID: 3},
	})
	want := []string{
		"2 sip:10.0.1.1:5080;transport=tcp 8 0 0",
		"1 sip:10.0.0.1:5060 8 0 duid=edge-0;node=n1",
		"1 sip:10.0.0.2:5060 8 0 duid=edge-1",
	}
	var got []string
	for line := range strings.SplitSeq(content, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			got = append(got, line)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("combined list:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(uris[1]) != 2 || len(uris[2]) != 1 || uris[3] == nil || len(uris[3]) != 0 {
		t.Fatalf("uris %v: a set with no list yet is there, and empty", uris)
	}
	if !sameURIs(parseDispatcherURIs(content, 1), uris[1]) {
		t.Fatal("the combined list parses back")
	}
}
