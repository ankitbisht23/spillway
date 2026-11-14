package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/local"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// Bearer tokens and auth modes in the gateway (GW-18 - GW-20, SEC-04,
// ADR-0039). The issuer, its keys and the clock tokens are checked on are
// the test's.

const (
	// tokenIssuer is the browser-facing issuer every token must carry; the
	// keys come from the test's own JWKS URL (GW-18, ADR-0004).
	tokenIssuer   = "http://localhost:8180/auth/realms/spillway" //nolint:gosec // G101: an issuer URL, not a credential
	tokenAudience = "spillway-gateway"
	aliceSubject  = "svc-alice-subject"
)

// jwks is a test issuer: its signing keys and the key set it serves.
type jwks struct {
	*httptest.Server
	rsa     *rsa.PrivateKey
	ec      *ecdsa.PrivateKey
	mu      sync.Mutex
	keys    []jose.JSONWebKey
	status  int
	fetches atomic.Int32
}

func newJWKS(t *testing.T) *jwks {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	j := &jwks{rsa: rk, ec: ek, status: http.StatusOK, keys: []jose.JSONWebKey{
		{Key: rk.Public(), KeyID: "rsa-1", Algorithm: "RS256", Use: "sig"},
		{Key: ek.Public(), KeyID: "ec-1", Algorithm: "ES256", Use: "sig"},
	}}
	j.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		j.fetches.Add(1)
		j.mu.Lock()
		defer j.mu.Unlock()
		w.WriteHeader(j.status)
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: j.keys})
	}))
	t.Cleanup(j.Close)
	return j
}

func (j *jwks) set(f func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f()
}

// sign signs claims as a compact JWS with alg and key under kid.
func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// tokenClock is the clock tokens are checked on; it starts on a whole
// second, so leeway edges are exact.
type tokenClock struct{ now atomic.Int64 }

func newTokenClock() *tokenClock {
	c := &tokenClock{}
	c.now.Store(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *tokenClock) Now() time.Time      { return time.Unix(0, c.now.Load()) }
func (c *tokenClock) add(d time.Duration) { c.now.Add(int64(d)) }
func (c *tokenClock) unix(d time.Duration) int64 {
	return c.Now().Add(d).Unix()
}

// claims are a client-credentials access token's claims, as Keycloak
// shapes them, for sub.
func (c *tokenClock) claims(sub string) map[string]any {
	return map[string]any{
		"iss": tokenIssuer, "sub": sub, "aud": tokenAudience, "azp": "gateway-tests",
		"exp": c.unix(5 * time.Minute), "iat": c.unix(0),
	}
}

// with is claims with f applied.
func (c *tokenClock) with(f func(m map[string]any)) map[string]any {
	m := c.claims(aliceSubject)
	f(m)
	return m
}

// jwtGateway is a managed gateway with a route per auth mode, consumer 3
// (tenant acme, plan 2) with a key and the JWT subject aliceSubject, and
// consumer 4 with neither. Its own log goes to events and the verifier's
// to keyLog.
type jwtGateway struct {
	*gateway
	key    string // consumer 3's API key
	keyLog *logBuffer
	v      *auth.Verifier
}

const authModesConfig = `
upstreams: [{name: orders, endpoints: [{address: 'orders:%s'}]}]
routes:
  - {name: open, upstream: orders, match: {path_prefix: /none}}
  - {name: keys, upstream: orders, match: {path_prefix: /api_key}, auth_mode: api_key}
  - {name: tokens, upstream: orders, match: {path_prefix: /jwt}, auth_mode: jwt}
  - {name: either, upstream: orders, match: {path_prefix: /either}, auth_mode: either}
policies:
  - {name: by-consumer, level: consumer, match: {routes: ['*']}, key_template: '{tenant}:{consumer}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
  - {name: by-key, level: consumer, match: {routes: ['*']}, key_template: '{api_key}', algorithm: gcra, limits: [{rate: 1000, period: 1s}], local: true}
`

func newJWTGateway(t *testing.T, j *jwks, clock *tokenClock, tracer trace.Tracer) *jwtGateway {
	t.Helper()
	u := newUpstream(t, "orders")
	g := &gateway{events: &logBuffer{}, clock: ratelimit.NewFakeClock(t0), metrics: prometheus.NewRegistry(), logs: &logBuffer{}}
	g.reg = NewRegistry(loopback, newResolver(u), slog.New(slog.NewJSONHandler(g.events, nil)))
	t.Cleanup(g.reg.Close)
	g.limiters = local.NewRegistry(g.clock)
	g.apply(t, fmt.Sprintf(authModesConfig, u.port()))

	k, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := config.CompileIdentity(&configpb.Identity{
		Version: 1, KeyFormat: config.KeyFormatManaged,
		Tenants: []*configpb.Tenant{{Id: 1, Name: "acme", PlanId: 2, Status: "active"}},
		Consumers: []*configpb.Consumer{
			{Id: 3, TenantId: 1, Name: "alice", Status: "active", JwtSubject: aliceSubject},
			{Id: 4, TenantId: 1, Name: "bob", Status: "active"},
		},
		ApiKeys: []*configpb.ApiKey{{Id: 1, ConsumerId: 3, Prefix: k.Prefix, Hash: fmt.Sprintf("%x", k.Hash[:])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	g.identity.Store(id)

	jg := &jwtGateway{gateway: g, key: k.Secret.Reveal(), keyLog: &logBuffer{}}
	jg.v, err = auth.New(auth.Config{
		Issuer: tokenIssuer, JWKSURL: j.URL, Audience: tokenAudience, AllowHTTP: true, Now: clock.Now,
		Log: slog.New(slog.NewJSONHandler(jg.keyLog, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := telemetry.NewRecorder(g.metrics, slog.New(slog.NewJSONHandler(g.logs, nil)), 1)
	g.h = NewHandler(g.current.Load, g.identity.Load, nil, g.reg, g.limiters, nil, false, 4096, tracer, rec, slog.New(slog.NewJSONHandler(g.events, nil)))
	g.h.SetTokens(jg.v)
	g.Server = httptest.NewServer(g.h)
	t.Cleanup(g.Close)
	return jg
}

// run starts the verifier's fetches and waits for the first to end.
func (jg *jwtGateway) run(t *testing.T, j *jwks) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go jg.v.Run(ctx)
	for deadline := time.Now().Add(5 * time.Second); j.fetches.Load() == 0 || jg.v.Ready() != (j.status == http.StatusOK); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no key fetch after 5 s")
		}
	}
}

// authAnswer is a gateway's answer: its status, headers and body, and for a
// 200 the upstream's echo.
type authAnswer struct {
	status int
	header http.Header
	body   string
	echo   echo
}

func (jg *jwtGateway) call(t *testing.T, path, id string, header http.Header) authAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, jg.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("X-Request-Id", id)
	resp, err := jg.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	a := authAnswer{status: resp.StatusCode, header: resp.Header, body: string(body)}
	if a.status == http.StatusOK {
		if err := json.Unmarshal(body, &a.echo); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// The gateway's answers to authentication failures (ADR-0039).
//
//nolint:gosec // G101: answers, not credentials
const (
	keyRequired      = `401 {"error":"API key required"}`
	invalidKey       = `401 {"error":"invalid API key"}`
	tokenRequired    = `401 {"error":"bearer token required"} Bearer realm="spillway"`
	eitherRequired   = `401 {"error":"API key or bearer token required"} Bearer realm="spillway"`
	invalidToken     = `401 {"error":"invalid token"} Bearer realm="spillway", error="invalid_token"`
	keysUnavailable  = `503 {"error":"token keys unavailable"} retry 1`
	asConsumer       = "consumer"  // 200, identified as consumer 3
	viaKey           = "key"       // 200, identified as consumer 3 by its key
	anonymous        = "anonymous" // 200, no identity
	consumerTemplate = "by-consumer"
	keyTemplate      = "by-key"
)

// outcome sums up an answer as the constants above do.
func (a authAnswer) outcome() string {
	if a.status != http.StatusOK {
		s := fmt.Sprintf("%d %s", a.status, a.body)
		if c := a.header.Get("WWW-Authenticate"); c != "" {
			s += " " + c
		}
		if r := a.header.Get("Retry-After"); r != "" {
			s += " retry " + r
		}
		return s
	}
	// The identity policies apply only when their templates fill (ADR-0017).
	switch p := strings.Join(a.header.Values("RateLimit-Policy"), ","); {
	case strings.Contains(p, consumerTemplate) && strings.Contains(p, keyTemplate):
		return viaKey
	case strings.Contains(p, consumerTemplate):
		return asConsumer
	case p == "":
		return anonymous
	default:
		return "200 with policies " + p
	}
}

// TestAuthModes runs every credential a request can carry against every
// auth mode (GW-20, ADR-0039's table): a sent key decides on every mode but
// jwt; jwt and either without a key verify the bearer token; none is
// anonymous without a key; the header not read goes upstream unchanged; a
// token's subject resolves to its consumer, tenant and plan like a key
// (GW-19), filling {consumer} and {tenant} but not {api_key}. No key, token,
// signature or subject appears in the access log, the gateway's log, a span,
// a metric or an error body (SEC-03).
func TestAuthModes(t *testing.T) {
	j, clock := newJWKS(t), newTokenClock()
	sr, tracer := newTracer(t, sdktrace.AlwaysSample())
	g := newJWTGateway(t, j, clock, tracer)
	g.run(t, j)

	token := sign(t, jose.RS256, j.rsa, "rsa-1", clock.claims(aliceSubject))
	bad := sign(t, jose.RS256, j.rsa, "rsa-1", clock.with(func(m map[string]any) { m["aud"] = "spillway-controlplane" }))
	noConsumer := sign(t, jose.RS256, j.rsa, "rsa-1", clock.claims("svc-nobody"))
	wrongKey := g.key[:len(g.key)-1] + map[bool]string{true: "1", false: "0"}[g.key[len(g.key)-1] == '0']
	bearer := func(tok string) []string { return []string{"Bearer " + tok} }

	type creds struct {
		name  string
		key   []string
		authz []string
	}
	cases := []creds{
		{"nothing", nil, nil},
		{"key", []string{g.key}, nil},
		{"wrong key", []string{wrongKey}, nil},
		{"two keys", []string{g.key, g.key}, nil},
		{"token", nil, bearer(token)},
		{"token, lower-case scheme", nil, []string{"bearer " + token}},
		{"bad token", nil, bearer(bad)},
		{"token without a consumer", nil, bearer(noConsumer)},
		{"Basic credentials", nil, []string{"Basic YWxpY2U6c2VjcmV0"}},
		{"Bearer with no token", nil, []string{"Bearer "}},
		{"two tokens", nil, append(bearer(token), bearer(token)...)},
		{"key and token", []string{g.key}, bearer(token)},
		{"key and bad token", []string{g.key}, bearer(bad)},
		{"wrong key and token", []string{wrongKey}, bearer(token)},
	}
	want := map[string][]string{ // per route: the outcome of each case, in order
		"none": {anonymous, viaKey, invalidKey, invalidKey, anonymous, anonymous, anonymous, anonymous, anonymous, anonymous, anonymous, viaKey, viaKey, invalidKey},
		"api_key": {keyRequired, viaKey, invalidKey, invalidKey, keyRequired, keyRequired, keyRequired, keyRequired, keyRequired, keyRequired, keyRequired,
			viaKey, viaKey, invalidKey},
		"jwt": {tokenRequired, tokenRequired, tokenRequired, tokenRequired, asConsumer, asConsumer, invalidToken, invalidToken, invalidToken, invalidToken, invalidToken,
			asConsumer, invalidToken, asConsumer},
		"either": {eitherRequired, viaKey, invalidKey, invalidKey, asConsumer, asConsumer, invalidToken, invalidToken, invalidToken, invalidToken, invalidToken,
			viaKey, viaKey, invalidKey},
	}
	var bodies strings.Builder
	n := 0
	for _, mode := range []string{"none", "api_key", "jwt", "either"} {
		for i, c := range cases {
			n++
			id := "auth-" + strconv.Itoa(n)
			h := http.Header{}
			if c.key != nil {
				h["X-Api-Key"] = c.key
			}
			if c.authz != nil {
				h["Authorization"] = c.authz
			}
			a := g.call(t, "/"+mode+"/orders", id, h)
			if a.status != http.StatusOK {
				bodies.WriteString(a.body + "\n")
			}
			if got := a.outcome(); got != want[mode][i] {
				t.Errorf("%s route, %s: %s; want %s", mode, c.name, got, want[mode][i])
			}
			if a.status == http.StatusOK {
				// Credentials go upstream as sent, read or not (HTTP drops
				// a trailing space).
				if strings.Join(a.echo.Header.Values("X-Api-Key"), ",") != strings.Join(c.key, ",") ||
					strings.Join(a.echo.Header.Values("Authorization"), ",") != strings.TrimSpace(strings.Join(c.authz, ",")) {
					t.Errorf("%s route, %s: upstream got X-API-Key %d values, Authorization %d values; want them unchanged",
						mode, c.name, len(a.echo.Header.Values("X-Api-Key")), len(a.echo.Header.Values("Authorization")))
				}
			} else if a.header.Get("Content-Type") != "application/json" || a.header.Get("X-Request-Id") != id {
				t.Errorf("%s route, %s: Content-Type %q, request ID %q", mode, c.name, a.header.Get("Content-Type"), a.header.Get("X-Request-Id"))
			}
			// The access log has a key's hash only when the key was read.
			wantHash := ""
			if c.key != nil && mode != "jwt" {
				wantHash = config.KeyHash(c.key[0])
			}
			if line := g.logs.access(t, id); line["key_hash"] != wantHash {
				t.Errorf("%s route, %s: access log key_hash %v, want %q", mode, c.name, line["key_hash"], wantHash)
			}
		}
	}

	// A token's consumer counts under its tenant's name (GW-19, OB-01), and
	// every failure is a gateway answer.
	m := scrape(t, g.metrics)
	for _, s := range []string{
		`gw_requests_total{code_class="2xx",method="GET",origin="upstream",reason="none",route="tokens",tenant="acme"} 4`,
		`gw_requests_total{code_class="4xx",method="GET",origin="gateway",reason="none",route="tokens",tenant="none"} 10`,
	} {
		if !strings.Contains(m, "\n"+s+"\n") {
			t.Errorf("/metrics has no %s", s)
		}
	}
	spans := spanText(sr)
	for _, r := range []string{"consumer", "anonymous", "invalid", "missing"} {
		if !strings.Contains(spans, `spillway.authn.result","Value":{"Type":"STRING","Value":"`+r+`"`) {
			t.Errorf("no authn span with result %s", r)
		}
	}
	noCredentials(t, map[string]string{
		"access log": g.logs.String(), "gateway log": g.events.String(), "key fetch log": g.keyLog.String(),
		"spans": spans, "metrics": m, "error bodies": bodies.String(),
	}, []string{token, bad, noConsumer}, []string{g.key, wrongKey})
}

// spanText is every ended span's name and attributes, as JSON.
func spanText(sr *tracetest.SpanRecorder) string {
	var b strings.Builder
	for _, s := range sr.Ended() {
		attrs, _ := json.Marshal(s.Attributes())
		fmt.Fprintf(&b, "%s %s %v\n", s.Name(), attrs, s.Events())
	}
	return b.String()
}

// noCredentials fails the test if any output holds a token, its signature
// or payload, a subject, or a key or its secret (SEC-03, ADR-0039).
func noCredentials(t *testing.T, outputs map[string]string, tokens, keys []string) {
	t.Helper()
	secrets := map[string]string{"subject": aliceSubject}
	for i, tok := range tokens {
		parts := strings.Split(tok, ".")
		secrets[fmt.Sprintf("token %d", i)], secrets[fmt.Sprintf("token %d's payload", i)], secrets[fmt.Sprintf("token %d's signature", i)] = tok, parts[1], parts[2]
	}
	for i, k := range keys {
		secrets[fmt.Sprintf("key %d", i)] = k
		if _, sec, ok := apikey.Parse(k); ok {
			secrets[fmt.Sprintf("key %d's secret", i)] = sec
		}
	}
	for what, out := range outputs {
		for name, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("the %s holds %s", what, name)
			}
		}
	}
}

// TestTokenChecks sends tokens that fail one check each through a jwt
// route, and the ones that pass at the leeway's edges (GW-18, SEC-04):
// only RS256 and ES256 (never none, never HS256 keyed with the issuer's
// public key), iss the browser-facing issuer exactly, aud the gateway's,
// exp and nbf with 60 s of leeway on the gateway's clock.
func TestTokenChecks(t *testing.T) {
	j, clock := newJWKS(t), newTokenClock()
	g := newJWTGateway(t, j, clock, nil)
	g.run(t, j)

	der, err := x509.MarshalPKIXPublicKey(j.rsa.Public())
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	b64 := base64.RawURLEncoding.EncodeToString
	good := sign(t, jose.RS256, j.rsa, "rsa-1", clock.claims(aliceSubject))
	payload := strings.Split(good, ".")[1]
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	set := func(k string, v any) map[string]any { return clock.with(func(m map[string]any) { m[k] = v }) }
	unset := func(k string) map[string]any { return clock.with(func(m map[string]any) { delete(m, k) }) }

	for name, tt := range map[string]struct {
		token string
		ok    bool
	}{
		"RS256":                         {good, true},
		"ES256":                         {sign(t, jose.ES256, j.ec, "ec-1", clock.claims(aliceSubject)), true},
		"alg none":                      {b64([]byte(`{"alg":"none","kid":"rsa-1","typ":"JWT"}`)) + "." + payload + ".", false},
		"alg none, signature kept":      {b64([]byte(`{"alg":"none","kid":"rsa-1"}`)) + "." + payload + "." + strings.Split(good, ".")[2], false},
		"HS256 keyed with the PEM":      {sign(t, jose.HS256, publicPEM, "rsa-1", clock.claims(aliceSubject)), false},
		"HS256 keyed with the modulus":  {sign(t, jose.HS256, j.rsa.N.Bytes(), "rsa-1", clock.claims(aliceSubject)), false},
		"HS512 keyed with the PEM":      {sign(t, jose.HS512, publicPEM, "rsa-1", clock.claims(aliceSubject)), false},
		"RS512":                         {sign(t, jose.RS512, j.rsa, "rsa-1", clock.claims(aliceSubject)), false},
		"PS256":                         {sign(t, jose.PS256, j.rsa, "rsa-1", clock.claims(aliceSubject)), false},
		"signed by another key":         {sign(t, jose.RS256, other, "rsa-1", clock.claims(aliceSubject)), false},
		"in-network issuer":             {sign(t, jose.RS256, j.rsa, "rsa-1", set("iss", "http://keycloak:8080/auth/realms/spillway")), false},
		"issuer with a trailing slash":  {sign(t, jose.RS256, j.rsa, "rsa-1", set("iss", tokenIssuer+"/")), false},
		"no issuer":                     {sign(t, jose.RS256, j.rsa, "rsa-1", unset("iss")), false},
		"control plane's audience":      {sign(t, jose.RS256, j.rsa, "rsa-1", set("aud", "spillway-controlplane")), false},
		"audience in a list":            {sign(t, jose.RS256, j.rsa, "rsa-1", set("aud", []string{"account", tokenAudience})), true},
		"no audience":                   {sign(t, jose.RS256, j.rsa, "rsa-1", unset("aud")), false},
		"no exp":                        {sign(t, jose.RS256, j.rsa, "rsa-1", unset("exp")), false},
		"expired 59 s ago":              {sign(t, jose.RS256, j.rsa, "rsa-1", set("exp", clock.unix(-59*time.Second))), true},
		"expired 60 s ago":              {sign(t, jose.RS256, j.rsa, "rsa-1", set("exp", clock.unix(-60*time.Second))), false},
		"expired 61 s ago":              {sign(t, jose.RS256, j.rsa, "rsa-1", set("exp", clock.unix(-61*time.Second))), false},
		"nbf 60 s ahead":                {sign(t, jose.RS256, j.rsa, "rsa-1", set("nbf", clock.unix(60*time.Second))), true},
		"nbf 61 s ahead":                {sign(t, jose.RS256, j.rsa, "rsa-1", set("nbf", clock.unix(61*time.Second))), false},
		"iat ahead (not checked)":       {sign(t, jose.RS256, j.rsa, "rsa-1", set("iat", clock.unix(time.Hour))), true},
		"no sub":                        {sign(t, jose.RS256, j.rsa, "rsa-1", unset("sub")), false},
		"unknown kid":                   {sign(t, jose.RS256, j.rsa, "rsa-9", clock.claims(aliceSubject)), false},
		"ES256 under the RSA key's kid": {sign(t, jose.ES256, j.ec, "rsa-1", clock.claims(aliceSubject)), false},
		"not a JWS":                     {"not-a-token", false},
	} {
		a := g.call(t, "/jwt/orders", name, http.Header{"Authorization": {"Bearer " + tt.token}})
		want := invalidToken
		if tt.ok {
			want = asConsumer
		}
		if got := a.outcome(); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

// TestTokenKeyRotation checks D8 on the gateway's clock: a token under a
// kid the gateway has not fetched is refused at once within 30 s of the
// last fetch, then one fetch, shared by concurrent requests, finds the
// issuer's new key; however many unknown kids follow, no more than one
// fetch runs per 30 s.
func TestTokenKeyRotation(t *testing.T) {
	j, clock := newJWKS(t), newTokenClock()
	g := newJWTGateway(t, j, clock, nil)
	g.run(t, j)
	if n := j.fetches.Load(); n != 1 {
		t.Fatalf("%d fetches at start, want 1", n)
	}
	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	j.set(func() {
		j.keys = append(j.keys, jose.JSONWebKey{Key: rotated.Public(), KeyID: "rsa-2", Algorithm: "RS256", Use: "sig"})
	})
	call := func(id, kid string) string {
		tok := sign(t, jose.RS256, rotated, kid, clock.claims(aliceSubject))
		return g.call(t, "/jwt/orders", id, http.Header{"Authorization": {"Bearer " + tok}}).outcome()
	}

	clock.add(29 * time.Second)
	if got := call("within-30s", "rsa-2"); got != invalidToken || j.fetches.Load() != 1 {
		t.Fatalf("new kid 29 s after the fetch: %s, %d fetches; want %s at once, no fetch", got, j.fetches.Load(), invalidToken)
	}
	clock.add(time.Second)
	var wg sync.WaitGroup
	outcomes := make([]string, 20)
	for i := range outcomes {
		wg.Go(func() { outcomes[i] = call("rotated-"+strconv.Itoa(i), "rsa-2") })
	}
	wg.Wait()
	for i, got := range outcomes {
		if got != asConsumer {
			t.Errorf("request %d after 30 s: %s, want %s", i, got, asConsumer)
		}
	}
	if n := j.fetches.Load(); n != 2 {
		t.Errorf("%d fetches after 30 s, want one more", n)
	}
	// 100 unknown kids over the next 29 s: no fetch; at 30 s, one.
	for i := range 100 {
		if got := call("unknown-"+strconv.Itoa(i), "kid-"+strconv.Itoa(i)); got != invalidToken {
			t.Fatalf("unknown kid %d: %s", i, got)
		}
		if i%10 == 9 {
			clock.add(2900 * time.Millisecond)
		}
	}
	if n := j.fetches.Load(); n != 2 {
		t.Errorf("%d fetches after 100 unknown kids within 29 s, want 2", n)
	}
	clock.add(time.Second)
	call("unknown-after-30s", "kid-x")
	if n := j.fetches.Load(); n != 3 {
		t.Errorf("%d fetches 30 s on, want 3", n)
	}
}

// TestTokenKeysOutage checks the JWKS outage rules (D8, ADR-0039): before
// any key set, a token is 503 token keys unavailable with Retry-After 1, a
// gateway answer with reason none, while keys and anonymous requests are
// served; once fetched, tokens verify; with the issuer down again, the
// last good keys keep working, and the failed fetch is logged without any
// token.
func TestTokenKeysOutage(t *testing.T) {
	j, clock := newJWKS(t), newTokenClock()
	j.status = http.StatusServiceUnavailable
	sr, tracer := newTracer(t, sdktrace.AlwaysSample())
	g := newJWTGateway(t, j, clock, tracer)
	g.run(t, j)
	token := sign(t, jose.RS256, j.rsa, "rsa-1", clock.claims(aliceSubject))
	tokenHeader := http.Header{"Authorization": {"Bearer " + token}}

	for _, path := range []string{"/jwt/orders", "/either/orders"} {
		if got := g.call(t, path, "cold"+path, tokenHeader).outcome(); got != keysUnavailable {
			t.Errorf("%s with no key set: %s, want %s", path, got, keysUnavailable)
		}
	}
	if got := g.call(t, "/jwt/orders", "cold-bad", http.Header{"Authorization": {"Bearer not-a-token"}}).outcome(); got != invalidToken {
		t.Errorf("a malformed token with no key set: %s, want %s", got, invalidToken)
	}
	for path, want := range map[string]string{"/either/orders": viaKey, "/api_key/orders": viaKey, "/none/orders": anonymous} {
		h := http.Header{"X-Api-Key": {g.key}}
		if want == anonymous {
			h = tokenHeader // not read on a none route
		}
		if got := g.call(t, path, "cold-other"+path, h).outcome(); got != want {
			t.Errorf("%s with no key set: %s, want %s", path, got, want)
		}
	}
	m := scrape(t, g.metrics)
	if s := `gw_requests_total{code_class="5xx",method="GET",origin="gateway",reason="none",route="tokens",tenant="none"} 1`; !strings.Contains(m, "\n"+s+"\n") {
		t.Errorf("/metrics has no %s", s)
	}

	// The issuer back: the next token 30 s on fetches the keys.
	j.set(func() { j.status = http.StatusOK })
	clock.add(30 * time.Second)
	if got := g.call(t, "/jwt/orders", "recovered", tokenHeader).outcome(); got != asConsumer {
		t.Fatalf("issuer back: %s, want %s", got, asConsumer)
	}
	// Down again: a fetch fails (an unknown kid 30 s on starts one), and
	// the last good keys keep verifying, with no age limit.
	j.set(func() { j.status = http.StatusInternalServerError })
	clock.add(30 * time.Second)
	before := j.fetches.Load()
	if got := g.call(t, "/jwt/orders", "down-unknown-kid", http.Header{"Authorization": {"Bearer " + sign(t, jose.RS256, j.rsa, "rsa-new", clock.claims(aliceSubject))}}).outcome(); got != invalidToken {
		t.Errorf("unknown kid with the issuer down: %s, want %s", got, invalidToken)
	}
	if j.fetches.Load() != before+1 {
		t.Errorf("%d fetches for the unknown kid, want 1", j.fetches.Load()-before)
	}
	clock.add(24 * time.Hour)
	fresh := sign(t, jose.RS256, j.rsa, "rsa-1", clock.claims(aliceSubject))
	if got := g.call(t, "/jwt/orders", "down-day-later", http.Header{"Authorization": {"Bearer " + fresh}}).outcome(); got != asConsumer {
		t.Errorf("a day into the outage: %s, want %s with the last good keys", got, asConsumer)
	}
	if !strings.Contains(g.keyLog.String(), "JWKS fetch failed") {
		t.Errorf("the failed fetch was not logged:\n%s", g.keyLog.String())
	}
	spans := spanText(sr)
	if !strings.Contains(spans, `"Value":"unavailable"`) {
		t.Error("no authn span with result unavailable")
	}
	noCredentials(t, map[string]string{
		"access log": g.logs.String(), "gateway log": g.events.String(), "key fetch log": g.keyLog.String(), "spans": spans,
	}, []string{token, fresh}, []string{g.key})
}
