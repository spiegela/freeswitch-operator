// Command sipbench runs SIP scenarios against an edge: it plays the far end (a carrier or PBX), answers
// transfers as their target, and checks what every end saw.
//
//	sipbench run [flags] [scenario files or directories...]
//	sipbench validate [scenario files or directories...]
//	sipbench report-card [flags]
//	sipbench blackout -address <node IP> -for <duration>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/fleet"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/runner"
	"github.com/spiegela/freeswitch-operator/sipbench/internal/scenario"

	"github.com/emiago/sipgo/sip"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "run":
		code, err = run(os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "report-card":
		err = reportCard(os.Args[2:])
	case "blackout":
		err = blackout(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sipbench:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  sipbench run [flags] [scenario files or dirs...]   run scenarios (default dir: scenarios)
  sipbench validate [scenario files or dirs...]      parse and check scenarios without running them
  sipbench report-card [flags]                       write the report card from run reports and the record
  sipbench blackout -address IP -for DURATION        take this host dark (run on a node, by the runner)

run -h lists the flags.`)
}

func load(args []string) ([]*scenario.Scenario, error) {
	if len(args) == 0 {
		args = []string{"scenarios"}
	}
	var out []*scenario.Scenario
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			ss, err := scenario.LoadDir(a)
			if err != nil {
				return nil, err
			}
			out = append(out, ss...)
			continue
		}
		s, err := scenario.Load(a)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func validate(args []string) error {
	ss, err := load(args)
	if err != nil {
		return err
	}
	for _, s := range ss {
		fmt.Printf("ok  %-32s %s\n", s.Name, filepath.Base(s.File))
	}
	return nil
}

// env is a flag's default: the variable's value when it is set, even to the empty string (which disables
// -mock, -esl and -target), and def otherwise.
func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func run(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	edge := fs.String("edge", env("SIPBENCH_EDGE", "freeswitch:5060"), "edge SIP address (host:port) the far end calls")
	mockURL := fs.String("mock", env("SIPBENCH_MOCK", "http://mockbackend:8080"),
		"mock backend control API; empty if the backend is not the mock")
	lkAgent := fs.String("lkagent", env("SIPBENCH_LKAGENT", ""),
		"stand-in LiveKit agent's API (cmd/lkagent); adds the livekit capability. Empty: livekit scenarios skip")
	eslAddr := fs.String("esl", env("SIPBENCH_ESL", "freeswitch:8021"),
		"FreeSWITCH event socket; empty disables setup/teardown")
	eslPass := fs.String("esl-password", env("SIPBENCH_ESL_PASSWORD", "ClueCon"), "event socket password")
	target := fs.String("target", env("SIPBENCH_TARGET", "0.0.0.0:5090"),
		"where to answer transfers (host:port); empty disables")
	caps := fs.String("caps", env("SIPBENCH_CAPS", "mock,esl,codec:pcmu,codec:pcma,sip-trace"),
		"capabilities this run provides, comma separated")
	fleetRef := fs.String("fleet", env("SIPBENCH_FLEET", ""),
		"the FreeswitchCluster fleet scenarios act on, namespace/name; adds the fleet capability")
	blackoutImage := fs.String("blackout-image", env("SIPBENCH_BLACKOUT_IMAGE", ""),
		"the image (with sipbench in it) blackoutNodeOf runs on a fleet node; with -fleet, adds the node-blackout capability")
	prom := fs.String("prometheus", env("SIPBENCH_PROMETHEUS", ""),
		"Prometheus HTTP API the scenarios' metric checks query, e.g. http://prometheus:9090; empty skips them")
	only := fs.String("run", env("SIPBENCH_RUN", ""), "only run scenarios whose name matches this regexp")
	short := fs.Bool("short", env("SIPBENCH_SHORT", "") == "true", "skip the scenarios marked long")
	parallel := fs.Int("parallel", 1, "run up to this many scenarios at once that leave the edge and the network "+
		"alone (no setup, actions, impairments or metric checks; SIPBENCH_PARALLEL); the rest run one at a time after them")
	jsonOut := fs.String("json", env("SIPBENCH_JSON", ""),
		"write the JSON report here ('-' for stdout, which moves the summary to stderr)")
	junitOut := fs.String("junit", "", "write a JUnit XML report here")
	level := fs.String("log-level", "warn", "debug, info, warn or error")
	waitFor := fs.Duration("wait", 30*time.Second, "how long to wait for the edge to answer OPTIONS before starting")
	if v := os.Getenv("SIPBENCH_PARALLEL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 2, fmt.Errorf("SIPBENCH_PARALLEL: %w", err)
		}
		*parallel = n
	}
	_ = fs.Parse(args)

	var lv slog.Level
	if err := lv.UnmarshalText([]byte(*level)); err != nil {
		return 2, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
	slog.SetDefault(log) // the SIP and media libraries log through the default logger
	// SIPBENCH_SIP_DEBUG=true logs every SIP message sent and received (at debug level, so with -log-level debug).
	sip.SIPDebug = os.Getenv("SIPBENCH_SIP_DEBUG") == "true"

	ss, err := load(fs.Args())
	if err != nil {
		return 2, err
	}
	if ss, err = pick(ss, *only, *short); err != nil {
		return 2, err
	}

	capSet := map[string]bool{}
	var capList []string
	for c := range strings.SplitSeq(*caps, ",") {
		if c = strings.TrimSpace(c); c != "" {
			capSet[c] = true
			capList = append(capList, c)
		}
	}
	var fl fleet.Fleet
	if *fleetRef != "" {
		if fl, err = fleet.Connect(*fleetRef); err != nil {
			return 2, err
		}
		if !capSet["fleet"] {
			capSet["fleet"] = true
			capList = append(capList, "fleet")
		}
		if *blackoutImage != "" && !capSet[scenario.BlackoutCapability] {
			capSet[scenario.BlackoutCapability] = true
			capList = append(capList, scenario.BlackoutCapability)
		}
	}
	if *lkAgent != "" && !capSet[scenario.LiveKitCapability] {
		capSet[scenario.LiveKitCapability] = true
		capList = append(capList, scenario.LiveKitCapability)
	}
	slices.Sort(capList)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := runner.New(runner.Config{
		Edge: *edge, MockURL: *mockURL, LKAgentURL: *lkAgent, ESL: *eslAddr, ESLPassword: *eslPass,
		TargetListen: *target, Caps: capSet, Fleet: fl, BlackoutImage: *blackoutImage,
		PrometheusURL: strings.TrimRight(*prom, "/"), Log: log,
	})
	if err := r.Start(ctx); err != nil {
		return 2, err
	}
	if *waitFor > 0 {
		if err := r.WaitReady(ctx, *waitFor); err != nil {
			return 2, err
		}
	}

	rep := &runner.Report{StartedAt: time.Now().UTC(), Edge: *edge, Caps: capList}
	for _, res := range runAll(ctx, r, ss, *parallel) {
		rep.Add(res)
	}
	// With the JSON report on stdout the summary goes to stderr, so stdout is the report alone.
	summary := io.Writer(os.Stdout)
	if *jsonOut == "-" {
		summary = os.Stderr
	}
	_, _ = fmt.Fprintln(summary)
	rep.WriteSummary(summary)

	if *jsonOut != "" {
		if err := writeTo(*jsonOut, rep.WriteJSON); err != nil {
			return 2, err
		}
	}
	if *junitOut != "" {
		if err := writeTo(*junitOut, rep.WriteJUnit); err != nil {
			return 2, err
		}
	}
	if rep.Fail > 0 {
		return 1, nil
	}
	return 0, nil
}

func writeTo(path string, write func(io.Writer) error) error {
	if path == "-" {
		return write(os.Stdout)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// pick keeps the scenarios whose name matches only (a regexp; empty keeps all), and with short leaves out those
// marked long, saying which.
func pick(ss []*scenario.Scenario, only string, short bool) ([]*scenario.Scenario, error) {
	var re *regexp.Regexp
	if only != "" {
		var err error
		if re, err = regexp.Compile(only); err != nil {
			return nil, fmt.Errorf("-run: %w", err)
		}
	}
	var kept []*scenario.Scenario
	var long []string
	for _, s := range ss {
		switch {
		case re != nil && !re.MatchString(s.Name):
		case short && s.Long:
			long = append(long, s.Name)
		default:
			kept = append(kept, s)
		}
	}
	if len(long) > 0 {
		fmt.Fprintf(os.Stderr, "-short: leaving out %d long scenarios: %s\n", len(long), strings.Join(long, ", "))
	}
	return kept, nil
}

// shareable reports whether s can run beside other scenarios: it changes nothing outside its own calls (no setup
// commands, fleet actions or impairments) and checks no edge-wide counters, which other calls would move too.
func shareable(s *scenario.Scenario) bool {
	return len(s.Setup) == 0 && len(s.Teardown) == 0 && len(s.Actions) == 0 && len(s.Impairments) == 0 &&
		len(s.Metrics) == 0
}

// runAll runs ss and returns their results in the same order. Up to parallel of the shareable ones run at once,
// first; the rest run one at a time after them. Each result's line is printed as it ends.
func runAll(ctx context.Context, r *runner.Runner, ss []*scenario.Scenario, parallel int) []runner.Result {
	results := make([]runner.Result, len(ss))
	done := make([]bool, len(ss))
	var mu sync.Mutex
	finish := func(i int, res runner.Result) {
		mu.Lock()
		defer mu.Unlock()
		results[i], done[i] = res, true
		fmt.Fprintf(os.Stderr, "%-4s %s\n", strings.ToUpper(res.Status), ss[i].Name)
	}
	if parallel > 1 {
		var wg sync.WaitGroup
		slots := make(chan struct{}, parallel)
		for i, s := range ss {
			if !shareable(s) {
				continue
			}
			wg.Go(func() {
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-slots }()
				finish(i, r.Run(ctx, s))
			})
		}
		wg.Wait()
	}
	for i, s := range ss {
		if done[i] {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		finish(i, r.Run(ctx, s))
	}
	var out []runner.Result
	for i := range ss {
		if done[i] {
			out = append(out, results[i])
		}
	}
	return out
}
