package fsexporter

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Answers as FreeSWITCH 1.10.10 gave them on the kind bench.
const (
	statusJSON = `{"command":"status","data":"","status":"success","response":{"systemStatus":"ready","uptime":{"years":0,"days":1,"hours":1,"minutes":28,"seconds":52,"milliseconds":500,"microseconds":0},"version":"1.10.10-release git 4cb05e7 2023-08-13 16:57:39Z 64bit","sessions":{"count":{"total":12,"active":4,"peak":6,"peak5Min":4,"limit":1000},"rate":{"current":0,"max":100,"peak":2,"peak5Min":0}},"idleCPU":{"used":0,"allowed":96.1},"stackSizeKB":{"current":240,"max":8192}}}`
	sofiaXML   = `<?xml version="1.0" encoding="ISO-8859-1"?>
<profiles>
<profile>
<name>backend</name>
<type>profile</type>
<data>sip:mod_sofia@172.18.0.3:5080</data>
<state>RUNNING (0)</state>
</profile>
<profile>
<name>trunks</name>
<type>profile</type>
<data>sip:mod_sofia@172.18.0.3:5060</data>
<state>RUNNING (0)</state>
</profile>
<profile>
<name>edge.example.com</name>
<type>alias</type>
<data>trunks</data>
<state>ALIASED</state>
</profile>
<profile>
<name>inside</name>
<type>profile</type>
<data>sip:mod_sofia@10.0.0.1:5070</data>
<state>DOWN</state>
</profile>
</profiles>
`
	trunksXML = `<?xml version="1.0" encoding="ISO-8859-1"?>
<profile>
  <profile-info>
    <name>trunks</name>
    <calls-in>7</calls-in>
    <calls-out>0</calls-out>
    <failed-calls-in>2</failed-calls-in>
    <failed-calls-out>0</failed-calls-out>
    <registrations>0</registrations>
  </profile-info>
</profile>
`
	backendXML = `<?xml version="1.0" encoding="ISO-8859-1"?>
<profile><profile-info><calls-in>0</calls-in><calls-out>5</calls-out><failed-calls-in>0</failed-calls-in><failed-calls-out>1</failed-calls-out><registrations>0</registrations></profile-info></profile>
`
)

// fakeFreeSWITCH serves the event socket protocol: auth, then one api/response per api command.
func fakeFreeSWITCH(t *testing.T, password string, answers map[string]string) string {
	t.Helper()
	return slowFreeSWITCH(t, password, answers, nil)
}

// slowFreeSWITCH is fakeFreeSWITCH, holding back the answer to each command in delays that long.
func slowFreeSWITCH(t *testing.T, password string, answers map[string]string, delays map[string]time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveESL(conn, password, answers, delays)
		}
	}()
	return ln.Addr().String()
}

func serveESL(conn net.Conn, password string, answers map[string]string, delays map[string]time.Duration) {
	defer func() { _ = conn.Close() }()
	r := textproto.NewReader(bufio.NewReader(conn))
	_, _ = io.WriteString(conn, "Content-Type: auth/request\n\n")
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "auth "):
			if strings.TrimPrefix(line, "auth ") != password {
				_, _ = io.WriteString(conn, "Content-Type: command/reply\nReply-Text: -ERR invalid\n\n")
				return
			}
			_, _ = io.WriteString(conn, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
		case strings.HasPrefix(line, "api "):
			cmd := strings.TrimPrefix(line, "api ")
			time.Sleep(delays[cmd])
			body, ok := answers[cmd]
			if !ok {
				body = "-ERR command not found\n"
			}
			_, _ = fmt.Fprintf(conn, "Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body)
		}
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCollectorReadsFreeSWITCH(t *testing.T) {
	addr := fakeFreeSWITCH(t, "secret", map[string]string{
		`json {"command":"status","data":""}`: statusJSON,
		"show calls count":                    "\n2 total.\n\n",
		"show channels count":                 "\n4 total.\n\n",
		"fsctl pause_check inbound":           "true\n",
		"sofia xmlstatus":                     sofiaXML,
		"sofia xmlstatus profile trunks":      trunksXML,
		"sofia xmlstatus profile backend":     backendXML,
	})
	c := &Collector{Address: addr, Password: "secret", Timeout: 2 * time.Second, Log: quietLog()}

	expected := `
# HELP freeswitch_up 1 when FreeSWITCH's event socket answered and authenticated this scrape, 0 otherwise.
# TYPE freeswitch_up gauge
freeswitch_up 1
# HELP freeswitch_ready 1 when FreeSWITCH reports itself ready (it says so before any SIP profile runs; see freeswitch_sofia_profile_up).
# TYPE freeswitch_ready gauge
freeswitch_ready 1
# HELP freeswitch_sessions_active Sessions (call legs) up now.
# TYPE freeswitch_sessions_active gauge
freeswitch_sessions_active 4
# HELP freeswitch_sessions_total Sessions created since FreeSWITCH started.
# TYPE freeswitch_sessions_total counter
freeswitch_sessions_total 12
# HELP freeswitch_sessions_limit max-sessions: sessions beyond this are refused.
# TYPE freeswitch_sessions_limit gauge
freeswitch_sessions_limit 1000
# HELP freeswitch_calls Bridged calls up now (show calls count).
# TYPE freeswitch_calls gauge
freeswitch_calls 2
# HELP freeswitch_channels Channels up now, bridged or not (show channels count).
# TYPE freeswitch_channels gauge
freeswitch_channels 4
# HELP freeswitch_inbound_paused 1 while new inbound calls are refused (fsctl pause inbound; the operator's drain sets it).
# TYPE freeswitch_inbound_paused gauge
freeswitch_inbound_paused 1
# HELP freeswitch_sofia_profile_up 1 when the SIP profile is RUNNING, 0 when it is listed in another state.
# TYPE freeswitch_sofia_profile_up gauge
freeswitch_sofia_profile_up{profile="backend"} 1
freeswitch_sofia_profile_up{profile="inside"} 0
freeswitch_sofia_profile_up{profile="trunks"} 1
# HELP freeswitch_sofia_calls_total Calls through the SIP profile since it started, by direction (in or out).
# TYPE freeswitch_sofia_calls_total counter
freeswitch_sofia_calls_total{direction="in",profile="backend"} 0
freeswitch_sofia_calls_total{direction="in",profile="trunks"} 7
freeswitch_sofia_calls_total{direction="out",profile="backend"} 5
freeswitch_sofia_calls_total{direction="out",profile="trunks"} 0
# HELP freeswitch_sofia_failed_calls_total Calls through the SIP profile that failed since it started, by direction (in or out).
# TYPE freeswitch_sofia_failed_calls_total counter
freeswitch_sofia_failed_calls_total{direction="in",profile="backend"} 0
freeswitch_sofia_failed_calls_total{direction="in",profile="trunks"} 2
freeswitch_sofia_failed_calls_total{direction="out",profile="backend"} 1
freeswitch_sofia_failed_calls_total{direction="out",profile="trunks"} 0
`
	names := []string{
		"freeswitch_up", "freeswitch_ready", "freeswitch_sessions_active", "freeswitch_sessions_total",
		"freeswitch_sessions_limit", "freeswitch_calls", "freeswitch_channels", "freeswitch_inbound_paused",
		"freeswitch_sofia_profile_up", "freeswitch_sofia_calls_total", "freeswitch_sofia_failed_calls_total",
	}
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), names...); err != nil {
		t.Error(err)
	}

	// 1 day, 1 h, 28 min, 52.5 s.
	if got, want := gaugeValue(t, c, "freeswitch_uptime_seconds"), 86400+3600+28*60+52.5; got != want {
		t.Errorf("uptime = %v, want %v", got, want)
	}
}

func TestCollectorReportsDownWhenSocketFails(t *testing.T) {
	for name, c := range map[string]*Collector{
		"refused":      {Address: "127.0.0.1:1", Password: "ClueCon", Timeout: time.Second, Log: quietLog()},
		"bad password": {Address: fakeFreeSWITCH(t, "secret", nil), Password: "ClueCon", Timeout: time.Second, Log: quietLog()},
	} {
		t.Run(name, func(t *testing.T) {
			if n := testutil.CollectAndCount(c); n != 1 {
				t.Errorf("series = %d, want only freeswitch_up", n)
			}
			if got := gaugeValue(t, c, "freeswitch_up"); got != 0 {
				t.Errorf("freeswitch_up = %v, want 0", got)
			}
		})
	}
}

func TestCollectorKeepsGoingWhenACommandFails(t *testing.T) {
	addr := fakeFreeSWITCH(t, "ClueCon", map[string]string{
		"show calls count": "\n0 total.\n\n",
		// No status, channels, pause_check or sofia: each fails on its own.
	})
	c := &Collector{Address: addr, Password: "ClueCon", Timeout: time.Second, Log: quietLog()}
	if got := gaugeValue(t, c, "freeswitch_up"); got != 1 {
		t.Errorf("freeswitch_up = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(c, "freeswitch_calls"); n != 1 {
		t.Errorf("freeswitch_calls series = %d, want 1", n)
	}
}

// A reply that comes after its command timed out must not be read as the next command's.
func TestCollectorDropsLateReply(t *testing.T) {
	addr := slowFreeSWITCH(t, "ClueCon", map[string]string{
		"show calls count":          "\n2 total.\n\n",
		"show channels count":       "\n4 total.\n\n",
		"fsctl pause_check inbound": "true\n",
	}, map[string]time.Duration{"show calls count": 300 * time.Millisecond})
	c := &Collector{Address: addr, Password: "ClueCon", Timeout: 200 * time.Millisecond, Log: quietLog()}
	// Read out of step, channels would be the calls count and pause_check the channels count (so 0).
	if n := testutil.CollectAndCount(c, "freeswitch_channels", "freeswitch_inbound_paused"); n != 0 {
		t.Errorf("channels and pause_check series = %d, want 0 after the connection timed out", n)
	}
}

func gaugeValue(t *testing.T, c prometheus.Collector, name string) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no %s", name)
	return 0
}

// sofia xmlstatus on an edge whose profiles run under per-address names (sipbench/freeswitch/conf/bin/profile-name.sh),
// as FreeSWITCH 1.10.10 gave it on the kind bench: aliases are <alias> elements. Plus a domain alias, which is not a
// prefix of its profile's name and so leaves its label alone.
const perAddressXML = `<?xml version="1.0" encoding="ISO-8859-1"?>
<profiles>
<alias>
<name>backend</name>
<type>alias</type>
<data>backend-172-18-255-200</data>
<state>ALIASED</state>
</alias>
<alias>
<name>trunks</name>
<type>alias</type>
<data>trunks-172-18-255-200</data>
<state>ALIASED</state>
</alias>
<alias>
<name>edge.example.com</name>
<type>alias</type>
<data>trunks-172-18-255-200</data>
<state>ALIASED</state>
</alias>
<profile>
<name>trunks-172-18-255-200</name>
<type>profile</type>
<data>sip:mod_sofia@172.18.0.7:5060</data>
<state>RUNNING (0)</state>
</profile>
<profile>
<name>backend-172-18-255-200</name>
<type>profile</type>
<data>sip:mod_sofia@172.18.0.7:5080</data>
<state>RUNNING (0)</state>
</profile>
<profile>
<name>inside</name>
<type>profile</type>
<data>sip:mod_sofia@10.0.0.1:5070</data>
<state>RUNNING (0)</state>
</profile>
<profile>
<name>trunks2</name>
<type>profile</type>
<data>sip:mod_sofia@10.0.0.1:5090</data>
<state>DOWN</state>
</profile>
</profiles>
`

func TestParseProfilesLabelsPerAddressProfiles(t *testing.T) {
	profiles, err := parseProfiles(perAddressXML)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range profiles {
		got[p.Name] = p.Label
	}
	want := map[string]string{
		"trunks-172-18-255-200":  "trunks",
		"backend-172-18-255-200": "backend",
		"inside":                 "inside",
		"trunks2":                "trunks2", // starts with "trunks", but no alias names it
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

// The series carry the plain names, while the per-profile commands use the names the profiles run under.
func TestCollectorLabelsPerAddressProfilesByAlias(t *testing.T) {
	addr := fakeFreeSWITCH(t, "ClueCon", map[string]string{
		"sofia xmlstatus": perAddressXML,
		"sofia xmlstatus profile trunks-172-18-255-200":  trunksXML,
		"sofia xmlstatus profile backend-172-18-255-200": backendXML,
	})
	c := &Collector{Address: addr, Password: "ClueCon", Timeout: time.Second, Log: quietLog()}
	expected := `
# HELP freeswitch_sofia_profile_up 1 when the SIP profile is RUNNING, 0 when it is listed in another state.
# TYPE freeswitch_sofia_profile_up gauge
freeswitch_sofia_profile_up{profile="backend"} 1
freeswitch_sofia_profile_up{profile="inside"} 1
freeswitch_sofia_profile_up{profile="trunks"} 1
freeswitch_sofia_profile_up{profile="trunks2"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "freeswitch_sofia_profile_up"); err != nil {
		t.Error(err)
	}
	// trunks and backend each answered their own command; inside's failed, so it has no calls series.
	if n := testutil.CollectAndCount(c, "freeswitch_sofia_calls_total"); n != 4 {
		t.Errorf("freeswitch_sofia_calls_total series = %d, want 4 (trunks and backend, in and out)", n)
	}
}
