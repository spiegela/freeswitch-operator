// Command freeswitch-exporter serves FreeSWITCH metrics at /metrics, read over FreeSWITCH's event socket at each
// scrape. The operator runs it beside FreeSWITCH in each pod of a FreeswitchCluster that sets spec.metrics.
//
// It also relays the edge's barge-in voice detections to the agent as SIP INFO (fsexporter.Relay), unless
// -barge-in-relay=false: FreeSWITCH can't run that itself without a scripting module, and this is the process
// already beside it holding the event socket's password.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"git.instruments.com/ci/freeswitch-operator/internal/fsexporter"
)

func main() {
	listen := flag.String("listen", net.JoinHostPort(os.Getenv("POD_IP"), cmp.Or(os.Getenv("METRICS_PORT"), "9282")),
		"address to serve /metrics on (default: $POD_IP:$METRICS_PORT, or :9282)")
	eslAddr := flag.String("esl-address", "127.0.0.1:"+cmp.Or(os.Getenv("ESL_PORT"), "8021"),
		"FreeSWITCH event socket host:port (default: 127.0.0.1:$ESL_PORT, or 127.0.0.1:8021)")
	timeout := flag.Duration("timeout", 3*time.Second, "time allowed for connecting and for each command")
	relay := flag.Bool("barge-in-relay", os.Getenv("BARGE_IN_RELAY") != "false",
		"send FreeSWITCH's barge-in voice detections to the agent as SIP INFO (default: true unless $BARGE_IN_RELAY is false)")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// The password comes from the environment only, never a flag, so it stays out of the pod's command line.
	password := cmp.Or(os.Getenv("ESL_PASSWORD"), "ClueCon")

	reg := prometheus.NewRegistry()
	reg.MustRegister(&fsexporter.Collector{Address: *eslAddr, Password: password, Timeout: *timeout, Log: log})
	mux := http.NewServeMux()
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelError)
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: errLog}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// As a native sidecar the exporter gets SIGTERM only once FreeSWITCH has stopped, so it can stop at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if *relay {
		r := fsexporter.NewRelay(*eslAddr, password, *timeout, log)
		reg.MustRegister(r.Collectors()...)
		go r.Run(ctx)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("serving FreeSWITCH metrics", "listen", *listen, "eventSocket", *eslAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
}
