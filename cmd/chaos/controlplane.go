package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ankitbisht23/spillway/internal/secret"
)

// make verify-m5's names (ADR-0042, test/verify/m5.yaml).
const (
	m5Name        = "m5-verify" // the plan, tenant and consumer
	m5Propagation = "m5-propagation"
	verifyHeader  = "X-Spillway-Verify"
	ciClient      = "spillway-ci"
	ciSecretEnv   = "SPILLWAY_CI_CLIENT_SECRET" //nolint:gosec // G101: the setting's name, not a credential
	ciSecretDev   = "dev-only-not-secret"       //nolint:gosec // G101: .env.example's dev-only value, used when the setting is unset
	// tokenMaxAge: a token older than this is replaced before a call;
	// Keycloak's tokens live 5 minutes and the outage step runs 12.
	tokenMaxAge = time.Minute
)

// cpOptions are "chaos controlplane"'s flags.
type cpOptions struct {
	step              string
	controlPlane      string // REST base URL, /api/v1 included
	controlPlaneAdmin string // admin base URL: /readyz
	tokenURL          string
	gateways          []string // replica names; traffic on :8000, admin on :8001
	runID             string
	restTimeout       time.Duration
	readyWait         time.Duration // ready: replicas live at the versions
	// propagation (rule a)
	publishes int
	limit     time.Duration // p99 below it (NFR-08); also rule b's per sample
	applyWait time.Duration // a publish missing on a replica after this counts as this
	poll      time.Duration
	pause     time.Duration // between publishes
	// revocation (rule b)
	rounds int
	// outage (rule c)
	outage outageOptions
}

func parseControlPlaneFlags(args []string, stderr io.Writer) (cpOptions, error) {
	o := cpOptions{}
	var gateways string
	fs := flag.NewFlagSet("chaos controlplane", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: chaos controlplane setup|ready|propagation|revocation|outage|cleanup [flags]")
		fs.PrintDefaults()
	}
	fs.StringVar(&o.controlPlane, "controlplane", "http://controlplane:8080/api/v1", "the control plane's REST API")
	fs.StringVar(&o.controlPlaneAdmin, "controlplane-admin", "http://controlplane:8081", "the control plane's admin port, for its /readyz")
	fs.StringVar(&o.tokenURL, "token-url", "http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/token", "Keycloak's token endpoint, for the "+ciClient+" client's service token")
	fs.StringVar(&gateways, "gateways", "gateway-1,gateway-2,gateway-3,gateway-4,gateway-5", "the replicas, comma-separated names")
	fs.StringVar(&o.runID, "run-id", "", "run ID in change notes and the verify header (default: the start time)")
	fs.DurationVar(&o.restTimeout, "rest-timeout", 10*time.Second, "per-call timeout of the REST API and Keycloak")
	fs.DurationVar(&o.readyWait, "ready-wait", time.Minute, "ready: how long the replicas may take to be live at the published versions")
	fs.IntVar(&o.publishes, "publishes", 60, "propagation: publishes (NFR-08, D10)")
	fs.DurationVar(&o.limit, "limit", 5*time.Second, "propagation: p99 must be below it; revocation: every sample (NFR-08, NFR-09)")
	fs.DurationVar(&o.applyWait, "apply-wait", 30*time.Second, "propagation: a publish not applied on a replica after this counts as this")
	fs.DurationVar(&o.poll, "poll", 100*time.Millisecond, "propagation: how often a version's applied list is read")
	fs.DurationVar(&o.pause, "pause", time.Second, "propagation: wait between publishes")
	fs.IntVar(&o.rounds, "rounds", 20, "revocation: revocations")
	oo := &o.outage
	fs.IntVar(&oo.rate, "rate", 500, "outage: requests per second over all replicas")
	fs.DurationVar(&oo.before, "before", 30*time.Second, "outage: load before the stop")
	fs.DurationVar(&oo.length, "outage", 600*time.Second, "outage: how long the control plane stays stopped")
	fs.StringVar(&oo.restart, "restart", "gateway-3", "outage: the replica restarted during the outage")
	fs.DurationVar(&oo.restartAt, "restart-at", 300*time.Second, "outage: when the replica is restarted, from the stop")
	fs.DurationVar(&oo.after, "after", 60*time.Second, "outage: load after the control plane is healthy")
	fs.DurationVar(&oo.every, "scrape-every", 5*time.Second, "outage: how often each replica's /readyz and /metrics are read")
	fs.DurationVar(&oo.within, "within", 60*time.Second, "outage: rules (c4) and (c5)'s bound")
	fs.DurationVar(&oo.slack, "slack", 10*time.Second, "outage: rule (c3)'s slack and (c5)'s sync age bound")
	fs.DurationVar(&oo.timeout, "request-timeout", 20*time.Second, "outage: the load client's per-request timeout")
	fs.IntVar(&oo.maxInFlight, "max-in-flight", 2000, "outage: a request due while this many are in flight is dropped")
	fs.DurationVar(&oo.actionWait, "action-wait", 3*time.Minute, "outage: how long the script may take to answer an action")
	var step string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		step, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.step = step
	for g := range strings.SplitSeq(gateways, ",") {
		if g = strings.TrimSpace(g); g != "" {
			o.gateways = append(o.gateways, g)
		}
	}
	if o.runID == "" {
		o.runID = strconv.FormatInt(time.Now().Unix(), 36)
	}
	var err error
	switch {
	case !slices.Contains([]string{"setup", "ready", "propagation", "revocation", "outage", "cleanup"}, step):
		err = fmt.Errorf("step %q: want setup, ready, propagation, revocation, outage or cleanup", step)
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case len(o.gateways) == 0:
		err = errors.New("-gateways: none")
	case o.publishes < 1 || o.rounds < 1 || oo.rate < 1 || oo.maxInFlight < 1:
		err = errors.New("-publishes, -rounds, -rate and -max-in-flight: must be positive")
	case o.restTimeout <= 0 || o.readyWait <= 0 || o.limit <= 0 || o.applyWait <= 0 || o.poll <= 0 || o.pause < 0:
		err = errors.New("-rest-timeout, -ready-wait, -limit, -apply-wait and -poll must be positive, -pause not negative")
	case oo.before <= 0 || oo.length <= 0 || oo.after <= 0 || oo.every <= 0 || oo.within <= 0 || oo.slack <= 0 || oo.timeout <= 0 || oo.actionWait <= 0:
		err = errors.New("the outage durations: must be positive")
	case oo.restartAt <= 0 || oo.restartAt >= oo.length:
		err = errors.New("-restart-at: must be positive and shorter than -outage")
	case !slices.Contains(o.gateways, oo.restart):
		err = fmt.Errorf("-restart %q: not one of -gateways", oo.restart)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos controlplane:", err)
	}
	return o, err
}

// runControlPlane is "chaos controlplane", the sender of make verify-m5
// (ADR-0042), in the compose network. It takes a service token for the
// spillway-ci client (its secret from SPILLWAY_CI_CLIENT_SECRET or its
// _FILE, else the dev value; never printed) and runs one step:
//
//   - setup: removes what an earlier run left of tenant m5-verify, imports
//     the config on stdin (test/verify/m5.yaml), publishes it (a 409, the
//     working set already published, is "already current"), and creates
//     tenant m5-verify on plan m5-verify and consumer m5-verify.
//   - ready: waits up to -ready-wait for every replica to be live at the
//     latest config version and the current identity version.
//   - propagation, revocation, outage: ADR-0042's rules (a), (b) and (c).
//   - cleanup: revokes the consumer's keys that are not revoked and deletes
//     consumer and tenant m5-verify, so that later imports may drop the plan.
//
// Each step prints its report as JSON on stdout and its PASS/FAIL lines on
// stderr, and exits 0 on PASS, 1 on FAIL and 2 on bad flags. No step
// prints a key or a token.
//
//	chaos controlplane <step> [flags]
func runControlPlane(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseControlPlaneFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	sec, err := secretFrom(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos controlplane:", err)
		return 2
	}
	return newCPRun(o, newCPClient(o.controlPlane, o.tokenURL, sec, o.restTimeout)).step(ctx, stdin, stdout, stderr)
}

// step runs r.o.step, prints its report and lines and returns the exit
// code.
func (r *cpRun) step(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	o := r.o
	var report any
	var lines []string
	pass := true
	fail := func(check string, err error) {
		lines, pass = append(lines, line(false, check, err.Error())), false
	}
	switch o.step {
	case "setup":
		yaml, err := io.ReadAll(io.LimitReader(stdin, 10<<20))
		if err != nil || len(yaml) == 0 {
			_, _ = fmt.Fprintln(stderr, "chaos controlplane: setup: no config on stdin", err)
			return 2
		}
		rep, err := r.setup(ctx, yaml)
		report = rep
		if err != nil {
			fail("setup", err)
		}
	case "ready":
		rep, err := r.ready(ctx)
		report = rep
		if err != nil {
			fail("setup", err)
		}
	case "propagation":
		rep := r.propagation(ctx)
		report, pass = rep, rep.Pass
		lines = rep.lines(len(o.gateways))
	case "revocation":
		rep, l, err := r.revocation(ctx)
		report, pass = rep, rep.Pass
		if err != nil {
			fail("(b) revoked key rejected on all replicas < 5 s", err)
		} else {
			lines = append(lines, l)
		}
	case "outage":
		rep := r.runOutage(ctx, stdin, stderr)
		report, pass = rep, rep.Pass
		lines = rep.lines(o.outage)
	case "cleanup":
		if err := r.removeTenant(ctx); err != nil {
			fail("cleanup", err)
		}
	}
	if report != nil {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	}
	for _, l := range lines {
		_, _ = fmt.Fprintln(stderr, l)
	}
	if !pass {
		return 1
	}
	return 0
}

// secretFrom is the spillway-ci client's secret: SPILLWAY_CI_CLIENT_SECRET
// or the file SPILLWAY_CI_CLIENT_SECRET_FILE names (SEC-09), else the dev
// value of .env.example. Errors never hold the value.
func secretFrom(getenv func(string) string) (secret.Value, error) {
	sec, err := secret.FromEnv(getenv, ciSecretEnv)
	if err == nil && !sec.IsSet() {
		sec = secret.New(ciSecretDev)
	}
	return sec, err
}

// cpClient calls the control plane's REST API with a service token.
type cpClient struct {
	base, tokenURL string
	secret         secret.Value
	client         *http.Client
	timeout        time.Duration

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

func newCPClient(base, tokenURL string, sec secret.Value, timeout time.Duration) *cpClient {
	return &cpClient{base: base, tokenURL: tokenURL, secret: sec, timeout: timeout,
		client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}}
}

// bearer returns a service token no older than tokenMaxAge.
func (c *cpClient) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenAt) < tokenMaxAge {
		return c.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {ciClient}, "client_secret": {c.secret.Reveal()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode())) //nolint:gosec // G704: Keycloak in the compose network is the point
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return "", fmt.Errorf("a token for %s: transport error or timeout", ciClient)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("a token for %s: status %d %s", ciClient, resp.StatusCode, out.Error)
	}
	c.token, c.tokenAt = out.AccessToken, time.Now()
	return c.token, nil
}

// answer is a REST answer.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// call sends method path with body: nil for none, []byte as it is (its
// Content-Type in header), anything else as JSON. header is name, value
// pairs.
func (c *cpClient) call(ctx context.Context, method, path string, body any, header ...string) (answer, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return answer{}, err
	}
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return answer{}, err
		}
		rd, ctype = bytes.NewReader(j), "application/json"
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd) //nolint:gosec // G704: the compose control plane is the point
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.client.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return answer{}, fmt.Errorf("%s %s: transport error or timeout", method, path)
	}
	defer func() { _ = resp.Body.Close() }()
	a := answer{status: resp.StatusCode, header: resp.Header}
	if a.body, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20)); err != nil {
		return a, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	return a, nil
}

// must is call, with any status but want an error that quotes the answer's
// error message (which never holds a key), and the answer decoded into
// out unless nil.
func (c *cpClient) must(ctx context.Context, want int, method, path string, body, out any, header ...string) (answer, error) {
	a, err := c.call(ctx, method, path, body, header...)
	if err != nil {
		return a, err
	}
	if a.status != want {
		return a, fmt.Errorf("%s %s: status %d, want %d: %s", method, path, a.status, want, errorMessage(a.body))
	}
	if out != nil {
		if err := json.Unmarshal(a.body, out); err != nil {
			return a, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return a, nil
}

// errorMessage is an error answer's message and details.
func errorMessage(body []byte) string {
	var e struct {
		Error   string `json:"error"`
		Details []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return "(no error message)"
	}
	msg := e.Error
	for i, d := range e.Details {
		if i == 5 {
			msg += fmt.Sprintf("; and %d more", len(e.Details)-i)
			break
		}
		msg += fmt.Sprintf("; %s: %s", d.Path, d.Message)
	}
	return msg
}

// cpRun is one step's state.
type cpRun struct {
	o    cpOptions
	rest *cpClient
	// traffic and admin are a replica's host:port; tests change them.
	traffic, admin func(replica string) string
}

func newCPRun(o cpOptions, rest *cpClient) *cpRun {
	return &cpRun{o: o, rest: rest,
		traffic: func(g string) string { return g + ":8000" },
		admin:   func(g string) string { return g + ":8001" }}
}

// lookup returns the ID of the entity named name in collection, 0 for
// none; filter is query name, value pairs.
func (r *cpRun) lookup(ctx context.Context, collection, name string, filter ...string) (int64, error) {
	q := url.Values{"name": {name}}
	for i := 0; i+1 < len(filter); i += 2 {
		q.Set(filter[i], filter[i+1])
	}
	var list struct {
		Items []struct{ ID int64 } `json:"items"`
	}
	if _, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, collection+"?"+q.Encode(), nil, &list); err != nil {
		return 0, err
	}
	if len(list.Items) == 0 {
		return 0, nil
	}
	return list.Items[0].ID, nil
}

// published is a ConfigVersion as publish and GET /config/versions/{v}
// answer it.
type published struct {
	Version     int64     `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	Applied     []struct {
		Replica string    `json:"replica"`
		At      time.Time `json:"at"`
	} `json:"applied"`
}

// publish validates the working set and publishes it at the validated
// revision with note. With current, a 409 (nothing to publish) answers the
// latest version instead.
func (r *cpRun) publish(ctx context.Context, note string, current bool) (published, error) {
	var v struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	a, err := r.rest.must(ctx, http.StatusOK, http.MethodPost, "/config/validate", nil, &v)
	if err != nil {
		return published{}, err
	}
	if !v.Valid {
		var msgs []string
		for _, e := range v.Errors {
			msgs = append(msgs, e.Path+": "+e.Message)
		}
		return published{}, fmt.Errorf("the working set does not validate: %s", strings.Join(msgs, "; "))
	}
	b, err := r.rest.call(ctx, http.MethodPost, "/config/publish", map[string]string{"note": note}, "If-Match", a.header.Get("ETag"))
	if err != nil {
		return published{}, err
	}
	var p published
	switch {
	case b.status == http.StatusOK:
		err = json.Unmarshal(b.body, &p)
	case b.status == http.StatusConflict && current:
		var list struct{ Items []published }
		if _, err = r.rest.must(ctx, http.StatusOK, http.MethodGet, "/config/versions?limit=1", nil, &list); err == nil {
			if len(list.Items) == 0 {
				err = errors.New("publish answered 409 but no version exists")
			} else {
				p = list.Items[0]
			}
		}
	default:
		err = fmt.Errorf("POST /config/publish: status %d: %s", b.status, errorMessage(b.body))
	}
	return p, err
}

// setupReport is what setup made.
type setupReport struct {
	ConfigVersion int64 `json:"config_version"`
	PlanID        int64 `json:"plan_id"`
	TenantID      int64 `json:"tenant_id"`
	ConsumerID    int64 `json:"consumer_id"`
}

func (r *cpRun) setup(ctx context.Context, yaml []byte) (setupReport, error) {
	var rep setupReport
	if err := r.removeTenant(ctx); err != nil {
		return rep, fmt.Errorf("removing an earlier run's tenant: %w", err)
	}
	a, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, "/config/export", nil, nil)
	if err != nil {
		return rep, err
	}
	note := "make verify-m5 " + r.o.runID + ": test/verify/m5.yaml"
	if _, err := r.rest.must(ctx, http.StatusOK, http.MethodPost, "/config/import?"+url.Values{"note": {note}}.Encode(), yaml, nil,
		"If-Match", a.header.Get("ETag"), "Content-Type", "application/yaml"); err != nil {
		return rep, err
	}
	p, err := r.publish(ctx, note, true)
	if err != nil {
		return rep, err
	}
	rep.ConfigVersion = p.Version
	if rep.PlanID, err = r.lookup(ctx, "/plans", m5Name); err != nil || rep.PlanID == 0 {
		return rep, cmpErr(err, "plan "+m5Name+" is not in the working set")
	}
	var created struct{ ID int64 }
	if _, err := r.rest.must(ctx, http.StatusCreated, http.MethodPost, "/tenants", map[string]any{"name": m5Name, "plan_id": rep.PlanID}, &created); err != nil {
		return rep, err
	}
	rep.TenantID = created.ID
	if _, err := r.rest.must(ctx, http.StatusCreated, http.MethodPost, "/consumers", map[string]any{"tenant_id": rep.TenantID, "name": m5Name}, &created); err != nil {
		return rep, err
	}
	rep.ConsumerID = created.ID
	return rep, nil
}

func cmpErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}

// consumer returns consumer m5-verify's ID, 0 for none.
func (r *cpRun) consumer(ctx context.Context) (tenant, consumer int64, err error) {
	if tenant, err = r.lookup(ctx, "/tenants", m5Name); err != nil || tenant == 0 {
		return 0, 0, err
	}
	consumer, err = r.lookup(ctx, "/consumers", m5Name, "tenant_id", strconv.FormatInt(tenant, 10))
	return tenant, consumer, err
}

// removeTenant revokes consumer m5-verify's keys that are not revoked and
// deletes the consumer and tenant m5-verify, if they exist.
func (r *cpRun) removeTenant(ctx context.Context) error {
	tenant, consumer, err := r.consumer(ctx)
	if err != nil || tenant == 0 {
		return err
	}
	if consumer != 0 {
		var keys struct {
			Items []struct {
				ID        int64      `json:"id"`
				RevokedAt *time.Time `json:"revoked_at"`
			} `json:"items"`
		}
		if _, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, fmt.Sprintf("/consumers/%d/keys?limit=500", consumer), nil, &keys); err != nil {
			return err
		}
		for _, k := range keys.Items {
			if k.RevokedAt == nil {
				if _, err := r.rest.must(ctx, http.StatusOK, http.MethodPost, fmt.Sprintf("/keys/%d/revoke", k.ID), map[string]string{"note": "make verify-m5: cleanup"}, nil); err != nil {
					return err
				}
			}
		}
		// The revocations changed the consumer's version.
		var c struct{ Version int64 }
		if _, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, fmt.Sprintf("/consumers/%d", consumer), nil, &c); err != nil {
			return err
		}
		if _, err := r.rest.must(ctx, http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/consumers/%d", consumer), nil, nil, "If-Match", etag(c.Version)); err != nil {
			return err
		}
	}
	var t struct{ Version int64 }
	if _, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, fmt.Sprintf("/tenants/%d", tenant), nil, &t); err != nil {
		return err
	}
	_, err = r.rest.must(ctx, http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/tenants/%d", tenant), nil, nil, "If-Match", etag(t.Version))
	return err
}

func etag(version int64) string { return fmt.Sprintf("%q", strconv.FormatInt(version, 10)) }

// readyReport is what ready saw.
type readyReport struct {
	ConfigVersion   int64              `json:"config_version"`
	IdentityVersion int64              `json:"identity_version"`
	TookMS          float64            `json:"took_ms"`
	Replicas        map[string]replica `json:"replicas"`
}

type replica struct {
	ID              string `json:"id"`
	ConfigVersion   int64  `json:"config_version"`
	IdentityVersion int64  `json:"identity_version"`
	WatchConnected  bool   `json:"watch_connected"`
	Live            bool   `json:"live"`
}

// replicas lists the replicas of r.o.gateways by name.
func (r *cpRun) replicas(ctx context.Context) (map[string]replica, error) {
	var list struct{ Items []replica }
	if _, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, "/replicas?limit=500", nil, &list); err != nil {
		return nil, err
	}
	out := map[string]replica{}
	for _, x := range list.Items {
		if slices.Contains(r.o.gateways, x.ID) {
			out[x.ID] = x
		}
	}
	return out, nil
}

// versions are the latest config version and the identity version.
func (r *cpRun) versions(ctx context.Context) (config, identity int64, err error) {
	var list struct{ Items []published }
	if _, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, "/config/versions?limit=1", nil, &list); err != nil {
		return 0, 0, err
	}
	if len(list.Items) == 0 {
		return 0, 0, errors.New("no config version is published")
	}
	a, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, "/identity/export", nil, nil)
	if err != nil {
		return 0, 0, err
	}
	// Its first line is "version: N"; the rest is not read.
	first, _, _ := bytes.Cut(a.body, []byte("\n"))
	v, ok := strings.CutPrefix(strings.TrimSpace(string(first)), "version: ")
	if identity, err = strconv.ParseInt(v, 10, 64); !ok || err != nil {
		return 0, 0, errors.New("GET /identity/export: no version on its first line")
	}
	return list.Items[0].Version, identity, nil
}

func (r *cpRun) ready(ctx context.Context) (readyReport, error) {
	var rep readyReport
	start := time.Now()
	var err error
	if rep.ConfigVersion, rep.IdentityVersion, err = r.versions(ctx); err != nil {
		return rep, err
	}
	for {
		rep.Replicas, err = r.replicas(ctx)
		if err == nil && r.allAt(rep.Replicas, rep.ConfigVersion, rep.IdentityVersion) {
			rep.TookMS = ms(time.Since(start))
			return rep, nil
		}
		if time.Since(start) > r.o.readyWait || ctx.Err() != nil {
			if err == nil {
				err = fmt.Errorf("after %v, not every replica is live at config %d and identity %d: %s",
					r.o.readyWait, rep.ConfigVersion, rep.IdentityVersion, describeReplicas(r.o.gateways, rep.Replicas))
			}
			return rep, err
		}
		sleepUntil(ctx, time.Now().Add(500*time.Millisecond))
	}
}

// allAt reports whether every gateway is live and watching, at config cv
// and identity iv (0: any).
func (r *cpRun) allAt(rs map[string]replica, cv, iv int64) bool {
	for _, g := range r.o.gateways {
		x, ok := rs[g]
		if !ok || !x.Live || !x.WatchConnected || (cv != 0 && x.ConfigVersion != cv) || (iv != 0 && x.IdentityVersion != iv) {
			return false
		}
	}
	return true
}

func describeReplicas(gateways []string, rs map[string]replica) string {
	var parts []string
	for _, g := range gateways {
		x, ok := rs[g]
		if !ok {
			parts = append(parts, g+" never heartbeat")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s live %v watching %v at %d/%d", g, x.Live, x.WatchConnected, x.ConfigVersion, x.IdentityVersion))
	}
	return strings.Join(parts, ", ")
}

// propagationSample is one publish of rule (a), times in milliseconds
// after its published_at, on Postgres's clock.
type propagationSample struct {
	Version   int64              `json:"version"`
	AppliedMS map[string]float64 `json:"applied_ms"`
	Missing   []string           `json:"missing,omitempty"`
	SampleMS  float64            `json:"sample_ms"`
}

type propagationReport struct {
	Samples []propagationSample `json:"samples"`
	N       int                 `json:"n"`
	P50MS   float64             `json:"p50_ms"`
	P99MS   float64             `json:"p99_ms"`
	MaxMS   float64             `json:"max_ms"`
	LimitMS float64             `json:"limit_ms"`
	Error   string              `json:"error,omitempty"`
	Pass    bool                `json:"pass"`
}

// nearestRank is the p-th percentile of sorted by nearest rank.
func nearestRank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

// propagation is rule (a), NFR-08: r.o.publishes times, edit the
// m5-propagation route's verify header, validate, publish, and read the
// version's applied list every r.o.poll until it holds every gateway; the
// sample is the latest gateway's first apply heartbeat after published_at.
// PASS when p99 of all samples is below r.o.limit.
func (r *cpRun) propagation(ctx context.Context) propagationReport {
	rep := propagationReport{LimitMS: ms(r.o.limit)}
	id, err := r.lookup(ctx, "/routes", m5Propagation)
	if err == nil && id == 0 {
		err = errors.New("route " + m5Propagation + " is not in the working set")
	}
	for i := 0; err == nil && i < r.o.publishes; i++ {
		var s propagationSample
		if s, err = r.propagate(ctx, id, i); err == nil {
			rep.Samples = append(rep.Samples, s)
			sleepUntil(ctx, time.Now().Add(r.o.pause))
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	var sorted []float64
	for _, s := range rep.Samples {
		sorted = append(sorted, s.SampleMS)
	}
	slices.Sort(sorted)
	rep.N = len(sorted)
	rep.P50MS, rep.P99MS = nearestRank(sorted, 0.5), nearestRank(sorted, 0.99)
	if rep.N > 0 {
		rep.MaxMS = sorted[rep.N-1]
	}
	if err != nil {
		rep.Error = err.Error()
	}
	rep.Pass = err == nil && rep.N == r.o.publishes && rep.P99MS < rep.LimitMS
	return rep
}

// propagate makes publish i and times it.
func (r *cpRun) propagate(ctx context.Context, id int64, i int) (propagationSample, error) {
	var s propagationSample
	path := fmt.Sprintf("/routes/%d", id)
	var route map[string]any
	a, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, path, nil, &route)
	if err != nil {
		return s, err
	}
	for _, k := range []string{"id", "version", "created_at", "updated_at"} {
		delete(route, k)
	}
	route["response_headers"] = map[string]any{"set": []map[string]string{{"name": verifyHeader, "value": fmt.Sprintf("%s-%d", r.o.runID, i)}}}
	if _, err := r.rest.must(ctx, http.StatusOK, http.MethodPut, path, route, nil, "If-Match", a.header.Get("ETag")); err != nil {
		return s, err
	}
	p, err := r.publish(ctx, fmt.Sprintf("make verify-m5 %s %d", r.o.runID, i), false)
	if err != nil {
		return s, err
	}
	s.Version = p.Version
	deadline := time.Now().Add(r.o.applyWait)
	var got published
	for {
		if _, err := r.rest.must(ctx, http.StatusOK, http.MethodGet, fmt.Sprintf("/config/versions/%d", p.Version), nil, &got); err != nil {
			return s, err
		}
		s.AppliedMS = map[string]float64{}
		for _, x := range got.Applied {
			if slices.Contains(r.o.gateways, x.Replica) {
				s.AppliedMS[x.Replica] = ms(x.At.Sub(got.PublishedAt))
			}
		}
		if len(s.AppliedMS) == len(r.o.gateways) || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		sleepUntil(ctx, time.Now().Add(r.o.poll))
	}
	for _, g := range r.o.gateways {
		at, ok := s.AppliedMS[g]
		if !ok {
			s.Missing, at = append(s.Missing, g), ms(r.o.applyWait)
		}
		s.SampleMS = max(s.SampleMS, at)
	}
	return s, nil
}

func (rep propagationReport) lines(replicas int) []string {
	measured := fmt.Sprintf("%d publishes: p50 %.0f ms, p99 %.0f ms (nearest rank), max %.0f ms; limit %.0f ms", rep.N, rep.P50MS, rep.P99MS, rep.MaxMS, rep.LimitMS)
	if rep.Error != "" {
		measured += "; stopped: " + rep.Error
	}
	ls := []string{line(rep.Pass, fmt.Sprintf("(a) publish on all %d replicas, p99 < 5 s", replicas), measured)}
	var late []string
	for _, s := range rep.Samples {
		if len(s.Missing) > 0 {
			late = append(late, fmt.Sprintf("version %d missing on %s", s.Version, strings.Join(s.Missing, ", ")))
		}
	}
	if len(late) > 0 {
		ls = append(ls, "      "+strings.Join(late, "; "))
	}
	return ls
}

// revocation is rule (b), NFR-09: chaos revocation's rounds for consumer
// m5-verify on every gateway.
func (r *cpRun) revocation(ctx context.Context) (revocationReport, string, error) {
	_, consumer, err := r.consumer(ctx)
	if err == nil && consumer == 0 {
		err = errors.New("consumer " + m5Name + " does not exist: run setup first")
	}
	if err != nil {
		return revocationReport{}, "", err
	}
	o := revocationOptions{
		controlPlane: r.o.controlPlane, consumer: consumer, path: "/status/200",
		rounds: r.o.rounds, limit: r.o.limit, every: 50 * time.Millisecond, after: 10,
		timeout: 4 * time.Second, restTimeout: r.o.restTimeout, readyWait: 10 * time.Second,
	}
	for _, g := range r.o.gateways {
		o.gateways = append(o.gateways, r.traffic(g))
	}
	rr := revocationRun{o: o, client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}, bearer: r.rest.bearer}
	rep, _ := rr.run(ctx)
	measured := fmt.Sprintf("%d revocations: p50 %.0f ms, max %.0f ms; limit %.0f ms, every sample (sender's clock)", len(rep.Rounds), rep.P50MS, rep.MaxMS, rep.LimitMS)
	for _, x := range rep.Rounds {
		if x.Error != "" {
			measured += fmt.Sprintf("; key %d: %s", x.KeyID, x.Error)
			break
		}
	}
	return rep, line(rep.Pass, fmt.Sprintf("(b) revoked key rejected on all %d replicas < 5 s", len(r.o.gateways)), measured), nil
}

// stdinLines delivers stdin's lines, each with the time it was read.
type stdinLine struct {
	text string
	at   time.Time
}

func readLines(stdin io.Reader) <-chan stdinLine {
	ch := make(chan stdinLine)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			ch <- stdinLine{strings.TrimSpace(sc.Text()), time.Now()}
		}
	}()
	return ch
}
