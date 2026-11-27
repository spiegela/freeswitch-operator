package fleet

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestCurrentWaitsForKamailio(t *testing.T) {
	base := Status{Generation: 2, ObservedGeneration: 2, Want: 2, Ready: 2, Updated: 2, UpdateRevision: "r2",
		Members: []Member{{Pod: "edge-0", State: "Active", Revision: "r2"}, {Pod: "edge-1", State: "Active", Revision: "r2"}}}
	for _, tc := range []struct {
		name    string
		proxies []Proxy
		want    bool
	}{
		{"no Kamailio dispatches to it", nil, true},
		{"Kamailio holds a list of every node", []Proxy{{Name: "proxy", Destinations: 2, Loaded: true}}, true},
		{"Kamailio still reloading", []Proxy{{Name: "proxy", Destinations: 2}}, false},
		{"Kamailio's list is behind the fleet", []Proxy{{Name: "proxy", Destinations: 1, Loaded: true}}, false},
		{"one of two Kamailios behind", []Proxy{{Name: "a", Destinations: 2, Loaded: true}, {Name: "b", Destinations: 2}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Proxies = tc.proxies
			if got := s.Current(); got != tc.want {
				t.Fatalf("Current() = %t, want %t", got, tc.want)
			}
		})
	}
}

// A fleet recovers its calls when its env turns call tracking on with a database, and spec.recovery is set.
func TestRecoversCalls(t *testing.T) {
	track := map[string]any{"name": "TRACK_CALLS", "value": "true"}
	dsn := map[string]any{"name": "CORE_DB_DSN", "value": "pgsql://hostaddr=10.0.0.1 dbname=freeswitch"}
	dsnFrom := map[string]any{"name": "CORE_DB_DSN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "db", "key": "dsn"}}}
	for _, tc := range []struct {
		name     string
		env      []any
		recovery bool
		want     bool
	}{
		{"tracked and recovered", []any{track, dsn}, true, true},
		{"DSN from a Secret", []any{track, dsnFrom}, true, true},
		{"no recovery", []any{track, dsn}, false, false},
		{"not tracked", []any{dsn}, true, false},
		{"no database", []any{track}, true, false},
	} {
		spec := map[string]any{"image": "freeswitch:dev", "env": tc.env}
		if tc.recovery {
			spec["recovery"] = map[string]any{}
		}
		fsc := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "sip.instruments.com/v1alpha1", "kind": "FreeswitchCluster",
			"metadata": map[string]any{"name": "edge", "namespace": "telephony"}, "spec": spec,
		}}
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{fscResource: "FreeswitchClusterList"}, fsc)
		got, err := (&kube{ns: "telephony", name: "edge", dyn: dyn}).RecoversCalls(context.Background())
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}
