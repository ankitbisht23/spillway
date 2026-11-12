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
	flush := func(context.Context) {}
	if s.otlpEndpoint != "" {
		// Logs go to stdout and to the OTel Collector (OB-03). Export errors
		// go to stdout only, never back into the export queue.
		stdout := log
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { stdout.Warn("OTLP export failed", "err", err) }))
		h, shutdown, err := telemetry.NewOTLPLogHandler(ctx, s.otlpEndpoint, "spillway-gateway", version)
		if err != nil {
			return err
		}
		flushed := false
		flush = func(ctx context.Context) {
			flushed = true
			if err := shutdown(ctx); err != nil {
				stdout.Warn("OTLP log flush failed", "err", err)
			}
		}
		defer func() { // serve returned early, with an error
			if !flushed {
				ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
				defer cancel()
				flush(ctx)
			}
		}()
		log = slog.New(slog.NewMultiHandler(log.Handler(), h))
	}
	return serve(ctx, s, log, flush)
}

// flushTimeout bounds the flush of queued telemetry at shutdown (ADR-0030).
const flushTimeout = 5 * time.Second

// serve runs the gateway until ctx ends, then shuts it down gracefully
// (GW-23, ADR-0030), in this order: /readyz fails; for the drain the
// listeners keep serving, closing each HTTP/1.1 connection after its
// response; the listeners close and in-flight requests get the shutdown
// grace, after which the rest are cut; the health probers, DNS refresh and
// watchers stop; flush sends the queued telemetry; the admin server closes.
func serve(ctx context.Context, s settings, log *slog.Logger, flush func(context.Context)) error {
	// Background work runs until step 4 of the shutdown, past ctx, so
	// that health checks and reloads go on while the gateway drains.
	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	pools := proxy.NewRegistry(s.allow, net.DefaultResolver, log)
	defer pools.Close() // on an early return
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
		go c.Warm(bg, time.Second)
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
		go ids.Run(bg, time.Second)
	}
	reg := telemetry.NewRegistry()
	reg.MustRegister(pools) // gw_upstream_endpoint_healthy
	rec := telemetry.NewRecorder(reg, log, s.accessLogSampleRate)
	// Requests in flight, for the shutdown's log line.
	var inFlight atomic.Int64
	handler := proxy.NewHandler(cfg.Snapshot, identity, s.trusted, pools, limiters, rl, s.devMode, rec, log)
	srv := proxy.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		handler.ServeHTTP(w, r)
	}), log)
	if s.tlsCert != "" {
		certs, err := proxy.NewCerts(s.tlsCert, s.tlsKey, log)
		if err != nil {
			return err
		}
		go certs.Run(bg, time.Second)
		srv.TLSConfig = certs.TLSConfig()
	}
	go cfg.Run(bg, time.Second)
	go pools.Run(bg, s.dnsRefresh)
	go limiters.Run(bg, 10*time.Second) // drops idle limiter keys (ADR-0017)
	go rec.Run(bg, time.Minute)         // tenants are ranked per minute (OB-01)

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
	// Read before Serve starts: it sets an empty TLSConfig when there is none.
	tlsOn := srv.TLSConfig != nil
	if s.httpAddr != "" {
		ln, err := lc.Listen(ctx, "tcp", s.httpAddr)
		if err != nil {
			closeAll()
			return err
		}
		go func() { errc <- srv.Serve(ln) }()
		addrs = append(addrs, "http", ln.Addr().String())
	}
	if tlsOn {
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
	// 1. Readiness fails, so load balancers stop sending new connections.
	draining.Store(true)
	log.Info("shutting down: draining", "drain", s.drain.String())
	// 2. Drain: keep serving, but close each HTTP/1.1 connection after its
	// response, so that clients reconnect, to other replicas once
	// readiness has taken effect.
	srv.SetKeepAlivesEnabled(false)
	drain := time.NewTimer(s.drain)
	select {
	case <-drain.C:
	case err := <-errc:
		drain.Stop()
		closeAll()
		return err
	}
	// 3. Stop accepting; in-flight requests get the grace, then are cut.
	log.Info("shutting down: closing listeners", "grace", s.shutdownGrace.String(), "requests", inFlight.Load())
	graceCtx, cancel := context.WithTimeout(context.Background(), s.shutdownGrace)
	defer cancel()
	err = srv.Shutdown(graceCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("shutdown grace ended; cutting requests", "requests", inFlight.Load())
		_ = srv.Close()
		err = nil
	}
	// 4. Background work: health probers, DNS refresh, watchers.
	stopBG()
	pools.Close()
	// 5. Queued telemetry.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), flushTimeout)
	defer cancelFlush()
	flush(flushCtx)
	// 6. The admin server.
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
