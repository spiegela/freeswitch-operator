package controller

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"

	sipv1alpha1 "github.com/spiegela/freeswitch-operator/api/v1alpha1"
)

//go:embed kamailio.cfg.tmpl
var kamailioConfigTemplate string

var kamailioConfig = template.Must(template.New("kamailio.cfg").Parse(kamailioConfigTemplate))

// kamailioConfigData is what the rendered kamailio.cfg depends on. Anything here is on the restart list: a change
// rolls the Kamailio pods. Per-pod values (the advertised and private addresses) are passed with -A at start, and
// the destinations live in the dispatcher list, which reloads live.
type kamailioConfigData struct {
	Name           string
	SIPPort        int32
	RPCPort        int32
	MetricsPort    int32
	DispatcherList string
	// Sets are the dispatcher set IDs in priority order.
	Sets []int32
	// TrustedPeers are the sources that may send requests, besides the dispatched nodes. Empty trusts every source.
	TrustedPeers []kamailioTrustedPeer
}

// kamailioTrustedPeer is one spec.trustedPeers entry as the config uses it.
type kamailioTrustedPeer struct {
	Name string
	// Subnets is the peer's CIDRs, comma-separated, as ipops' is_in_subnet takes them.
	Subnets string
}

// kamailioTrustedPeers turns spec.trustedPeers into config data. The API validates names and CIDRs, so nothing here
// can break the config's quoting.
func kamailioTrustedPeers(peers []sipv1alpha1.TrustedPeer) []kamailioTrustedPeer {
	out := make([]kamailioTrustedPeer, 0, len(peers))
	for _, p := range peers {
		out = append(out, kamailioTrustedPeer{Name: p.Name, Subnets: strings.Join(p.Addresses, ",")})
	}
	return out
}

// Rules is the ds_select_routes rule string: every set, in priority order, round-robin (algorithm 4) within each.
func (d kamailioConfigData) Rules() string {
	rules := make([]string, len(d.Sets))
	for i, id := range d.Sets {
		rules[i] = strconv.Itoa(int(id)) + "=4"
	}
	return strings.Join(rules, ";")
}

func renderKamailioConfig(d kamailioConfigData) (string, error) {
	var b strings.Builder
	if err := kamailioConfig.Execute(&b, d); err != nil {
		return "", fmt.Errorf("rendering kamailio.cfg: %w", err)
	}
	return b.String(), nil
}
