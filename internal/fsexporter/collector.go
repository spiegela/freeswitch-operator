package fsexporter

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector reads FreeSWITCH over one event socket connection per scrape. When the socket doesn't answer (the
// node is starting, stopping or hung), freeswitch_up is 0 and nothing else is reported, so a dead node never
// shows stale numbers.
type Collector struct {
	Address  string        // host:port of the event socket
	Password string        // event socket password
	Timeout  time.Duration // for connecting and for each command
	Log      *slog.Logger
}

// labelProfile names a SIP profile in the sofia series.
const labelProfile = "profile"

var (
	descUp = prometheus.NewDesc("freeswitch_up",
		"1 when FreeSWITCH's event socket answered and authenticated this scrape, 0 otherwise.", nil, nil)
	descReady = prometheus.NewDesc("freeswitch_ready",
		"1 when FreeSWITCH reports itself ready (it says so before any SIP profile runs; see freeswitch_sofia_profile_up).", nil, nil)
	descInfo = prometheus.NewDesc("freeswitch_info",
		"Always 1; the version label is FreeSWITCH's own.", []string{"version"}, nil)
	descUptime = prometheus.NewDesc("freeswitch_uptime_seconds",
		"Seconds since FreeSWITCH started.", nil, nil)
	descSessionsActive = prometheus.NewDesc("freeswitch_sessions_active",
		"Sessions (call legs) up now.", nil, nil)
	descSessionsPeak = prometheus.NewDesc("freeswitch_sessions_peak",
		"Most sessions up at once since FreeSWITCH started.", nil, nil)
	descSessionsLimit = prometheus.NewDesc("freeswitch_sessions_limit",
		"max-sessions: sessions beyond this are refused.", nil, nil)
	descSessionsTotal = prometheus.NewDesc("freeswitch_sessions_total",
		"Sessions created since FreeSWITCH started.", nil, nil)
	descSessionRateLimit = prometheus.NewDesc("freeswitch_session_rate_limit",
		"sessions-per-second: new sessions beyond this rate are refused.", nil, nil)
	descCalls = prometheus.NewDesc("freeswitch_calls",
		"Bridged calls up now (show calls count).", nil, nil)
	descChannels = prometheus.NewDesc("freeswitch_channels",
		"Channels up now, bridged or not (show channels count).", nil, nil)
	descInboundPaused = prometheus.NewDesc("freeswitch_inbound_paused",
		"1 while new inbound calls are refused (fsctl pause inbound; the operator's drain sets it).", nil, nil)
	descProfileUp = prometheus.NewDesc("freeswitch_sofia_profile_up",
		"1 when the SIP profile is RUNNING, 0 when it is listed in another state.", []string{labelProfile}, nil)
	descProfileCalls = prometheus.NewDesc("freeswitch_sofia_calls_total",
		"Calls through the SIP profile since it started, by direction (in or out).", []string{labelProfile, "direction"}, nil)
	descProfileFailed = prometheus.NewDesc("freeswitch_sofia_failed_calls_total",
		"Calls through the SIP profile that failed since it started, by direction (in or out).", []string{labelProfile, "direction"}, nil)
	descProfileRegistrations = prometheus.NewDesc("freeswitch_sofia_registrations",
		"Registrations held by the SIP profile (as a registrar).", []string{labelProfile}, nil)
)

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descUp, descReady, descInfo, descUptime, descSessionsActive, descSessionsPeak, descSessionsLimit,
		descSessionsTotal, descSessionRateLimit, descCalls, descChannels, descInboundPaused, descProfileUp,
		descProfileCalls, descProfileFailed, descProfileRegistrations,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	cl, err := dialESL(c.Address, c.Password, c.Timeout)
	if err != nil {
		c.logger().Debug("event socket did not answer", "address", c.Address, "error", err)
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		return
	}
	defer func() { _ = cl.Close() }()
	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 1)

	// Each part is reported on its own: one command that fails (a module not loaded, say) leaves the rest. One that
	// times out closes the connection (eslClient.fail), so the rest fail too rather than read its late reply.
	if out, err := cl.API(`json {"command":"status","data":""}`, c.Timeout); err != nil {
		c.logger().Warn("status failed", "error", err)
	} else if st, err := parseStatus(out); err != nil {
		c.logger().Warn("could not parse status", "error", err)
	} else {
		ready := 0.0
		if st.Response.SystemStatus == "ready" {
			ready = 1
		}
		ch <- prometheus.MustNewConstMetric(descReady, prometheus.GaugeValue, ready)
		ch <- prometheus.MustNewConstMetric(descInfo, prometheus.GaugeValue, 1, st.Response.Version)
		ch <- prometheus.MustNewConstMetric(descUptime, prometheus.GaugeValue, st.Response.Uptime.seconds())
		s := st.Response.Sessions
		ch <- prometheus.MustNewConstMetric(descSessionsActive, prometheus.GaugeValue, s.Count.Active)
		ch <- prometheus.MustNewConstMetric(descSessionsPeak, prometheus.GaugeValue, s.Count.Peak)
		ch <- prometheus.MustNewConstMetric(descSessionsLimit, prometheus.GaugeValue, s.Count.Limit)
		ch <- prometheus.MustNewConstMetric(descSessionsTotal, prometheus.CounterValue, s.Count.Total)
		ch <- prometheus.MustNewConstMetric(descSessionRateLimit, prometheus.GaugeValue, s.Rate.Max)
	}
	for _, q := range []struct {
		cmd  string
		desc *prometheus.Desc
	}{{"show calls count", descCalls}, {"show channels count", descChannels}} {
		out, err := cl.API(q.cmd, c.Timeout)
		if err != nil {
			c.logger().Warn("command failed", "command", q.cmd, "error", err)
			continue
		}
		n, err := parseCount(out)
		if err != nil {
			c.logger().Warn("could not parse count", "command", q.cmd, "error", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(q.desc, prometheus.GaugeValue, n)
	}
	if out, err := cl.API("fsctl pause_check inbound", c.Timeout); err != nil {
		c.logger().Warn("pause_check failed", "error", err)
	} else {
		paused := 0.0
		if strings.TrimSpace(out) == "true" {
			paused = 1
		}
		ch <- prometheus.MustNewConstMetric(descInboundPaused, prometheus.GaugeValue, paused)
	}

	out, err := cl.API("sofia xmlstatus", c.Timeout)
	if err != nil {
		c.logger().Warn("sofia xmlstatus failed", "error", err)
		return
	}
	profiles, err := parseProfiles(out)
	if err != nil {
		c.logger().Warn("could not parse sofia xmlstatus", "error", err)
		return
	}
	for _, p := range profiles {
		up := 0.0
		if p.running() {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(descProfileUp, prometheus.GaugeValue, up, p.Label)
		if !p.running() {
			continue
		}
		out, err := cl.API("sofia xmlstatus profile "+p.Name, c.Timeout)
		if err != nil {
			c.logger().Warn("sofia xmlstatus profile failed", "profile", p.Name, "error", err)
			continue
		}
		info, err := parseProfileInfo(out)
		if err != nil {
			c.logger().Warn("could not parse sofia xmlstatus profile", "profile", p.Name, "error", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(descProfileCalls, prometheus.CounterValue, info.CallsIn, p.Label, "in")
		ch <- prometheus.MustNewConstMetric(descProfileCalls, prometheus.CounterValue, info.CallsOut, p.Label, "out")
		ch <- prometheus.MustNewConstMetric(descProfileFailed, prometheus.CounterValue, info.FailedCallsIn, p.Label, "in")
		ch <- prometheus.MustNewConstMetric(descProfileFailed, prometheus.CounterValue, info.FailedCallsOut, p.Label, "out")
		ch <- prometheus.MustNewConstMetric(descProfileRegistrations, prometheus.GaugeValue, info.Registrations, p.Label)
	}
}

func (c *Collector) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// status is the answer to `json {"command":"status"}` (FreeSWITCH 1.6 and later).
type status struct {
	Status   string `json:"status"`
	Response struct {
		SystemStatus string `json:"systemStatus"`
		Version      string `json:"version"`
		Uptime       uptime `json:"uptime"`
		Sessions     struct {
			Count struct {
				Total  float64 `json:"total"`
				Active float64 `json:"active"`
				Peak   float64 `json:"peak"`
				Limit  float64 `json:"limit"`
			} `json:"count"`
			Rate struct {
				Max float64 `json:"max"`
			} `json:"rate"`
		} `json:"sessions"`
	} `json:"response"`
}

type uptime struct {
	Years        float64 `json:"years"`
	Days         float64 `json:"days"`
	Hours        float64 `json:"hours"`
	Minutes      float64 `json:"minutes"`
	Seconds      float64 `json:"seconds"`
	Milliseconds float64 `json:"milliseconds"`
	Microseconds float64 `json:"microseconds"`
}

// seconds adds up FreeSWITCH's uptime fields; it counts a year as 365 days, as FreeSWITCH does.
func (u uptime) seconds() float64 {
	return ((u.Years*365+u.Days)*24+u.Hours)*3600 + u.Minutes*60 + u.Seconds + u.Milliseconds/1e3 + u.Microseconds/1e6
}

func parseStatus(out string) (status, error) {
	var st status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return st, err
	}
	if st.Status != "success" {
		return st, fmt.Errorf("status %q", st.Status)
	}
	return st, nil
}

// parseCount reads "N total." from show ... count.
func parseCount(out string) (float64, error) {
	f := strings.Fields(out)
	if len(f) < 2 || f[1] != "total." {
		return 0, fmt.Errorf("unexpected output %q", out)
	}
	n, err := strconv.Atoi(f[0])
	return float64(n), err
}

type profileEntry struct {
	Name  string `xml:"name"`
	Type  string `xml:"type"`
	Data  string `xml:"data"` // for an alias, the profile it names
	State string `xml:"state"`
	// Label is the name the sofia series report the profile under: its name, or the alias NAME for a profile that
	// runs as NAME-<suffix> with NAME as an alias. Edges name their profiles that way, one name per movable
	// address (trunks-203-0-113-10, alias trunks), so call recovery takes one address's calls at a time
	// (docs/call-tracking.md); the series keep the plain name, the same on every node.
	Label string `xml:"-"`
}

func (p profileEntry) running() bool { return strings.HasPrefix(p.State, "RUNNING") }

// parseProfiles reads the profiles from sofia xmlstatus, leaving out aliases (another name for a profile) and
// gateways, and labels each profile (profileEntry.Label). FreeSWITCH 1.10 lists aliases as <alias> elements; a
// <profile> of type alias counts too.
func parseProfiles(out string) ([]profileEntry, error) {
	var doc struct {
		Profiles []profileEntry `xml:"profile"`
		Aliases  []profileEntry `xml:"alias"`
	}
	if err := decodeXML(out, &doc); err != nil {
		return nil, err
	}
	aliases := map[string][]string{} // profile name: its aliases
	for _, a := range append(doc.Aliases, doc.Profiles...) {
		if a.Type == "alias" {
			aliases[a.Data] = append(aliases[a.Data], a.Name)
		}
	}
	var profiles []profileEntry
	for _, p := range doc.Profiles {
		if p.Type != "profile" {
			continue
		}
		p.Label = p.Name
		for _, a := range aliases[p.Name] {
			if suffix, ok := strings.CutPrefix(p.Name, a+"-"); ok && suffix != "" {
				p.Label = a
				break
			}
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

type profileInfo struct {
	CallsIn        float64 `xml:"profile-info>calls-in"`
	CallsOut       float64 `xml:"profile-info>calls-out"`
	FailedCallsIn  float64 `xml:"profile-info>failed-calls-in"`
	FailedCallsOut float64 `xml:"profile-info>failed-calls-out"`
	Registrations  float64 `xml:"profile-info>registrations"`
}

func parseProfileInfo(out string) (profileInfo, error) {
	var info profileInfo
	return info, decodeXML(out, &info)
}

// decodeXML decodes FreeSWITCH's XML, which declares ISO-8859-1. Every field read here is ASCII, so the bytes are
// read as they are.
func decodeXML(out string, v any) error {
	d := xml.NewDecoder(bytes.NewReader([]byte(out)))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return d.Decode(v)
}
