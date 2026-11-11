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
	"go.opentelemetry.io/otel"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/proxy"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
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
	if s.otlpEndpoint != "" {
		// Logs go to stdout and to the OTel Collector (OB-03). Export errors
		// go to stdout only, never back into the export queue.
		stdout := log
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { stdout.Warn("OTLP export failed", "err", err) }))
		h, shutdown, err := telemetry.NewOTLPLogHandler(ctx, s.otlpEndpoint, "spillway-gateway", version)
		if err != nil {
			return err
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				stdout.Warn("OTLP log flush failed", "err", err)
			}
		}()
		log = slog.New(slog.NewMultiHandler(log.Handler(), h))
	}
	return serve(ctx, s, log)
}

// serve runs the gateway until ctx ends. Then /readyz fails, in-flight
// requests get up to 10 s to finish, and serve returns.
func serve(ctx context.Context, s settings, log *slog.Logger) error {
	pools := proxy.NewRegistry(s.allow, net.DefaultResolver, log)
	// Upstream pools and rate limiters live outside the snapshot and are
	// reconciled before each new one goes live (GW-29).
	limiters := local.NewRegistry(ratelimit.SystemClock)
	// Redis policies run on the limiter's cluster (ADR-0021). The client
	// warms up in the background, so readiness never waits for Redis
	// (GW-24, ADR-0024).
	var rl proxy.Redis
	if len(s.redisAddrs) > 0 {
		c := redis.NewClient(s.redisAddrs, s.redisAddrMap, ratelimit.SystemClock, log)
		defer func() { _ = c.Close() }()
		go c.Warm(ctx, time.Second)
		rl = c
	}
	cfg, err := config.NewWatcher(s.configPath, log, func(snap *config.Snapshot) error {
		if err := checkRedis(snap, rl != nil); err != nil {
			return err
		}
		if err := pools.Apply(snap); err != nil {
			return err
		}
		limiters.Apply(snap)
		return nil
	})
	if err != nil {
		return err
	}
	// API keys resolve against the identity file, polled like the config
	// (ADR-0017); without one, every key is invalid.
	identity := func() *config.Identity { return nil }
	if s.identity != "" {
		ids, err := config.NewIdentityWatcher(s.identity, log)
		if err != nil {
			return err
		}
		identity = ids.Identity
		go ids.Run(ctx, time.Second)
	}
	reg := telemetry.NewRegistry()
	rec := telemetry.NewRecorder(reg, log, s.accessLogSampleRate)
	srv := proxy.NewServer(proxy.NewHandler(cfg.Snapshot, identity, s.trusted, pools, limiters, rl, rec, log), log)
	if s.tlsCert != "" {
		certs, err := proxy.NewCerts(s.tlsCert, s.tlsKey, log)
		if err != nil {
			return err
		}
		go certs.Run(ctx, time.Second)
		srv.TLSConfig = certs.TLSConfig()
	}
	go cfg.Run(ctx, time.Second)
	go pools.Run(ctx, s.dnsRefresh)
	go limiters.Run(ctx, 10*time.Second) // drops idle limiter keys (ADR-0017)
	go rec.Run(ctx, time.Minute)         // tenants are ranked per minute (OB-01)

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "gw_config_version",
		Help: "Version of the config snapshot in use.",
	}, func() float64 { return float64(cfg.Snapshot().Version) }))
	// Ready while a config snapshot is loaded and shutdown has not started;
	// Redis never counts (GW-24).
	var draining atomic.Bool
	admin := telemetry.NewAdminServer(reg, func() bool { return cfg.Snapshot() != nil && !draining.Load() }, log)

	lc := &net.ListenConfig{}
	aln, err := lc.Listen(ctx, "tcp", s.adminAddr)
	if err != nil {
		return err
	}
	errc := make(chan error, 3)
	go func() { errc <- admin.Serve(aln) }()
	// Closing a server also closes the listeners handed to it.
	closeAll := func() {
		_ = srv.Close()
		_ = admin.Close()
	}
	addrs := []any{"admin", aln.Addr().String()}
	if s.httpAddr != "" {
		ln, err := lc.Listen(ctx, "tcp", s.httpAddr)
		if err != nil {
			closeAll()
			return err
		}
		go func() { errc <- srv.Serve(ln) }()
		addrs = append(addrs, "http", ln.Addr().String())
	}
	if srv.TLSConfig != nil {
		ln, err := lc.Listen(ctx, "tcp", s.tlsAddr)
		if err != nil {
			closeAll()
			return err
		}
		go func() { errc <- srv.ServeTLS(ln, "", "") }() // HTTP/2 or HTTP/1.1, by ALPN
		addrs = append(addrs, "tls", ln.Addr().String())
	}
	log.Info("listening", addrs...)

	select {
	case err := <-errc:
		closeAll()
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

// checkRedis rejects a snapshot with a Redis policy when the gateway has no
// Redis (ADR-0021).
func checkRedis(snap *config.Snapshot, haveRedis bool) error {
	for _, p := range snap.Policies {
		if !p.Local && !haveRedis {
			return fmt.Errorf("policy %q runs in Redis (local is not true), but SPILLWAY_GATEWAY_REDIS_ADDRS is not set", p.Name)
		}
	}
	return nil
}
