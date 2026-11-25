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
	"strings"
	"time"
)

// runKindsmoke is "chaos kindsmoke", the in-cluster half of make kind-smoke
// (ADR-0075 §D10), run as a Job in the kind cluster's spillway namespace:
// Keycloak's discovery document over HTTPS, trusting only -ca, with an
// issuer equal to -keycloak-issuer and an https JWKS URL, and that JWKS
// served with at least one key (SEC-04). It prints one PASS/FAIL line per
// check on stdout and exits 0 when all pass, 1 when any fails and 2 on bad
// flags. The helm task adds the chart's checks.
//
//	chaos kindsmoke -keycloak-issuer https://… -ca ca.pem [-timeout 10s]
func runKindsmoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chaos kindsmoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	issuer := fs.String("keycloak-issuer", "", "the realm's https issuer URL")
	caFile := fs.String("ca", "", "PEM file of the only CA to trust")
	// One request's budget: far above the host stalls of up to ~0.8 s seen
	// on this laptop (ADR-0026, ADR-0034), far below the Job's deadline.
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || *caFile == "" || !strings.HasPrefix(*issuer, "https://") {
		_, _ = fmt.Fprintln(stderr, "chaos kindsmoke: need -keycloak-issuer https://… and -ca, and no other arguments")
		return 2
	}
	pem, err := os.ReadFile(*caFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos kindsmoke:", err)
		return 2
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		_, _ = fmt.Fprintf(stderr, "chaos kindsmoke: no certificate in %s\n", *caFile)
		return 2
	}
	client := &http.Client{
		Timeout:   *timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	fails := 0
	line := func(err error, check, measured string) {
		verdict := "PASS"
		if err != nil {
			verdict, measured = "FAIL", err.Error()
			fails++
		}
		_, _ = fmt.Fprintf(stdout, "%-4s  %-44s %s\n", verdict, check, measured)
	}

	jwks, err := keycloakDiscovery(ctx, client, *issuer)
	line(err, "keycloak discovery over https (SEC-04)", "issuer "+*issuer)
	if err == nil {
		n, err := jwksKeys(ctx, client, jwks)
		line(err, "keycloak JWKS over https", fmt.Sprintf("%d keys at %s", n, jwks))
	}
	if fails > 0 {
		return 1
	}
	return 0
}

// keycloakDiscovery reads issuer's OpenID discovery document and returns its
// JWKS URL, checking that the document names issuer exactly and that the
// JWKS URL is https.
func keycloakDiscovery(ctx context.Context, c *http.Client, issuer string) (string, error) {
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, c, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return "", err
	}
	if doc.Issuer != issuer {
		return "", fmt.Errorf("issuer %q, want %q", doc.Issuer, issuer)
	}
	u, err := url.Parse(doc.JWKSURI)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("jwks_uri %q is not an https URL", doc.JWKSURI)
	}
	return doc.JWKSURI, nil
}

// jwksKeys returns the number of keys the JWKS at u serves; none is an error.
func jwksKeys(ctx context.Context, c *http.Client, u string) (int, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := getJSON(ctx, c, u, &set); err != nil {
		return 0, err
	}
	if len(set.Keys) == 0 {
		return 0, errors.New("JWKS has no keys")
	}
	return len(set.Keys), nil
}

func getJSON(ctx context.Context, c *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	return nil
}
