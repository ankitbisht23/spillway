// Command gateway is Spillway's data plane: it routes requests on the current
// config snapshot and proxies them to upstreams. `gateway version` prints the
// version; `gateway healthcheck` exits 0 only if the running gateway is ready.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("spillway-gateway", version)
			return
		case "healthcheck":
			if err := healthcheck(context.Background(), os.Getenv); err != nil {
				fmt.Fprintln(os.Stderr, "not ready:", err)
				os.Exit(1)
			}
			return
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	setMemoryLimit(log)
	log.Info("starting", "version", version, "gomaxprocs", runtime.GOMAXPROCS(0), "gomemlimit_bytes", debug.SetMemoryLimit(-1))

	s, err := loadSettings(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, s, log)
}

// serve runs the gateway until ctx ends. Then /readyz fails, in-flight
// requests get up to 10 s to finish, and serve returns.
func serve(ctx context.Context, s settings, log *slog.Logger) error {
	pools := proxy.NewRegistry(s.allow, net.DefaultResolver, log)
	cfg, err := config.NewWatcher(s.configPath, log, pools.Apply)
	if err != nil {
		return err
	}
	go cfg.Run(ctx, time.Second)
	go pools.Run(ctx, s.dnsRefresh)

	reg := telemetry.NewRegistry()
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "gw_config_version",
		Help: "Version of the config snapshot in use.",
	}, func() float64 { return float64(cfg.Snapshot().Version) }))
	// Ready while a config snapshot is loaded and shutdown has not started;
	// Redis never counts (GW-24).
	var draining atomic.Bool
	admin := telemetry.NewAdminServer(reg, func() bool { return cfg.Snapshot() != nil && !draining.Load() }, log)

	lc := &net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.httpAddr)
	if err != nil {
		return err
	}
	aln, err := lc.Listen(ctx, "tcp", s.adminAddr)
	if err != nil {
		_ = ln.Close()
		return err
	}
	srv := proxy.NewServer(proxy.NewHandler(cfg.Snapshot, pools, log), log)
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	go func() { errc <- admin.Serve(aln) }()
	log.Info("listening", "http", ln.Addr().String(), "admin", aln.Addr().String())

	select {
	case err := <-errc:
		_ = srv.Close()
		_ = admin.Close()
		return err
	case <-ctx.Done():
	}
	// Draining (GW-23) arrives in M4; until then, fail readiness, finish
	// in-flight requests for a few seconds and exit.
	draining.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	_ = admin.Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
