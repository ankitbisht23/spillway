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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/managed"
	"github.com/ankitbisht23/spillway/internal/mtls"
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
	var tracer trace.Tracer // nil: tracing off, traceparent passed through (OB-02)
	if s.otlpEndpoint != "" {
		// Export errors go to stdout only, never back into the export
		// queue.
		stdout := log
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { stdout.Warn("OTLP export failed", "err", err) }))
		h, t, flushOTLP, err := otlpExport(ctx, s, stdout)
		if err != nil {
			return err
		}
		tracer = t
		flushed := false
		flush = func(ctx context.Context) {
			flushed = true
			flushOTLP(ctx)
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
	return serve(ctx, s, log, tracer, flush)
}

// otlpExport sets up export to the OTel Collector at s.otlpEndpoint: logs
// through the returned handler (OB-03) and traces from the returned tracer
// (OB-02). flush sends what is queued of both at once, within its
// context, and stops them; failures are logged to stdout.
func otlpExport(ctx context.Context, s settings, stdout *slog.Logger) (h slog.Handler, tracer trace.Tracer, flush func(context.Context), err error) {
	h, shutdownLogs, err := telemetry.NewOTLPLogHandler(ctx, s.otlpEndpoint, "spillway-gateway", version)
	if err != nil {
		return nil, nil, nil, err
	}
	tp, shutdownTraces, err := telemetry.NewTracerProvider(ctx, s.otlpEndpoint, "spillway-gateway", version, s.traceSampleRatio)
	if err != nil {
		_ = shutdownLogs(ctx)
		return nil, nil, nil, err
	}
	flush = func(ctx context.Context) {
		// Traces and logs together, within the one timeout (ADR-0030).
		var wg sync.WaitGroup
		for signal, shutdown := range map[string]func(context.Context) error{"traces": shutdownTraces, "logs": shutdownLogs} {
			wg.Go(func() {
				if err := shutdown(ctx); err != nil {
					stdout.Warn("OTLP flush failed", "signal", signal, "err", err)
				}
			})
		}
		wg.Wait()
	}
	return h, tp.Tracer(proxy.TracerName, trace.WithInstrumentationVersion(version)), flush, nil
}

// flushTimeout bounds the flush of queued telemetry at shutdown (ADR-0030).
const flushTimeout = 5 * time.Second

// serve runs the gateway until ctx ends, then shuts it down gracefully
// (GW-23, ADR-0030), in this order: /readyz fails; for the drain the
// listeners keep serving, closing each HTTP/1.1 connection after its
// response; the listeners close and in-flight requests get the shutdown
// grace, after which the rest are cut; the health probers, DNS refresh,
// watchers and the control-plane stream stop; flush sends the queued telemetry; the admin server closes.
// With a tracer, requests are traced (OB-02).
func serve(ctx context.Context, s settings, log *slog.Logger, tracer trace.Tracer, flush func(context.Context)) error {
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
		c := redis.NewClient(s.redisAddrs, s.redisAddrMap, s.redisAuth, ratelimit.SystemClock, log)
		defer func() { _ = c.Close() }()
		go c.Warm(bg, time.Second)
		rl = c
	}
	apply := func(snap *config.Snapshot) error {
		if err := checkRedis(snap, rl != nil); err != nil {
			return err
		}
		if err := pools.Apply(snap); err != nil {
			return err
		}
		limiters.Apply(snap)
		return nil
	}
	// Config and identity come from files in static mode and from the
	// control plane in managed mode (ADR-0038).
	var snapshot func() *config.Snapshot
	identity := func() *config.Identity { return nil }
	ready := func() bool { return snapshot() != nil }
	var syncAge func() time.Duration // managed mode only (ADR-0038)
	if s.controlPlane == "" {
		cfg, err := config.NewWatcher(s.configPath, log, apply)
		if err != nil {
			return err
		}
		snapshot = cfg.Snapshot
		go cfg.Run(bg, time.Second)
		// API keys resolve against the identity file, polled like the
		// config (ADR-0017); without one, every key is invalid.
		if s.identity != "" {
			ids, err := config.NewIdentityWatcher(s.identity, log)
			if err != nil {
				return err
			}
			identity = ids.Identity
			go ids.Run(bg, time.Second)
		}
	} else {
		// mTLS only; the replica name is the client certificate's (SEC-02,
		// D7, ADR-0040).
		streamCerts, replica, err := mtls.LoadGateway(s.streamTLS, log)
		if err != nil {
			return fmt.Errorf("config stream mTLS: %w", err)
		}
		go streamCerts.Run(bg, time.Second)
		conn, err := grpc.NewClient(s.controlPlane, managed.DialOptions(streamCerts.ClientConfig(s.cpServerName))...)
		if err != nil {
			return fmt.Errorf("SPILLWAY_GATEWAY_CONTROL_PLANE: %w", err)
		}
		defer func() { _ = conn.Close() }()
		m := managed.New(configpb.NewConfigServiceClient(conn), managed.Options{
			ReplicaID: replica, SnapshotPath: s.snapshot, Log: log,
			Compile: func(c *configpb.Config) (*config.Snapshot, error) { return config.Prepare(c, log, apply) },
		})
		log.Info("managed mode", "control_plane", s.controlPlane, "server_name", s.cpServerName, "replica", replica, "snapshot", s.snapshot)
		go m.Run(bg)
		go m.Boot(bg) // the stream gets 5 s, then the on-disk snapshot is served (D9)
		snapshot, identity, syncAge = m.Snapshot, m.Identity, m.SyncAge
		ready = func() bool { return m.Snapshot() != nil && m.Identity() != nil }
	}
	reg := telemetry.NewRegistry()
	reg.MustRegister(pools) // gw_upstream_endpoint_healthy
	rec := telemetry.NewRecorder(reg, log, s.accessLogSampleRate)
	// The handler sheds requests past the in-flight cap and the listeners
	// connections past the connection cap; the admin port is exempt (GW-26).
	handler := proxy.NewHandler(snapshot, identity, s.trusted, pools, limiters, rl, s.devMode, s.maxInFlight, tracer, rec, log)
	conns := proxy.NewConnLimit(s.maxConnections, rec, log)
	srv := proxy.NewServer(handler, log)
	srv.ConnState = conns.ConnState // waiting connections give way to new ones at the cap (ADR-0033)
	if s.tlsCert != "" {
		certs, err := proxy.NewCerts(s.tlsCert, s.tlsKey, log)
		if err != nil {
			return err
		}
		go certs.Run(bg, time.Second)
		srv.TLSConfig = certs.TLSConfig()
	}
	go pools.Run(bg, s.dnsRefresh)
	go limiters.Run(bg, 10*time.Second) // drops idle limiter keys (ADR-0017)
	go rec.Run(bg, time.Minute)         // tenants are ranked per minute (OB-01)

	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "gw_config_version",
		Help: "Version of the config snapshot in use; 0 before the first.",
	}, func() float64 {
		if c := snapshot(); c != nil {
			return float64(c.Version)
		}
		return 0
	}), prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "gw_identity_version",
		Help: "Version of the identity in use; 0 before the first, or without an identity file.",
	}, func() float64 {
		if id := identity(); id != nil {
			return float64(id.Version)
		}
		return 0
	}))
	if syncAge != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "gw_config_sync_age_seconds",
			Help: "Seconds since the last heartbeat the control plane answered while the Watch stream was established (CP-08); managed mode only.",
		}, func() float64 { return syncAge().Seconds() }))
	}
	// Ready while a config snapshot is loaded (in managed mode, an identity
	// too) and shutdown has not started; Redis never counts (GW-24,
	// ADR-0038).
	var draining atomic.Bool
	admin := telemetry.NewAdminServer(reg, func() bool { return ready() && !draining.Load() }, log)

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
	tlsConfig := srv.TLSConfig
	tlsOn := tlsConfig != nil
	if s.httpAddr != "" {
		ln, err := lc.Listen(ctx, "tcp", s.httpAddr)
		if err != nil {
			closeAll()
			return err
		}
		ln = conns.Listener(ln, nil)
		go func() { errc <- srv.Serve(ln) }()
		addrs = append(addrs, "http", ln.Addr().String())
	}
	if tlsOn {
		ln, err := lc.Listen(ctx, "tcp", s.tlsAddr)
		if err != nil {
			closeAll()
			return err
		}
		ln = conns.Listener(ln, tlsConfig)
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
	log.Info("shutting down: closing listeners", "grace", s.shutdownGrace.String(), "requests", handler.InFlight())
	graceCtx, cancel := context.WithTimeout(context.Background(), s.shutdownGrace)
	defer cancel()
	err = srv.Shutdown(graceCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("shutdown grace ended; cutting requests", "requests", handler.InFlight())
		_ = srv.Close()
		err = nil
	}
	// 4. Background work: health probers, DNS refresh, watchers.
	stopBG()
	pools.Close()
	// 5. Queued telemetry: traces and logs.
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
