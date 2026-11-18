// Command sipbench runs SIP scenarios against an edge: it plays the far end (a carrier or PBX), answers
// transfers as their target, and checks what every end saw.
//
//	sipbench run [flags] [scenario files or directories...]
//	sipbench validate [scenario files or directories...]
//	sipbench report-card [flags]
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
	"strings"
	"syscall"
	"time"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/fleet"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/runner"
	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/scenario"

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
	eslAddr := fs.String("esl", env("SIPBENCH_ESL", "freeswitch:8021"),
		"FreeSWITCH event socket; empty disables setup/teardown")
	eslPass := fs.String("esl-password", env("SIPBENCH_ESL_PASSWORD", "ClueCon"), "event socket password")
	target := fs.String("target", env("SIPBENCH_TARGET", "0.0.0.0:5090"),
		"where to answer transfers (host:port); empty disables")
	caps := fs.String("caps", env("SIPBENCH_CAPS", "mock,esl,codec:pcmu,codec:pcma,sip-trace"),
		"capabilities this run provides, comma separated")
	fleetRef := fs.String("fleet", env("SIPBENCH_FLEET", ""),
		"the FreeswitchCluster fleet scenarios act on, namespace/name; adds the fleet capability")
	prom := fs.String("prometheus", env("SIPBENCH_PROMETHEUS", ""),
		"Prometheus HTTP API the scenarios' metric checks query, e.g. http://prometheus:9090; empty skips them")
	only := fs.String("run", env("SIPBENCH_RUN", ""), "only run scenarios whose name matches this regexp")
	jsonOut := fs.String("json", env("SIPBENCH_JSON", ""),
		"write the JSON report here ('-' for stdout, which moves the summary to stderr)")
	junitOut := fs.String("junit", "", "write a JUnit XML report here")
	level := fs.String("log-level", "warn", "debug, info, warn or error")
	waitFor := fs.Duration("wait", 30*time.Second, "how long to wait for the edge to answer OPTIONS before starting")
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
	if *only != "" {
		re, err := regexp.Compile(*only)
		if err != nil {
			return 2, fmt.Errorf("-run: %w", err)
		}
		var kept []*scenario.Scenario
		for _, s := range ss {
			if re.MatchString(s.Name) {
				kept = append(kept, s)
			}
		}
		ss = kept
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
	}
	slices.Sort(capList)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := runner.New(runner.Config{
		Edge: *edge, MockURL: *mockURL, ESL: *eslAddr, ESLPassword: *eslPass,
		TargetListen: *target, Caps: capSet, Fleet: fl, PrometheusURL: strings.TrimRight(*prom, "/"), Log: log,
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
	for _, s := range ss {
		if ctx.Err() != nil {
			break
		}
		res := r.Run(ctx, s)
		rep.Add(res)
		fmt.Fprintf(os.Stderr, "%-4s %s\n", strings.ToUpper(res.Status), s.Name)
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
