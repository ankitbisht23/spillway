package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	issuer   = "http://localhost:8180/auth/realms/spillway"
	audience = "spillway-controlplane"
)

// issuerKeys is a test issuer: signing keys and the JWKS it serves.
type issuerKeys struct {
	t       *testing.T
	rsa     *rsa.PrivateKey
	ec      *ecdsa.PrivateKey
	mu      sync.Mutex
	jwks    []jose.JSONWebKey
	status  int
	body    []byte // served instead of jwks when set
	fetches atomic.Int32
	delay   time.Duration
}

func newIssuer(t *testing.T) *issuerKeys {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &issuerKeys{t: t, rsa: rk, ec: ek, status: 200, jwks: []jose.JSONWebKey{
		{Key: rk.Public(), KeyID: "rsa-1", Algorithm: "RS256", Use: "sig"},
		{Key: ek.Public(), KeyID: "ec-1"},
	}}
}

func (k *issuerKeys) set(f func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f()
}

func (k *issuerKeys) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	k.fetches.Add(1)
	k.mu.Lock()
	defer k.mu.Unlock()
	time.Sleep(k.delay)
	w.WriteHeader(k.status)
	if k.body != nil {
		_, _ = w.Write(k.body)
		return
	}
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: k.jwks})
}

// sign signs claims with key under kid; extra headers are added as given.
func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any, extra map[jose.HeaderKey]any) string {
	t.Helper()
	opts := &jose.SignerOptions{}
	if kid != "" {
		opts = opts.WithHeader("kid", kid)
	}
	for h, v := range extra {
		opts = opts.WithHeader(h, v)
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
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

// clock is a settable clock.
type clock struct{ now atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.now.Store(time.Now().UnixNano())
	return c
}

func (c *clock) Now() time.Time           { return time.Unix(0, c.now.Load()) }
func (c *clock) add(d time.Duration)      { c.now.Add(int64(d)) }
func (c *clock) unix(d time.Duration) int { return int(c.Now().Add(d).Unix()) }

func claims(c *clock) map[string]any {
	return map[string]any{
		"iss": issuer, "sub": "user-1", "aud": []string{"account", audience},
		"exp": c.unix(5 * time.Minute), "iat": c.unix(0), "preferred_username": "test-admin",
		"realm_access": map[string]any{"roles": []string{"admin"}},
	}
}

func newVerifier(t *testing.T, k *issuerKeys, c *clock) *Verifier {
	t.Helper()
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	v, err := New(Config{Issuer: issuer, JWKSURL: srv.URL, Audience: audience, AllowHTTP: true, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// started runs v and waits for its first fetch.
func started(t *testing.T, v *Verifier) *Verifier {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go v.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !v.Ready() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !v.Ready() {
		t.Fatal("no key set after 5 s")
	}
	return v
}

func TestVerifyAccepts(t *testing.T) {
	k, c := newIssuer(t), newClock()
	v := started(t, newVerifier(t, k, c))
	for name, tok := range map[string]string{
		"RS256": sign(t, jose.RS256, k.rsa, "rsa-1", claims(c), nil),
		"ES256": sign(t, jose.ES256, k.ec, "ec-1", claims(c), nil),
	} {
		got, err := v.Verify(t.Context(), tok)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var extra struct {
			Name  string `json:"preferred_username"`
			Realm struct {
				Roles []string `json:"roles"`
			} `json:"realm_access"`
		}
		if err := got.Decode(&extra); err != nil || got.Subject != "user-1" || extra.Name != "test-admin" || extra.Realm.Roles[0] != "admin" {
			t.Errorf("%s: claims %+v %+v %v", name, got, extra, err)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	k, c := newIssuer(t), newClock()
	small, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // G403: the small key the verifier must refuse
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	k.jwks = append(k.jwks,
		jose.JSONWebKey{Key: small.Public(), KeyID: "small"},
		jose.JSONWebKey{Key: p384.Public(), KeyID: "p384"},
		jose.JSONWebKey{Key: encKey.Public(), KeyID: "enc", Use: "enc"},
		jose.JSONWebKey{Key: encKey.Public(), KeyID: "alg-mismatch", Algorithm: "PS256"},
	)
	v := started(t, newVerifier(t, k, c))
	with := func(f func(m map[string]any)) map[string]any {
		m := claims(c)
		f(m)
		return m
	}
	b64 := base64.RawURLEncoding.EncodeToString
	payload := b64([]byte(`{"iss":"` + issuer + `","aud":"` + audience + `","exp":9999999999}`))
	good := sign(t, jose.RS256, k.rsa, "rsa-1", claims(c), nil)
	parts := strings.Split(good, ".")
	tests := map[string]string{
		"alg none":                 b64([]byte(`{"alg":"none","kid":"rsa-1"}`)) + "." + payload + ".",
		"HS256":                    sign(t, jose.HS256, []byte("dev-only-not-secret-dev-only-not"), "rsa-1", claims(c), nil),
		"PS256":                    sign(t, jose.PS256, k.rsa, "rsa-1", claims(c), nil),
		"no kid":                   sign(t, jose.RS256, k.rsa, "", claims(c), nil),
		"crit header":              sign(t, jose.RS256, k.rsa, "rsa-1", claims(c), map[jose.HeaderKey]any{"crit": []string{"exp"}, "exp": 1}),
		"unknown kid":              sign(t, jose.RS256, k.rsa, "rsa-2", claims(c), nil),
		"ES256 header, RSA key":    sign(t, jose.ES256, k.ec, "rsa-1", claims(c), nil),
		"RSA under 2048 bits":      sign(t, jose.RS256, small, "small", claims(c), nil),
		"EC P-384":                 sign(t, jose.ES384, p384, "p384", claims(c), nil),
		"ES256 on a P-384 key":     sign(t, jose.ES256, k.ec, "p384", claims(c), nil),
		"use enc":                  sign(t, jose.RS256, encKey, "enc", claims(c), nil),
		"key alg PS256":            sign(t, jose.RS256, encKey, "alg-mismatch", claims(c), nil),
		"signed by another key":    sign(t, jose.RS256, other, "rsa-1", claims(c), nil),
		"embedded jwk":             sign(t, jose.RS256, other, "rsa-1", claims(c), map[jose.HeaderKey]any{"jwk": jose.JSONWebKey{Key: other.Public()}}),
		"jku":                      sign(t, jose.RS256, other, "rsa-1", claims(c), map[jose.HeaderKey]any{"jku": "http://attacker/jwks"}),
		"tampered payload":         parts[0] + "." + payload + "." + parts[2],
		"not three parts":          parts[0] + "." + parts[1],
		"garbage":                  "not-a-token",
		"wrong issuer":             sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["iss"] = "http://keycloak:8080/auth/realms/spillway" }), nil),
		"issuer with a slash":      sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["iss"] = issuer + "/" }), nil),
		"no audience":              sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { delete(m, "aud") }), nil),
		"other audience":           sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["aud"] = "spillway-gateway" }), nil),
		"no sub":                   sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { delete(m, "sub") }), nil),
		"empty sub":                sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["sub"] = "" }), nil),
		"no exp":                   sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { delete(m, "exp") }), nil),
		"expired past the leeway":  sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["exp"] = c.unix(-61 * time.Second) }), nil),
		"nbf past the leeway":      sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["nbf"] = c.unix(62 * time.Second) }), nil),
		"exp not a number":         sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["exp"] = "soon" }), nil),
		"JSON serialization":       `{"payload":"` + parts[1] + `","protected":"` + parts[0] + `","signature":"` + parts[2] + `"}`,
		"audience a number":        sign(t, jose.RS256, k.rsa, "rsa-1", with(func(m map[string]any) { m["aud"] = 7 }), nil),
		"claims not a JSON object": sign(t, jose.RS256, k.rsa, "rsa-1", nil, nil),
	}
	for name, tok := range tests {
		if _, err := v.Verify(t.Context(), tok); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	for name, m := range map[string]map[string]any{
		"expired within the leeway": with(func(m map[string]any) { m["exp"] = c.unix(-30 * time.Second) }),
		"nbf within the leeway":     with(func(m map[string]any) { m["nbf"] = c.unix(30 * time.Second) }),
		"audience a string":         with(func(m map[string]any) { m["aud"] = audience }),
		"iat in the future":         with(func(m map[string]any) { m["iat"] = c.unix(time.Hour) }),
	} {
		if _, err := v.Verify(t.Context(), sign(t, jose.RS256, k.rsa, "rsa-1", m, nil)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestUnknownKidRefresh checks D8: an unknown kid fetches the key set at
// most once per 30 s, concurrent requests share one fetch, and a key the
// issuer adds is found by that fetch.
func TestUnknownKidRefresh(t *testing.T) {
	k, c := newIssuer(t), newClock()
	v := started(t, newVerifier(t, k, c))
	if n := k.fetches.Load(); n != 1 {
		t.Fatalf("%d fetches at start", n)
	}
	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok := sign(t, jose.RS256, rotated, "rsa-2", claims(c), nil)

	// Within 30 s of the start's fetch: refused at once, no fetch.
	c.add(10 * time.Second)
	k.set(func() { k.jwks = append(k.jwks, jose.JSONWebKey{Key: rotated.Public(), KeyID: "rsa-2"}) })
	if _, err := v.Verify(t.Context(), tok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kid within 30 s: %v", err)
	}
	if n := k.fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want no fetch within 30 s", n)
	}

	// 30 s on: one fetch, shared by concurrent requests, finds the key.
	c.add(20 * time.Second)
	k.set(func() { k.delay = 100 * time.Millisecond })
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 20 {
		wg.Go(func() {
			if _, err := v.Verify(context.Background(), tok); err != nil {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if n := k.fetches.Load(); n != 2 || failed.Load() != 0 {
		t.Errorf("%d fetches, %d failed; want one more fetch and none failed", n, failed.Load())
	}

	// Bad kids right after: no fetch.
	for range 10 {
		if _, err := v.Verify(t.Context(), sign(t, jose.RS256, rotated, "nope", claims(c), nil)); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if n := k.fetches.Load(); n != 2 {
		t.Errorf("%d fetches after bad kids within 30 s", n)
	}
}

// TestKeysUnavailable checks the cold start: with no key set ever fetched
// a token is ErrUnavailable (503), and the next unknown-kid fetch after 30 s
// recovers.
func TestKeysUnavailable(t *testing.T) {
	k, c := newIssuer(t), newClock()
	k.status = 500
	v := newVerifier(t, k, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v.Run(ctx)
	tok := sign(t, jose.RS256, k.rsa, "rsa-1", claims(c), nil)
	deadline := time.Now().Add(5 * time.Second)
	for k.fetches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := v.Verify(t.Context(), tok); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no keys: %v, want ErrUnavailable", err)
	}
	if _, err := v.Verify(t.Context(), "garbage"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a malformed token is invalid whatever the keys: %v", err)
	}
	k.set(func() { k.status = 200 })
	c.add(RetryInterval)
	if _, err := v.Verify(t.Context(), tok); err != nil {
		t.Errorf("after the issuer recovers: %v", err)
	}
}

// TestLastGoodKeysKept checks that failed fetches keep the last good key
// set, with no age limit (D8).
func TestLastGoodKeysKept(t *testing.T) {
	k, c := newIssuer(t), newClock()
	v := started(t, newVerifier(t, k, c))
	for name, broken := range map[string]func(){
		"500":           func() { k.status, k.body = 500, nil },
		"not JSON":      func() { k.status, k.body = 200, []byte("<html>") },
		"no usable key": func() { k.status, k.body = 200, []byte(`{"keys":[{"kty":"oct","k":"AAAA","kid":"x"}]}`) },
		"over 1 MiB":    func() { k.status, k.body = 200, []byte(`{"keys":[],"pad":"`+strings.Repeat("x", MaxJWKSBytes)+`"}`) },
	} {
		k.set(broken)
		<-v.fetch()
		if !v.failedLast() {
			t.Errorf("%s: the fetch did not fail", name)
		}
		c.add(time.Hour)
		if _, err := v.Verify(t.Context(), sign(t, jose.RS256, k.rsa, "rsa-1", claims(c), nil)); err != nil {
			t.Errorf("%s: %v, want the last good keys", name, err)
		}
	}
}

func TestNew(t *testing.T) {
	for _, tt := range []struct {
		cfg  Config
		want string
	}{
		{Config{Issuer: "http://kc/realms/x", JWKSURL: "https://kc/certs", Audience: "a"}, "issuer: an http:// URL is accepted only in dev mode"},
		{Config{Issuer: "https://kc/realms/x", JWKSURL: "http://kc/certs", Audience: "a"}, "JWKS URL: an http:// URL is accepted only in dev mode"},
		{Config{Issuer: "", JWKSURL: "https://kc/certs", Audience: "a"}, "issuer: required"},
		{Config{Issuer: "https://kc/realms/x", JWKSURL: "/certs", Audience: "a"}, "JWKS URL: want an absolute"},
		{Config{Issuer: "https://kc/realms/x", JWKSURL: "ftp://kc/certs", Audience: "a"}, "JWKS URL: want an absolute"},
		{Config{Issuer: "https://kc/realms/x", JWKSURL: "https://kc/certs"}, "audience: required"},
	} {
		if _, err := New(tt.cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("New(%+v) = %v, want %q", tt.cfg, err, tt.want)
		}
	}
	if _, err := New(Config{Issuer: "http://kc/realms/x", JWKSURL: "http://kc/certs", Audience: "a", AllowHTTP: true}); err != nil {
		t.Errorf("dev mode: %v", err)
	}
}

// TestKeysFromTheConfiguredURLOnly checks SEC-04's "from the configured
// JWKS URL only": a redirect, even to a good key set, is a failed fetch;
// and NewClient with roots fetches over TLS from a server those roots
// trust.
func TestKeysFromTheConfiguredURLOnly(t *testing.T) {
	k, c := newIssuer(t), newClock()
	good := httptest.NewServer(k)
	t.Cleanup(good.Close)
	redirect := httptest.NewServer(http.RedirectHandler(good.URL, http.StatusFound))
	t.Cleanup(redirect.Close)
	v, err := New(Config{Issuer: issuer, JWKSURL: redirect.URL, Audience: audience, AllowHTTP: true, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	<-v.fetch()
	if v.Ready() || k.fetches.Load() != 0 {
		t.Errorf("a redirect was followed: ready %v, %d fetches of the target", v.Ready(), k.fetches.Load())
	}

	tlsSrv := httptest.NewUnstartedServer(k)
	tlsSrv.Config.ErrorLog = log.New(io.Discard, "", 0) // the untrusted fetch's handshake error
	tlsSrv.StartTLS()
	t.Cleanup(tlsSrv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(tlsSrv.Certificate())
	v, err = New(Config{Issuer: "https://kc.test/realms/x", JWKSURL: tlsSrv.URL, Audience: audience, Client: NewClient(roots), Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if <-v.fetch(); !v.Ready() {
		t.Error("no key set over TLS with the server's root")
	}
	v, err = New(Config{Issuer: "https://kc.test/realms/x", JWKSURL: tlsSrv.URL, Audience: audience, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if <-v.fetch(); v.Ready() {
		t.Error("a key set from a server the system roots do not trust")
	}
}
