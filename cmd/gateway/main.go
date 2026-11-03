// Command gateway is Spillway's data plane: it routes requests on the current
// config snapshot and proxies them to upstreams. `gateway version` prints the
// version.
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
	"syscall"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/proxy"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("spillway-gateway", version)
		return
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

	pools := proxy.NewRegistry(s.allow, net.DefaultResolver, log)
	cfg, err := config.NewWatcher(s.configPath, log, pools.Apply)
	if err != nil {
		return err
	}
	go cfg.Run(ctx, time.Second)
	go pools.Run(ctx, s.dnsRefresh)

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.httpAddr)
	if err != nil {
		return err
	}
	srv := proxy.NewServer(proxy.NewHandler(cfg.Snapshot, pools, log), log)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "http", ln.Addr().String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Draining (GW-23) arrives in M4; until then, finish in-flight requests
	// for a few seconds and exit.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
