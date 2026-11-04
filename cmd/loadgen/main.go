// Command loadgen sends GET requests to one URL at a constant arrival rate
// (open model) and prints the run's result as JSON on stdout: target and
// achieved rate, answers by status class, errors, latency percentiles from an
// HDR histogram, and the hardware (ADR-0014). It exits 1 when the achieved
// rate is below 99% of the target, because a saturated generator makes the
// run invalid (docs/design.md, Benchmark method), and 2 on bad flags.
//
//	loadgen -url http://gateway-1:8000/bytes/1024 -rate 10000
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
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
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
	send, err := newSender(o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "loadgen:", err)
		return 2
	}
	g := generator{clock: realClock{}, rate: o.rate, warmup: o.warmup, duration: o.duration, maxInFlight: o.maxInFlight, do: send}
	r := g.run(ctx)
	r.URL, r.Hardware = o.url, readHardware()
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		_, _ = fmt.Fprintln(stderr, "loadgen:", err)
		return 1
	}
	if !r.Valid {
		_, _ = fmt.Fprintf(stderr, "loadgen: invalid run: achieved %.1f rps, below 99%% of the target %d rps\n", r.AchievedRPS, r.TargetRPS)
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
	case o.rate <= 0:
		return errors.New("-rate: must be positive")
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
// response. Connections are kept alive and capped at conns; redirects are not
// followed, and no proxy is used.
func newSender(o options) (func(ctx context.Context) (int, string, error), error) {
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
