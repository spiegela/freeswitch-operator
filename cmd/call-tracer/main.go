// Command call-tracer builds one OpenTelemetry trace per call out of the SIP messages the edge's components
// capture and LiveKit's webhooks, and sends the traces to an OTLP/HTTP endpoint (Jaeger, Tempo, VictoriaTraces, an
// OpenTelemetry Collector). See docs/call-tracing.md.
//
// It takes HEPv3 over UDP (a KamailioCluster's spec.capture, FreeSWITCH's sofia capture-server) and, optionally,
// LiveKit webhooks at /livekit, checked against the API keys in $LIVEKIT_KEYS ("key: secret" pairs). It serves
// its own metrics at /metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/spiegela/freeswitch-operator/internal/calltrace"
)

func main() {
	hepListen := flag.String("hep-listen", ":9060", "UDP address to take HEPv3 on")
	httpListen := flag.String("http-listen", ":8080", "address to serve /livekit, /metrics and /healthz on")
	otlp := flag.String("otlp-endpoint", os.Getenv("OTLP_TRACES_ENDPOINT"),
		"OTLP/HTTP traces URL, such as http://collector:4318/v1/traces (default $OTLP_TRACES_ENDPOINT)")
	names := flag.String("capture-names", "",
		"service name for each HEP capture ID, as id=name pairs: 1=kamailio,2=freeswitch")
	header := flag.String("correlation-header", "X-CID",
		"header carrying the Call-ID a call arrived with onto the legs a B2BUA starts for it")
	grace := flag.Duration("grace", time.Minute, "how long after a call ends its trace waits for late events")
	setup := flag.Duration("setup-timeout", 3*time.Minute, "close a call without a final answer after this long idle")
	maxAge := flag.Duration("max-call-age", 4*time.Hour, "close any call this old")
	maxCalls := flag.Int("max-calls", 20000, "calls traced at once; past it the oldest is closed")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, *hepListen, *httpListen, *otlp, *names, *header, *grace, *setup, *maxAge, *maxCalls); err != nil {
		log.Error("call-tracer failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, hepListen, httpListen, otlp, names, header string, grace, setup, maxAge time.Duration,
	maxCalls int) error {
	if otlp == "" {
		return errors.New("-otlp-endpoint (or $OTLP_TRACES_ENDPOINT) is required")
	}
	captureNames, err := parseNames(names)
	if err != nil {
		return err
	}
	var verifier *calltrace.LiveKitVerifier
	if s := os.Getenv("LIVEKIT_KEYS"); s != "" {
		keys, err := calltrace.ParseLiveKitKeys(s)
		if err != nil {
			return fmt.Errorf("$LIVEKIT_KEYS: %w", err)
		}
		verifier = &calltrace.LiveKitVerifier{Keys: keys}
	}

	m := calltrace.NewMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.Collectors()...)
	tracker := calltrace.NewTracker(calltrace.Config{
		CaptureNames: captureNames, CorrelationHeader: header, Grace: grace, SetupTimeout: setup,
		MaxCallAge: maxAge, MaxCalls: maxCalls,
	}, m, nil)
	exporter := calltrace.NewExporter(otlp, log, m)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	exported := make(chan struct{})
	expCtx, expStop := context.WithCancel(context.Background())
	defer expStop()
	go func() { exporter.Run(expCtx); close(exported) }()

	conn, err := net.ListenPacket("udp", hepListen)
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				if ctx.Err() == nil {
					log.Error("reading HEP", "error", err)
				}
				return
			}
			exporter.Export(tracker.HandleHEP(buf[:n]))
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /livekit", func(w http.ResponseWriter, r *http.Request) {
		if verifier == nil {
			http.Error(w, "LiveKit webhooks are off: no $LIVEKIT_KEYS", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ev, err := verifier.Verify(r.Header.Get("Authorization"), body)
		if err != nil {
			m.Webhooks.WithLabelValues("rejected").Inc()
			log.Warn("rejected a LiveKit webhook", "error", err, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tracker.HandleLiveKit(ev)
	})
	srv := &http.Server{Addr: httpListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP server failed", "error", err)
			stop()
		}
	}()

	log.Info("tracing calls", "hep", hepListen, "http", httpListen, "otlp", otlp, "livekit", verifier != nil)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
loop:
	for {
		select {
		case <-tick.C:
			exporter.Export(tracker.Sweep())
		case <-ctx.Done():
			break loop
		}
	}
	_ = conn.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	exporter.Export(tracker.Flush())
	expStop()
	<-exported
	return nil
}

// parseNames reads id=name pairs.
func parseNames(s string) (map[uint32]string, error) {
	out := map[uint32]string{}
	for pair := range strings.SplitSeq(s, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		id, name, ok := strings.Cut(pair, "=")
		n, err := strconv.ParseUint(strings.TrimSpace(id), 10, 32)
		if !ok || err != nil || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("-capture-names: want id=name, got %q", pair)
		}
		out[uint32(n)] = strings.TrimSpace(name)
	}
	return out, nil
}
