package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const redisSecret = "redis-dev-only-not-secret" //nolint:gosec // G101: a fake password the test looks for in logs

// wrongPassRedis answers every command as a Redis that refuses the password
// would, and records what it was sent.
func wrongPassRedis(t *testing.T) (addr string, sent *lockedBuffer) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	sent = &lockedBuffer{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = sent.Write([]byte(line))
					if strings.HasPrefix(line, "*") { // a command: one reply
						_, _ = io.WriteString(c, "-WRONGPASS invalid username-password pair or user is disabled.\r\n")
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), sent
}

// TestRedisPasswordNeverLogged loads the Redis password from env and from a
// file (SEC-09, ADR-0040), checks the settings dump, then runs the gateway
// against a Redis that refuses it: the password reaches Redis, and no log
// line shows it.
func TestRedisPasswordNeverLogged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "upstream") }))
	t.Cleanup(up.Close)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "version: 1\nupstreams: [{ name: up, endpoints: [{ address: '" + up.Listener.Addr().String() + "' }] }]\n" +
		"routes: [{ name: all, match: { path_prefix: / }, upstream: up }]\n" +
		"policies: [{ name: shared, level: route, match: { routes: ['*'] }, key_template: '{route}', algorithm: gcra, limits: [{ rate: 1, period: 1h, burst: 1 }] }]\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	pwFile := filepath.Join(t.TempDir(), "redis-password")
	if err := os.WriteFile(pwFile, []byte(redisSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"env", "file"} {
		t.Run(from, func(t *testing.T) {
			redisAddr, sent := wrongPassRedis(t)
			kv := map[string]string{
				"SPILLWAY_GATEWAY_CONFIG":           cfg,
				"SPILLWAY_GATEWAY_HTTP_ADDR":        freeAddr(t),
				"SPILLWAY_GATEWAY_ADMIN_ADDR":       freeAddr(t),
				"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": "127.0.0.0/8",
				"SPILLWAY_GATEWAY_REDIS_ADDRS":      redisAddr,
				"SPILLWAY_GATEWAY_REDIS_USERNAME":   "spillway",
				"SPILLWAY_GATEWAY_SHUTDOWN_GRACE":   "1s",
				"SPILLWAY_GATEWAY_DRAIN":            "0s",
			}
			if from == "env" {
				kv["SPILLWAY_GATEWAY_REDIS_PASSWORD"] = redisSecret
			} else {
				kv["SPILLWAY_GATEWAY_REDIS_PASSWORD_FILE"] = pwFile
			}
			s, err := loadSettings(func(k string) string { return kv[k] })
			if err != nil || s.redisAuth.Password.Reveal() != redisSecret || s.redisAuth.Username != "spillway" {
				t.Fatalf("settings: %+v, %v", s.redisAuth, err)
			}
			logs := &lockedBuffer{}
			log := slog.New(slog.NewJSONHandler(logs, nil))
			log.Info("settings", "settings", s, "redis_auth", s.redisAuth)
			dump := fmt.Sprintf("%v %+v %#v", s, s, s)

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- serve(ctx, s, log, nil, noFlush) }()
			waitFor(t, "the warm-up failure logged", func() bool { return strings.Contains(logs.String(), "redis limiter warm-up failed") })
			// A request goes to the limiter, whose call fails on the
			// refused connection.
			get(t, "http://"+s.httpAddr+"/x")
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
			if !strings.Contains(sent.String(), redisSecret) {
				t.Errorf("the password never reached Redis: %q", sent.String())
			}
			if out := logs.String() + dump; strings.Contains(out, redisSecret) {
				t.Errorf("a log line or the settings dump shows the password:\n%s", out)
			}
		})
	}
}

// TestServeManagedNeedsStreamCerts: a managed gateway whose stream files do
// not load does not start (SEC-02, ADR-0040).
func TestServeManagedNeedsStreamCerts(t *testing.T) {
	dir := t.TempDir()
	kv := map[string]string{
		"SPILLWAY_GATEWAY_CONTROL_PLANE":    "127.0.0.1:1",
		"SPILLWAY_GATEWAY_CP_CERT":          filepath.Join(dir, "cert.pem"),
		"SPILLWAY_GATEWAY_CP_KEY":           filepath.Join(dir, "key.pem"),
		"SPILLWAY_GATEWAY_CP_CA":            filepath.Join(dir, "ca.pem"),
		"SPILLWAY_GATEWAY_SNAPSHOT":         filepath.Join(dir, "snapshot.pb"),
		"SPILLWAY_GATEWAY_HTTP_ADDR":        freeAddr(t),
		"SPILLWAY_GATEWAY_ADMIN_ADDR":       freeAddr(t),
		"SPILLWAY_GATEWAY_ALLOWED_NETWORKS": "127.0.0.0/8",
	}
	s, err := loadSettings(func(k string) string { return kv[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(t.Context(), s, slog.New(slog.DiscardHandler), nil, noFlush); err == nil || !strings.Contains(err.Error(), "config stream mTLS: certificate:") {
		t.Errorf("serve = %v, want the stream certificate error", err)
	}
}
