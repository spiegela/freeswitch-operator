// Command mockbackend is a scriptable SIP user agent server that stands in for whatever sits behind the edge.
// Each call runs the script of the route its Request-URI user names; what each call received is served on an
// HTTP API for the runner to check.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/mock"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mockbackend:", err)
		os.Exit(1)
	}
}

func run() error {
	sipAddr := flag.String("sip", "0.0.0.0:5060", "SIP UDP listen address")
	httpAddr := flag.String("http", "0.0.0.0:8080", "control API listen address")
	cfgPath := flag.String("config", "", "routes file (YAML); empty starts with no routes")
	externalHost := flag.String("external-host", "",
		"host to advertise in Contact/SDP when it differs from the bind address")
	level := flag.String("log-level", envOr("MOCK_LOG_LEVEL", "info"), "debug, info, warn or error")
	flag.Parse()

	var lv slog.Level
	if err := lv.UnmarshalText([]byte(*level)); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
	slog.SetDefault(log) // the SIP and media libraries log through the default logger

	cfg := &mock.Config{Routes: map[string]mock.Route{}}
	if *cfgPath != "" {
		var err error
		if cfg, err = mock.LoadConfig(*cfgPath); err != nil {
			return err
		}
	}
	srv := mock.NewServer(cfg, log)

	host, portS, err := net.SplitHostPort(*sipAddr)
	if err != nil {
		return fmt.Errorf("-sip: %w", err)
	}
	port, err := strconv.Atoi(portS)
	if err != nil {
		return fmt.Errorf("-sip port: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sipbench-mockbackend"))
	if err != nil {
		return err
	}
	defer func() { _ = ua.Close() }()
	tr := diago.Transport{Transport: "udp", BindHost: host, BindPort: port}
	if *externalHost != "" {
		tr.ExternalHost = *externalHost
	}
	dg := diago.NewDiago(ua,
		diago.WithTransport(tr),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{
			media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000,
		}}),
		diago.WithLogger(log),
	)
	if err := dg.ServeBackground(ctx, srv.HandleCall); err != nil {
		return fmt.Errorf("sip: %w", err)
	}

	hs := &http.Server{Addr: *httpAddr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("mockbackend serving", "sip", *sipAddr, "http", *httpAddr, "routes", len(cfg.Routes))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
