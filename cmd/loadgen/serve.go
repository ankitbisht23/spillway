package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// runServe is "loadgen serve": the no-op upstream of NFR-02's scaling runs
// (ADR-0074 §D9). It answers every request on -addr with 200 and -bytes
// bytes from one fixed buffer, logs nothing, and stops on SIGINT or
// SIGTERM. With -healthcheck it is instead a client of a running server on
// -addr, for readiness checks in images without a shell.
//
//	loadgen serve [-addr :8080] [-bytes 1024] [-healthcheck]
func runServe(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("loadgen serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8080", "listen address")
	n := fs.Int("bytes", 1024, "response body size in bytes")
	healthcheck := fs.Bool("healthcheck", false, "check a running server on -addr, then exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || *n < 0 {
		_, _ = fmt.Fprintln(stderr, "loadgen serve: unexpected arguments or negative -bytes")
		return 2
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "loadgen serve:", err)
		return 1
	}
	if *healthcheck {
		local := *addr
		if strings.HasPrefix(local, ":") {
			local = "127.0.0.1" + local
		}
		if err := checkServer(ctx, "http://"+local+"/", *n); err != nil {
			return fail(err)
		}
		return 0
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return fail(err)
	}
	if err := serve(ctx, ln, *n); err != nil {
		return fail(err)
	}
	return 0
}

// serve answers on ln until ctx ends, then shuts down within 5 s.
func serve(ctx context.Context, ln net.Listener, n int) error {
	body := bytes.Repeat([]byte{'x'}, n)
	size := strconv.Itoa(n)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			h := w.Header()
			h["Content-Type"] = []string{"application/octet-stream"}
			h["Content-Length"] = []string{size}
			_, _ = w.Write(body)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		// ctx has ended; shutting down needs a context of its own.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}

// checkServer GETs url and wants 200 with n bytes.
func checkServer(ctx context.Context, url string, n int) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: a local healthcheck of -addr
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || got != int64(n) {
		return fmt.Errorf("%s: status %d with %d bytes, want 200 with %d", url, resp.StatusCode, got, n)
	}
	return nil
}
