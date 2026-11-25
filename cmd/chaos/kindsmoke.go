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
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// runKindsmoke is "chaos kindsmoke", the in-cluster half of make kind-smoke
// (ADR-0075 §D10, ADR-0086), run as a Job in the kind cluster's spillway
// namespace: Keycloak's discovery document over HTTPS, trusting only -ca,
// with an issuer equal to -keycloak-issuer and an https JWKS URL, and that
// JWKS served with at least one key (SEC-04); then, with the chart's flags,
// a GET through the gateway's Service answered 200 (-gateway), each admin
// address in -closed refusing the Job's connection within -dial-timeout
// (GW-25's NetworkPolicies), and Prometheus's active targets of each job in
// -targets all up, as many as given, within -targets-wait (-prometheus). It
// prints one PASS/FAIL line per check on stdout and exits 0 when all pass,
// 1 when any fails and 2 on bad flags.
//
//	chaos kindsmoke -keycloak-issuer https://… -ca ca.pem [-timeout 10s]
//	  [-gateway https://…] [-closed ip:port,…] [-dial-timeout 2s]
//	  [-prometheus http://… -targets gateway=3,controlplane=1 -targets-wait 60s]
func runKindsmoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chaos kindsmoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	issuer := fs.String("keycloak-issuer", "", "the realm's https issuer URL")
	caFile := fs.String("ca", "", "PEM file of the only CA to trust")
	// One request's budget: far above the host stalls of up to ~0.8 s seen
	// on this laptop (ADR-0026, ADR-0034), far below the Job's deadline.
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout")
	gateway := fs.String("gateway", "", "an https URL through the gateway's Service that must answer 200")
	closed := fs.String("closed", "", "comma-separated host:port admin addresses this pod must not reach")
	// ADR-0075 §D10: the connection must not open within 2 s; a stall of
	// ~0.8 s cannot open a connection the NetworkPolicy drops.
	dialTimeout := fs.Duration("dial-timeout", 2*time.Second, "how long each -closed address gets to accept a connection")
	prom := fs.String("prometheus", "", "Prometheus's base URL, for -targets")
	targets := fs.String("targets", "", "job=count,…: each job's active targets, all of which must be up")
	targetsWait := fs.Duration("targets-wait", 60*time.Second, "how long the targets may take to be up")
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
	if *gateway != "" && !strings.HasPrefix(*gateway, "https://") {
		_, _ = fmt.Fprintln(stderr, "chaos kindsmoke: -gateway must be an https:// URL (the gateway serves no plain HTTP in Helm)")
		return 2
	}
	want, err := parseTargets(*targets)
	if err != nil || (len(want) > 0) != (*prom != "") {
		_, _ = fmt.Fprintln(stderr, "chaos kindsmoke: -prometheus and -targets job=count,… go together", err)
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
	if *gateway != "" {
		n, err := getOK(ctx, client, *gateway)
		line(err, "gateway Service over https -> 200", fmt.Sprintf("%d bytes from %s", n, *gateway))
	}
	for a := range strings.SplitSeq(*closed, ",") {
		if a = strings.TrimSpace(a); a != "" {
			measured, err := unreachable(ctx, a, *dialTimeout)
			line(err, "admin "+a+" closed to this pod (GW-25)", measured)
		}
	}
	if len(want) > 0 {
		measured, err := targetsUp(ctx, client, *prom, want, *targetsWait)
		line(err, "prometheus targets up", measured)
	}
	if fails > 0 {
		return 1
	}
	return 0
}

// getOK GETs u and returns the body's length; any status but 200 is an
// error.
func getOK(ctx context.Context, c *http.Client, u string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return n, err
}

// unreachable passes when a TCP connection to addr does not open within
// timeout (a NetworkPolicy drops it, or nothing answers); it fails when the
// connection opens.
func unreachable(ctx context.Context, addr string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return "", fmt.Errorf("connected to %s in %s", addr, time.Since(start).Round(time.Millisecond))
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return fmt.Sprintf("no connection within %s", timeout), nil
	}
	return fmt.Sprintf("refused after %s: %v", time.Since(start).Round(time.Millisecond), err), nil
}

// parseTargets parses job=count,….
func parseTargets(s string) (map[string]int, error) {
	out := map[string]int{}
	for kv := range strings.SplitSeq(s, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		job, n, ok := strings.Cut(kv, "=")
		c, err := strconv.Atoi(n)
		if !ok || job == "" || err != nil || c < 1 {
			return nil, fmt.Errorf("-targets: %q is not job=count", kv)
		}
		out[job] = c
	}
	return out, nil
}

// targetsUp waits up to wait for Prometheus's active targets of each job in
// want to number exactly want[job] and all be up, polling every second; it
// reports the last counts.
func targetsUp(ctx context.Context, c *http.Client, base string, want map[string]int, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	var measured string
	for {
		var res struct {
			Data struct {
				ActiveTargets []struct {
					Labels map[string]string `json:"labels"`
					Health string            `json:"health"`
				} `json:"activeTargets"`
			} `json:"data"`
		}
		err := getJSON(ctx, c, strings.TrimSuffix(base, "/")+"/api/v1/targets?state=active", &res)
		if err == nil {
			total, up := map[string]int{}, map[string]int{}
			for _, t := range res.Data.ActiveTargets {
				job := t.Labels["job"]
				total[job]++
				if t.Health == "up" {
					up[job]++
				}
			}
			ok := true
			var parts []string
			for _, job := range slices.Sorted(maps.Keys(want)) {
				parts = append(parts, fmt.Sprintf("%s %d of %d up (want %d)", job, up[job], total[job], want[job]))
				ok = ok && up[job] == want[job] && total[job] == want[job]
			}
			measured = strings.Join(parts, ", ")
			if ok {
				return measured, nil
			}
		} else {
			measured = err.Error()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("after %s: %s", wait, measured)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
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
