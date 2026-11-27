package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

// runProbe is "demo-traffic probe --consumer <name> --route
// <orders|catalog|payments> [-n 5]" (ADR-0096 §1, the README's terminal
// scenes, ADR-0099): it issues the consumer a key, waits until every
// replica accepts it, then sends n requests one after another, round
// robin, and prints each answer's status with its RateLimit,
// RateLimit-Policy and Retry-After. The key is named by its ID only.
func runProbe(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	consumer := fs.String("consumer", "", "the consumer whose new key the probe sends (required)")
	route := fs.String("route", "orders", "orders, catalog or payments")
	n := fs.Int("n", 5, "how many requests")
	timeout := fs.Duration("timeout", 10*time.Second, "per-call timeout of the REST API and Keycloak")
	if code, done := parse(fs, args); done {
		return code
	}
	if _, ok := routes[*route]; !ok || *consumer == "" || *n < 1 || *n > 1000 {
		_, _ = fmt.Fprintln(stderr, "usage: demo-traffic probe --consumer <name> --route <orders|catalog|payments> [-n 1…1000]")
		return 2
	}
	cp, err := cpClient(getenv, *timeout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic probe:", err)
		return 2
	}
	gws, err := gatewaysFrom(setting(getenv, "SPILLWAY_DEMO_GATEWAYS", "http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000"),
		setting(getenv, "SPILLWAY_DEMO_ADMIN_URLS", "http://gateway-1:8001,http://gateway-2:8001,http://gateway-3:8001"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic probe:", err)
		return 2
	}
	keys := newKeyring(cp)
	key, err := keys.newKey(ctx, *consumer)
	if err == nil {
		err = waitApplied(ctx, gws, key)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic probe: FAIL:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s %s: %s, %d requests\n", *consumer, *route, key, *n)
	client := &http.Client{Timeout: clientTimeout}
	defer client.CloseIdleConnections()
	for i := range *n {
		g := gws[i%len(gws)]
		status, h, err := probeAnswer(ctx, client, g, key, *route)
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "%3d %s: no answer\n", i+1, g.name)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%3d %s: %d  RateLimit: %s  RateLimit-Policy: %s  Retry-After: %s\n", i+1, g.name, status,
			orDash(h.Get("RateLimit")), orDash(h.Get("RateLimit-Policy")), orDash(h.Get("Retry-After")))
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
