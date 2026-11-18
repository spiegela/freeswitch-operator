// Command freeswitch-exporter serves FreeSWITCH metrics at /metrics, read over FreeSWITCH's event socket at each
// scrape. The operator runs it beside FreeSWITCH in each pod of a FreeswitchCluster that sets spec.metrics.
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
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	reg := prometheus.NewRegistry()
	reg.MustRegister(&fsexporter.Collector{
		Address: *eslAddr,
		// The password comes from the environment only, never a flag, so it stays out of the pod's command line.
		Password: cmp.Or(os.Getenv("ESL_PASSWORD"), "ClueCon"),
		Timeout:  *timeout,
		Log:      log,
	})
	mux := http.NewServeMux()
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelError)
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorLog: errLog}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// As a native sidecar the exporter gets SIGTERM only once FreeSWITCH has stopped, so it can stop at once.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
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
