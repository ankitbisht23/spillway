// Command loadgen sends GET requests to one URL at a constant arrival rate
// (open model) and prints the run's result as JSON on stdout: target and
// achieved rate, answers by status class, errors, latency percentiles from an
// HDR histogram, and the hardware (ADR-0014); and, warm-up included, how many
// requests failed and when (ADR-0034). It exits 1 when the achieved
// rate is below 99% of the target, because a saturated generator makes the
// run invalid (docs/design.md, Benchmark method), and 2 on bad flags.
//
//	loadgen -url http://gateway-1:8000/bytes/1024 -rate 10000
//
// In the per-tenant mode (NFR-15, ADR-0074 §D9), repeated -tenant flags
// replace -rate: each tenant sends at its own rate with its own X-API-Key,
// read once from its key file and never printed, and the result gains
// tenants.<name> with per-slot p99s (tenants.go):
//
//	loadgen -url http://gateway-1:8000/bytes/1024 -slot 10s \
//	    -tenant name=A,rate=2000,key-file=/keys/a,toggle -tenant name=B,rate=100,key-file=/keys/b
//
// "loadgen serve" is the no-op upstream of the NFR-02 scaling runs
// (serve.go).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ankitbisht23/spillway/internal/hardware"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	var code int
	if args := os.Args[1:]; len(args) > 0 && args[0] == "serve" {
		code = runServe(ctx, args[1:], os.Stderr)
	} else {
		code = run(ctx, args, os.Stdout, os.Stderr)
	}
	stop()
	os.Exit(code)
}

// run is the command: it parses args, makes the run, prints the result to
// stdout and returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	var r result
	if len(o.tenants) > 0 {
		var newErr error
		r, err = runTenants(ctx, o, realClock{}, func(apiKey string) func(context.Context) (int, string, error) {
			send, err := newSender(o, apiKey)
			if err != nil && newErr == nil {
				newErr = err
			}
			return send
		})
		if err == nil {
			err = newErr
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "loadgen:", err)
			return 2
		}
	} else {
		send, err := newSender(o, "")
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "loadgen:", err)
			return 2
		}
		g := generator{clock: realClock{}, rate: o.rate, warmup: o.warmup, duration: o.duration, maxInFlight: o.maxInFlight, do: send}
		r = g.run(ctx)
	}
	r.URL, r.Hardware = o.url, hardware.Read()
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		_, _ = fmt.Fprintln(stderr, "loadgen:", err)
		return 1
	}
	if !r.Valid {
		if len(o.tenants) > 0 {
			_, _ = fmt.Fprintf(stderr, "loadgen: invalid run: %v\n", invalidTenants(r))
		} else {
			_, _ = fmt.Fprintf(stderr, "loadgen: invalid run: achieved %.1f rps, below 99%% of the target %d rps\n", r.AchievedRPS, r.TargetRPS)
		}
		return 1
	}
	return 0
}

type options struct {
	url         string
	rate        int
	warmup      time.Duration
	duration    time.Duration
	maxInFlight int
	conns       int
	timeout     time.Duration
	caCert      string
	tenants     tenantFlags   // the per-tenant mode when not empty
	slot        time.Duration // its slot length
}

// parseFlags parses and checks the flags. The defaults are the benchmark
// protocol's 30 s warm-up and 3 min measured (docs/design.md, Benchmark
// method). Errors are printed to stderr.
func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.url, "url", "", "target `URL`: http:// for HTTP/1.1, https:// for HTTP/2 if the server offers it (required)")
	fs.IntVar(&o.rate, "rate", 0, "requests per second (required)")
	fs.DurationVar(&o.warmup, "warmup", 30*time.Second, "warm-up, sent at the rate but left out of the result")
	fs.DurationVar(&o.duration, "duration", 3*time.Minute, "measured time after the warm-up")
	fs.IntVar(&o.maxInFlight, "max-in-flight", 10000, "most requests in flight; a request due while it is reached is dropped")
	fs.IntVar(&o.conns, "conns", 100, "most connections to the target; requests beyond them wait for one")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-request timeout")
	fs.StringVar(&o.caCert, "cacert", "", "PEM `file` of certificates to trust for https:// instead of the system's")
	fs.Var(&o.tenants, "tenant", "name=<name>,rate=<rps>,key-file=<path>[,toggle]: a tenant of the per-tenant mode (repeatable; replaces -rate)")
	fs.DurationVar(&o.slot, "slot", 10*time.Second, "per-tenant mode: slot length of the per-slot p99s; a toggle tenant sends in odd slots only")
	if err := fs.Parse(args); err != nil {
		return o, err // printed by fs, with the usage
	}
	err := checkOptions(o, fs.Args())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "loadgen:", err)
	}
	return o, err
}

func checkOptions(o options, rest []string) error {
	u, err := url.Parse(o.url)
	switch {
	case len(rest) > 0:
		return fmt.Errorf("unexpected arguments %q", rest)
	case err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "":
		return fmt.Errorf("-url: %q is not an http:// or https:// URL", o.url)
	case len(o.tenants) > 0 && o.rate != 0:
		return errors.New("-rate: not with -tenant, which sets each tenant's rate")
	case len(o.tenants) == 0 && o.rate <= 0:
		return errors.New("-rate: must be positive")
	case len(o.tenants) > 0 && (o.slot < 2*time.Second || o.slot > o.duration):
		return errors.New("-slot: must be at least 2s and at most -duration")
	case o.warmup < 0:
		return errors.New("-warmup: must not be negative")
	case o.duration <= 0:
		return errors.New("-duration: must be positive")
	case o.maxInFlight <= 0:
		return errors.New("-max-in-flight: must be positive")
	case o.conns <= 0:
		return errors.New("-conns: must be positive")
	case o.timeout <= 0:
		return errors.New("-timeout: must be positive")
	}
	return nil
}

// newSender returns the generator's do: one GET of the URL, reading the whole
// response, with apiKey in X-API-Key unless it is empty. Connections are kept
// alive and capped at conns, per sender; redirects are not followed, and no
// proxy is used.
func newSender(o options, apiKey string) (func(ctx context.Context) (int, string, error), error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.caCert != "" {
		pem, err := os.ReadFile(o.caCert)
		if err != nil {
			return nil, fmt.Errorf("-cacert: %w", err)
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-cacert: no PEM certificate in %s", o.caCert)
		}
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     tlsConfig,
			ForceAttemptHTTP2:   true,
			MaxConnsPerHost:     o.conns,
			MaxIdleConnsPerHost: o.conns,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       o.timeout,
	}
	return func(ctx context.Context) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url, nil) //nolint:gosec // G704: requesting the operator's -url is the point
		if err != nil {
			return 0, "", err
		}
		if apiKey != "" {
			req.Header[apiKeyHeader] = []string{apiKey}
		}
		resp, err := client.Do(req) //nolint:gosec // G704: as above
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return 0, "", err
		}
		return resp.StatusCode, resp.Proto, nil
	}, nil
}
