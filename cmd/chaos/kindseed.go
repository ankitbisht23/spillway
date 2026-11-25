package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runKindseed is "chaos kindseed", run as a Job by make kind-up (ADR-0086):
// it imports the config file -config as the control plane's working set and
// publishes it, so that the chart's gateways get a config and become ready
// (a managed gateway is not ready before its first config, ADR-0038). It
// takes a service token for the spillway-ci client (its secret from
// SPILLWAY_CI_CLIENT_SECRET or its _FILE, else the dev value; never
// printed) from -token-url, trusting -ca for it. A 409 (the working set is
// already that config, published) is "already current", so a second
// kind-up publishes nothing. Until -wait has passed it tries again every 2
// s while the control plane cannot answer yet (no signing keys fetched,
// ADR-0039). It prints one PASS/FAIL line and exits 0, 1, or 2 on bad flags.
//
//	chaos kindseed -config file [-ca ca.pem] [-controlplane URL] [-token-url URL] [-wait 2m]
func runKindseed(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chaos kindseed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("controlplane", "http://spillway-controlplane.spillway.svc:8080/api/v1", "the control plane's REST API")
	tokenURL := fs.String("token-url", "https://keycloak.spillway-deps.svc.cluster.local:8443/auth/realms/spillway/protocol/openid-connect/token", "Keycloak's token endpoint")
	caFile := fs.String("ca", "", "PEM file of the CA to trust for https, instead of the system's")
	file := fs.String("config", "", "the config file to import and publish (required)")
	timeout := fs.Duration("rest-timeout", 10*time.Second, "per-call timeout of the REST API and Keycloak")
	wait := fs.Duration("wait", 2*time.Minute, "how long the control plane may take to answer")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *file == "" || fs.NArg() > 0 || *timeout <= 0 || *wait <= 0 {
		_, _ = fmt.Fprintln(stderr, "chaos kindseed: need -config, positive durations and no other arguments")
		return 2
	}
	yaml, err := os.ReadFile(*file)
	if err != nil || len(yaml) == 0 {
		_, _ = fmt.Fprintln(stderr, "chaos kindseed: no config:", err)
		return 2
	}
	sec, err := secretFrom(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos kindseed:", err)
		return 2
	}
	rest := newCPClient(*base, *tokenURL, sec, *timeout)
	if *caFile != "" {
		pem, err := os.ReadFile(*caFile)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			_, _ = fmt.Fprintf(stderr, "chaos kindseed: no certificate in %s %v\n", *caFile, err)
			return 2
		}
		rest.client.Transport = &http.Transport{MaxIdleConnsPerHost: 4, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	r := newCPRun(cpOptions{}, rest)
	start := time.Now()
	deadline := start.Add(*wait)
	for {
		p, err := r.importPublish(ctx, yaml, "make kind-up: "+*file)
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "%-4s  %-44s %s\n", "PASS", "config published", fmt.Sprintf("version %d after %s", p.Version, time.Since(start).Round(time.Second)))
			return 0
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_, _ = fmt.Fprintf(stdout, "%-4s  %-44s %s\n", "FAIL", "config published", err.Error())
			return 1
		}
		_, _ = fmt.Fprintln(stderr, "chaos kindseed: not yet:", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}
