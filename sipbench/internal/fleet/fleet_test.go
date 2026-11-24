package fleet

import "testing"

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
