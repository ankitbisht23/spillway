// Package auth verifies bearer tokens, compact JWS JWTs signed with RS256 or
// ES256, against the keys an issuer publishes at its JWKS URL (GW-18, SEC-04,
// ADR-0039). The control plane uses it for its REST API's bearer tokens
// (CP-11, ADR-0040); the gateway will use it for JWT routes.
//
// Keys come only from the configured JWKS URL, never from a token's own
// jku, x5u or jwk headers. The key set is fetched in the background and
// every 5 minutes; a token with an unknown kid triggers at most one extra
// fetch per 30 s. A failed fetch keeps the last good keys with no age limit
// (D8); while no key set was ever fetched, Verify answers ErrUnavailable.
package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// The rules of ADR-0039 (D8).
const (
	// RefreshInterval is how often the key set is fetched.
	RefreshInterval = 5 * time.Minute
	// RetryInterval is how soon a failed fetch is retried, and the least
	// time between two fetches that unknown kids trigger.
	RetryInterval = 30 * time.Second
	// UnknownKidWait is how long a request with an unknown kid waits for
	// the fetch it triggered.
	UnknownKidWait = 2 * time.Second
	// FetchTimeout bounds one fetch.
	FetchTimeout = 5 * time.Second
	// MaxJWKSBytes is the largest key set accepted.
	MaxJWKSBytes = 1 << 20
	// Leeway is the clock skew allowed on exp and nbf.
	Leeway = 60 * time.Second
	// failureLogInterval is the least time between two logged fetch failures.
	failureLogInterval = time.Minute
)

var (
	// ErrInvalid means the token failed a check. Its message carries no
	// detail about the token.
	ErrInvalid = errors.New("invalid token")
	// ErrUnavailable means no key set was ever fetched, so no token can be
	// checked: the client should try again (503, not 401).
	ErrUnavailable = errors.New("token keys unavailable")
)

// algorithms are the only ones accepted: never none, never HS256 (SEC-04).
var algorithms = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// Config is a verifier's issuer and key source.
type Config struct {
	Issuer    string // the iss every token must carry exactly (the browser-facing issuer)
	JWKSURL   string // where the keys are fetched
	Audience  string // a value aud must contain
	AllowHTTP bool   // accept http:// URLs: dev mode only (SEC-04)
	Client    *http.Client
	Log       *slog.Logger
	Now       func() time.Time // the clock; time.Now when nil
}

// Claims are a verified token's claims.
type Claims struct {
	Subject string
	payload []byte
}

// Decode decodes the token's claims into v, as encoding/json does.
func (c *Claims) Decode(v any) error { return json.Unmarshal(c.payload, v) }

// Verifier checks tokens against one issuer's keys.
type Verifier struct {
	cfg  Config
	keys atomic.Pointer[keySet] // nil until a fetch succeeds

	mu        sync.Mutex
	lastStart time.Time     // when the last fetch started
	inflight  chan struct{} // closed when the fetch in progress ends; nil when none is
	failed    bool          // whether the last fetch failed
	lastLog   time.Time     // when a fetch failure was last logged
}

// keySet is the usable keys of a JWKS by kid.
type keySet map[string][]jose.JSONWebKey

// New returns a verifier for cfg. Call Run to fetch keys.
func New(cfg Config) (*Verifier, error) {
	for name, u := range map[string]string{"issuer": cfg.Issuer, "JWKS URL": cfg.JWKSURL} {
		if err := CheckURL(u, cfg.AllowHTTP); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if cfg.Audience == "" {
		return nil, errors.New("audience: required")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: FetchTimeout}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Verifier{cfg: cfg}, nil
}

// CheckURL accepts an absolute https:// URL, or an http:// one when
// allowHTTP is set (SEC-04).
func CheckURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	switch {
	case raw == "":
		return errors.New("required")
	case err != nil || u.Host == "":
		return errors.New("want an absolute https:// URL")
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && allowHTTP:
		return nil
	case u.Scheme == "http":
		return errors.New("an http:// URL is accepted only in dev mode (SEC-04)")
	}
	return errors.New("want an absolute https:// URL")
}

// Run fetches the key set at once and every RefreshInterval after, a
// failed fetch again after RetryInterval, until ctx is done.
func (v *Verifier) Run(ctx context.Context) {
	for {
		wait := RefreshInterval
		select {
		case <-v.fetch():
		case <-ctx.Done():
			return
		}
		if v.failedLast() {
			wait = RetryInterval
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// Ready reports whether a key set was ever fetched.
func (v *Verifier) Ready() bool { return v.keys.Load() != nil }

// Verify checks a compact JWS token (ADR-0039): RS256 or ES256 with a kid
// and no crit header, signed by a fitting key of the JWKS with that kid, iss
// the issuer exactly, aud containing the audience, exp present and not
// passed, nbf not in the future (both with Leeway). iat is not checked.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	sig, kid, alg, err := parse(token)
	if err != nil {
		return nil, ErrInvalid
	}
	keys := v.keys.Load()
	if keys == nil || (*keys)[kid] == nil {
		if keys, err = v.refreshFor(ctx); err != nil {
			return nil, err
		}
	}
	key, ok := pick((*keys)[kid], alg)
	if !ok {
		return nil, ErrInvalid
	}
	payload, err := sig.Verify(key.Key)
	if err != nil {
		return nil, ErrInvalid
	}
	return v.claims(payload)
}

// parse parses a compact JWS with an allowed algorithm and returns its kid
// and algorithm. A crit header is refused, whatever it names.
func parse(token string) (*jose.JSONWebSignature, string, string, error) {
	head, _, ok := strings.Cut(token, ".")
	if !ok {
		return nil, "", "", ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return nil, "", "", ErrInvalid
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, "", "", ErrInvalid
	}
	if _, ok := h["crit"]; ok {
		return nil, "", "", ErrInvalid
	}
	sig, err := jose.ParseSignedCompact(token, algorithms)
	if err != nil || len(sig.Signatures) != 1 {
		return nil, "", "", ErrInvalid
	}
	hdr := sig.Signatures[0].Protected
	if hdr.KeyID == "" {
		return nil, "", "", ErrInvalid
	}
	return sig, hdr.KeyID, hdr.Algorithm, nil
}

// pick returns the first key fit for alg.
func pick(keys []jose.JSONWebKey, alg string) (jose.JSONWebKey, bool) {
	for _, k := range keys {
		if (k.Algorithm == "" || k.Algorithm == alg) && fits(k, alg) {
			return k, true
		}
	}
	return jose.JSONWebKey{}, false
}

// fits reports whether k's type suits alg: RSA of at least 2,048 bits for
// RS256, EC P-256 for ES256.
func fits(k jose.JSONWebKey, alg string) bool {
	switch pub := k.Key.(type) {
	case *rsa.PublicKey:
		return alg == string(jose.RS256) && pub.N.BitLen() >= 2048
	case *ecdsa.PublicKey:
		return alg == string(jose.ES256) && pub.Curve == elliptic.P256()
	}
	return false
}

// registered are the registered claims Verify checks.
type registered struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	Expiry    *json.Number    `json:"exp"`
	NotBefore *json.Number    `json:"nbf"`
}

func (v *Verifier) claims(payload []byte) (*Claims, error) {
	var c registered
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return nil, ErrInvalid
	}
	now := v.cfg.Now()
	switch {
	case c.Issuer != v.cfg.Issuer:
		return nil, ErrInvalid
	case !hasAudience(c.Audience, v.cfg.Audience):
		return nil, ErrInvalid
	case c.Expiry == nil:
		return nil, ErrInvalid
	}
	exp, err := unixTime(*c.Expiry)
	if err != nil || !now.Before(exp.Add(Leeway)) {
		return nil, ErrInvalid
	}
	if c.NotBefore != nil {
		nbf, err := unixTime(*c.NotBefore)
		if err != nil || nbf.After(now.Add(Leeway)) {
			return nil, ErrInvalid
		}
	}
	return &Claims{Subject: c.Subject, payload: payload}, nil
}

// hasAudience reports whether aud, a string or an array of strings,
// contains want.
func hasAudience(aud json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(aud, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(aud, &many) != nil {
		return false
	}
	for _, a := range many {
		if a == want {
			return true
		}
	}
	return false
}

func unixTime(n json.Number) (time.Time, error) {
	f, err := n.Float64()
	if err != nil {
		return time.Time{}, err
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), nil
}

// Fetching

// refreshFor runs for a token whose kid is unknown: if the last fetch
// started at least RetryInterval ago it starts one (or joins the one in
// progress) and waits for it up to UnknownKidWait; within RetryInterval of
// the last fetch it answers at once. It returns the key set to look in.
func (v *Verifier) refreshFor(ctx context.Context) (*keySet, error) {
	v.mu.Lock()
	var done <-chan struct{}
	switch {
	case v.inflight != nil:
		done = v.inflight
	case v.cfg.Now().Sub(v.lastStart) >= RetryInterval:
		done = v.startLocked()
	}
	v.mu.Unlock()
	if done != nil {
		t := time.NewTimer(UnknownKidWait)
		select {
		case <-done:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	keys := v.keys.Load()
	if keys == nil {
		return nil, ErrUnavailable
	}
	return keys, nil
}

// fetch starts a fetch, or joins the one in progress, and returns a channel
// closed when it ends.
func (v *Verifier) fetch() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.inflight != nil {
		return v.inflight
	}
	return v.startLocked()
}

// failedLast reports whether the last fetch failed.
func (v *Verifier) failedLast() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.failed
}

func (v *Verifier) startLocked() chan struct{} {
	done := make(chan struct{})
	v.inflight = done
	v.lastStart = v.cfg.Now()
	go func() {
		keys, err := v.get()
		if err == nil {
			v.keys.Store(&keys)
		}
		v.mu.Lock()
		v.failed = err != nil
		v.inflight = nil
		logIt := err != nil && v.cfg.Now().Sub(v.lastLog) >= failureLogInterval
		if logIt {
			v.lastLog = v.cfg.Now()
		}
		v.mu.Unlock()
		close(done)
		if logIt {
			v.cfg.Log.Warn("JWKS fetch failed; keeping the last good keys", "jwks_url", v.cfg.JWKSURL, "have_keys", v.keys.Load() != nil, "err", err)
		}
	}()
	return done
}

// get fetches and parses the key set: status 200, at most MaxJWKSBytes of
// JSON, unusable keys dropped; a set with no usable key is an error.
func (v *Verifier) get() (keySet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.cfg.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxJWKSBytes {
		return nil, fmt.Errorf("the key set is over %d bytes", MaxJWKSBytes)
	}
	return parseKeySet(body)
}

func parseKeySet(body []byte) (keySet, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("the key set is not JSON: %w", err)
	}
	keys := keySet{}
	for _, raw := range set.Keys {
		var k jose.JSONWebKey
		if k.UnmarshalJSON(raw) != nil || k.KeyID == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		if !k.IsPublic() {
			k = k.Public()
		}
		usable := false
		for _, alg := range algorithms {
			usable = usable || (k.Algorithm == "" || k.Algorithm == string(alg)) && fits(k, string(alg))
		}
		if usable {
			keys[k.KeyID] = append(keys[k.KeyID], k)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("the key set has no usable signing key")
	}
	return keys, nil
}
