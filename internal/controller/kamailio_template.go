package controller

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"
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
