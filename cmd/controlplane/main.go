// Command controlplane is Spillway's control plane (CP-01, CP-10, ADR-0036):
// a REST admin API over Postgres. `controlplane serve` (the default) runs
// it; `controlplane migrate` applies the schema migrations and exits (CP-19);
// `controlplane healthcheck` exits 0 only if the running control plane is
// ready; `controlplane version` prints the version.
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "version":
		fmt.Println("spillway-controlplane", version)
		return
	case "healthcheck":
		if err := healthcheck(context.Background(), os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, "not ready:", err)
			os.Exit(1)
		}
		return
	case "migrate":
		err = migrate(log, os.Getenv)
	case "serve":
		err = serve(log, os.Getenv)
	default:
		err = fmt.Errorf("unknown command %q: want serve, migrate, healthcheck or version", cmd)
	}
	if err != nil {
		log.Error("controlplane "+cmd+" failed", "err", err)
		os.Exit(1)
	}
}

// migrate applies the pending migrations as the owner role, sets the app
// role's password, and exits; a second run applies nothing (CP-19).
func migrate(log *slog.Logger, getenv func(string) string) error {
	s, err := loadMigrateSettings(getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	applied, err := store.Migrate(ctx, s.databaseURL, s.password, s.databasePassword)
	if err != nil {
		return err
	}
	for _, v := range applied {
		log.Info("migration applied", "version", v)
	}
	log.Info("database schema is current", "version", store.SchemaVersion(), "applied", len(applied))
	return nil
}

// shutdownTimeout bounds how long in-flight requests get after SIGTERM.
const shutdownTimeout = 10 * time.Second

// serve runs the REST API and the admin server until SIGTERM or SIGINT.
func serve(log *slog.Logger, getenv func(string) string) error {
	log.Info("starting", "version", version)
	s, err := loadSettings(getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, s.databaseURL, s.databasePassword)
	if err != nil {
		return err
	}
	defer st.Close()

	verifier, err := auth.New(auth.Config{
		Issuer: s.oidcIssuer, JWKSURL: s.oidcJWKSURL, Audience: s.oidcAudience, AllowHTTP: s.devMode, Log: log,
	})
	if err != nil {
		return err
	}
	go verifier.Run(ctx) // readiness does not wait for the keys: until they come, tokens are 503 (ADR-0039)

	var ready atomic.Bool
	admin := telemetry.NewAdminServer(telemetry.NewRegistry(), ready.Load, log)
	apiSrv := &http.Server{
		Handler:           api.New(st, s.allow, net.DefaultResolver, api.OIDC{Verifier: verifier}, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 2)
	lc := &net.ListenConfig{}
	for _, srv := range []struct {
		name, addr string
		*http.Server
	}{{"admin", s.adminAddr, admin}, {"api", s.httpAddr, apiSrv}} {
		ln, err := lc.Listen(ctx, "tcp", srv.addr)
		if err != nil {
			return fmt.Errorf("%s listener: %w", srv.name, err)
		}
		log.Info("listening", "server", srv.name, "addr", ln.Addr().String())
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s server: %w", srv.name, err)
			}
		}()
	}
	ready.Store(true)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	ready.Store(false)
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = apiSrv.Shutdown(sctx)
	_ = admin.Shutdown(sctx)
	return err
}

// healthcheck asks the admin port's /readyz and fails unless it answers 200.
// It backs the compose healthcheck: the image has no shell or HTTP client. A
// wildcard admin address is asked on 127.0.0.1.
func healthcheck(ctx context.Context, getenv func(string) string) error {
	host, port, err := net.SplitHostPort(adminAddr(getenv))
	if err != nil {
		return fmt.Errorf("SPILLWAY_CP_ADMIN_ADDR: %w", err)
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz: %s", resp.Status)
	}
	return nil
}
