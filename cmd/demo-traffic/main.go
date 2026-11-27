// Command demo-traffic is the demo's tool (SC-01, ADR-0094, ADR-0096 §1),
// run in the compose network by the demo's one-shot services and make
// targets. Three subcommands so far:
//
//	demo-traffic seed [-config file] [-seed file] [-wait 3m]
//	demo-traffic proxies [-delete]
//	demo-traffic fault [-note text] <redis-cut|upstream-slow> <on|off>
//	demo-traffic fault [-note text] clear
//
// "seed" (the demo-seed one-shot) imports and publishes the demo config and
// creates the demo's tenants and consumers through the control plane's REST
// API, idempotently (ADR-0094 §3); it seeds no API key. "proxies" (the
// demo-init one-shot) creates the toxiproxy proxies of the demo gateways'
// Redis and orders upstream paths, and with -delete (make demo-down)
// removes every demo proxy (ADR-0094 §5). "fault" (make demo-fault-redis,
// make demo-fault-upstream, make demo-fault-clear) switches a demo fault on
// or off, or both off with clear, through the control plane's setDemoFault
// with the service token, so the switch has the API's RBAC (admin), audit
// record and demo-mode check (SC-03, ADR-0095 §5).
//
// Settings come from the environment: SPILLWAY_DEMO_CP_URL (the control
// plane, default http://controlplane:8080), SPILLWAY_DEMO_TOKEN_URL
// (Keycloak's token endpoint in the network), SPILLWAY_DEMO_CLIENT_ID
// (default spillway-ci), SPILLWAY_DEMO_CLIENT_SECRET or its _FILE (seed
// and fault; never printed, SEC-09) and SPILLWAY_DEMO_TOXIPROXY_URL (default
// http://toxiproxy:8474). It exits 0 on success, 1 on failure and 2 on bad
// arguments or settings.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ankitbisht23/spillway/internal/demo"
	"github.com/ankitbisht23/spillway/internal/secret"
)

const usage = `usage: demo-traffic <command> [flags]

commands:
  seed      import and publish the demo config, create the demo's tenants and consumers
  proxies   create the demo's toxiproxy proxies (-delete removes them)
  fault     switch a demo fault: fault <redis-cut|upstream-slow> <on|off>, or fault clear (both off)`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		_, _ = fmt.Fprintln(stdout, usage)
		return 0
	case "seed":
		return runSeed(ctx, args[1:], getenv, stdout, stderr)
	case "proxies":
		return runProxies(ctx, args[1:], getenv, stdout, stderr)
	case "fault":
		return runFault(ctx, args[1:], getenv, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "demo-traffic: unknown command %q\n%s\n", args[0], usage)
	return 2
}

// setting is getenv(name), or def when that is empty.
func setting(getenv func(string) string, name, def string) string {
	if v := getenv(name); v != "" {
		return v
	}
	return def
}

// parse parses args into fs; done is true when the command must exit with
// code (help asked, or bad flags).
func parse(fs *flag.FlagSet, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
		return 2, true
	}
	return 0, false
}

// runSeed is "demo-traffic seed". Until -wait has passed it tries again
// every 2 s while the control plane cannot answer yet (not ready, or its
// signing keys not fetched yet, ADR-0039); any other error fails at once.
func runSeed(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFile := fs.String("config", "/etc/spillway-demo/config.yaml", "the demo config to import and publish")
	seedFile := fs.String("seed", "/etc/spillway-demo/seed.yaml", "the demo's tenants and consumers")
	wait := fs.Duration("wait", 3*time.Minute, "how long the control plane may take to answer")
	timeout := fs.Duration("timeout", 10*time.Second, "per-call timeout of the REST API and Keycloak")
	if code, done := parse(fs, args); done {
		return code
	}
	cfg, err := os.ReadFile(*configFile)
	if err != nil || len(cfg) == 0 {
		_, _ = fmt.Fprintln(stderr, "demo-traffic seed: no demo config:", err)
		return 2
	}
	raw, err := os.ReadFile(*seedFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic seed:", err)
		return 2
	}
	seed, err := demo.ParseSeed(raw)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic seed:", err)
		return 2
	}
	sec, err := secret.Required(getenv, "SPILLWAY_DEMO_CLIENT_SECRET")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic seed:", err)
		return 2
	}
	c := demo.NewClient(setting(getenv, "SPILLWAY_DEMO_CP_URL", "http://controlplane:8080")+"/api/v1",
		setting(getenv, "SPILLWAY_DEMO_TOKEN_URL", "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/token"),
		setting(getenv, "SPILLWAY_DEMO_CLIENT_ID", "spillway-ci"), sec, *timeout)
	start := time.Now()
	deadline := start.Add(*wait)
	for {
		rep, err := demo.Seed(ctx, c, cfg, seed, stdout)
		if err == nil {
			consumers := 0
			for _, t := range seed.Tenants {
				consumers += len(t.Consumers)
			}
			_, _ = fmt.Fprintf(stdout, "demo-seed: done in %.1f s: %d tenants and %d consumers (%d created, %d updated, %d unchanged), config version %d, no API keys\n",
				time.Since(start).Seconds(), len(seed.Tenants), consumers, rep.Created, rep.Updated, rep.Unchanged, rep.ConfigVersion)
			return 0
		}
		if !demo.NotReady(err) || time.Now().After(deadline) || ctx.Err() != nil {
			_, _ = fmt.Fprintln(stderr, "demo-seed: FAIL:", err)
			return 1
		}
		_, _ = fmt.Fprintln(stderr, "demo-seed: the control plane cannot answer yet:", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// runProxies is "demo-traffic proxies [-delete]".
func runProxies(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic proxies", flag.ContinueOnError)
	fs.SetOutput(stderr)
	del := fs.Bool("delete", false, "remove every demo proxy (name demo-*) instead")
	timeout := fs.Duration("timeout", 10*time.Second, "per-call timeout of the toxiproxy API")
	if code, done := parse(fs, args); done {
		return code
	}
	tp := demo.NewToxiproxy(setting(getenv, "SPILLWAY_DEMO_TOXIPROXY_URL", "http://toxiproxy:8474"), *timeout)
	if *del {
		names, err := tp.Names(ctx)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-proxies: FAIL:", err)
			return 1
		}
		n := 0
		for _, name := range names {
			if !strings.HasPrefix(name, demo.DemoPrefix) {
				continue
			}
			if err := tp.Delete(ctx, name); err != nil {
				_, _ = fmt.Fprintln(stderr, "demo-proxies: FAIL:", err)
				return 1
			}
			n++
		}
		_, _ = fmt.Fprintf(stdout, "demo-proxies: deleted %d demo proxies\n", n)
		return 0
	}
	for _, p := range demo.DemoProxies() {
		if err := tp.Create(ctx, p); err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-proxies: FAIL:", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "demo-proxies: %s %s -> %s\n", p.Name, p.Listen, p.Upstream)
	}
	return 0
}

// runFault is "demo-traffic fault [-note text] <redis-cut|upstream-slow>
// <on|off>" and "demo-traffic fault [-note text] clear". It first asks GET
// /api/v1/demo, so with demo mode off it stops with the demo-mode-off
// message (ADR-0095 §1), then sends one setDemoFault per fault and prints
// each new state.
func runFault(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic fault", flag.ContinueOnError)
	fs.SetOutput(stderr)
	note := fs.String("note", "", "the audit record's note (default: the command)")
	timeout := fs.Duration("timeout", 10*time.Second, "per-call timeout of the REST API and Keycloak")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	type change struct {
		name string
		on   bool
	}
	var changes []change
	switch a := fs.Args(); {
	case len(a) == 1 && a[0] == "clear":
		changes = []change{{"redis-cut", false}, {"upstream-slow", false}}
	case len(a) == 2 && (a[0] == "redis-cut" || a[0] == "upstream-slow") && (a[1] == "on" || a[1] == "off"):
		changes = []change{{a[0], a[1] == "on"}}
	default:
		_, _ = fmt.Fprintln(stderr, "usage: demo-traffic fault [-note text] <redis-cut|upstream-slow> <on|off>, or demo-traffic fault [-note text] clear")
		return 2
	}
	if *note == "" {
		*note = "demo-traffic fault " + strings.Join(fs.Args(), " ")
	}
	sec, err := secret.Required(getenv, "SPILLWAY_DEMO_CLIENT_SECRET")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic fault:", err)
		return 2
	}
	c := demo.NewClient(setting(getenv, "SPILLWAY_DEMO_CP_URL", "http://controlplane:8080")+"/api/v1",
		setting(getenv, "SPILLWAY_DEMO_TOKEN_URL", "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/token"),
		setting(getenv, "SPILLWAY_DEMO_CLIENT_ID", "spillway-ci"), sec, *timeout)
	a, err := c.Call(ctx, "GET", "/demo", nil)
	switch {
	case err != nil:
		_, _ = fmt.Fprintln(stderr, "demo-traffic fault: FAIL:", err)
		return 1
	case a.Status == 404:
		_, _ = fmt.Fprintln(stderr, "demo-traffic fault: demo mode is off (GET /api/v1/demo answered 404)")
		return 1
	case a.Status != 200:
		_, _ = fmt.Fprintf(stderr, "demo-traffic fault: FAIL: GET /api/v1/demo: status %d: %s\n", a.Status, demo.ErrorMessage(a.Body))
		return 1
	}
	for _, ch := range changes {
		var out struct {
			Active *bool `json:"active"`
		}
		if _, err := c.Must(ctx, 200, "PUT", "/demo/faults/"+ch.name, map[string]any{"active": ch.on, "note": *note}, &out); err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-traffic fault: FAIL:", err)
			return 1
		}
		if out.Active == nil || *out.Active != ch.on {
			_, _ = fmt.Fprintf(stderr, "demo-traffic fault: FAIL: %s: the control plane answered another state\n", ch.name)
			return 1
		}
		state := "off"
		if ch.on {
			state = "on"
		}
		_, _ = fmt.Fprintf(stdout, "%s: %s\n", ch.name, state)
	}
	return 0
}
