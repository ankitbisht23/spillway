package demo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ankitbisht23/spillway/internal/secret"
)

// The public demo's Keycloak side (SC-09, ADR-0097 §3, §4): the admin API
// calls of make demo-share, the OIDC grants make demo-share-test checks
// with, and a browser's login through the demo proxy. No password, client
// secret, code or token ever appears in an error or in output.

// Realm is the demo's realm.
const Realm = "spillway"

// adminTokenMaxAge: an admin token older than this is replaced; the master
// realm's tokens live 60 s.
const adminTokenMaxAge = 30 * time.Second

// Keycloak calls the admin REST API of realm spillway as the master realm's
// admin, logged in through admin-cli with a password grant.
type Keycloak struct {
	base, user string // base is Keycloak's root with its path, such as http://keycloak:8080/auth
	password   secret.Value
	http       *http.Client
	timeout    time.Duration

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

// NewKeycloak returns an admin API client of the Keycloak at base for the
// master realm's user with password; timeout bounds each call.
func NewKeycloak(base, user string, password secret.Value, timeout time.Duration) *Keycloak {
	return &Keycloak{base: strings.TrimSuffix(base, "/"), user: user, password: password, timeout: timeout,
		http: &http.Client{CheckRedirect: noRedirect}}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Grant is a token endpoint's answer: the status and, on success, the
// access token (never printed).
type Grant struct {
	Status int
	Token  string
	Error  string
}

// TokenGrant posts form to realm's token endpoint under base and returns
// the answer; err is a transport error only.
func TokenGrant(ctx context.Context, hc *http.Client, base, realm string, form url.Values) (Grant, error) {
	u := strings.TrimSuffix(base, "/") + "/realms/" + realm + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode())) //nolint:gosec // G704: Keycloak in the compose network is the point
	if err != nil {
		return Grant{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return Grant{}, fmt.Errorf("the token endpoint of realm %s: transport error or timeout", realm)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	g := Grant{Status: resp.StatusCode, Error: out.Error}
	if resp.StatusCode == http.StatusOK {
		g.Token = out.AccessToken
	}
	return g, nil
}

// adminToken returns a master-realm admin token no older than
// adminTokenMaxAge.
func (k *Keycloak) adminToken(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Since(k.tokenAt) < adminTokenMaxAge {
		return k.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()
	g, err := TokenGrant(ctx, k.http, k.base, "master", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {k.user}, "password": {k.password.Reveal()},
	})
	if err != nil {
		return "", err
	}
	if g.Token == "" {
		return "", fmt.Errorf("the Keycloak admin login: status %d %s", g.Status, g.Error)
	}
	k.token, k.tokenAt = g.Token, time.Now()
	return k.token, nil
}

// call sends method to the admin API path under /admin/realms/spillway
// with body as JSON (nil for none) and decodes a 2xx answer into out
// unless nil. Any other status is an error naming the call.
func (k *Keycloak) call(ctx context.Context, method, path string, body, out any) error {
	token, err := k.adminToken(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, k.base+"/admin/realms/"+Realm+path, rd) //nolint:gosec // G704: as above
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return fmt.Errorf("keycloak %s %s: transport error or timeout", method, path)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("keycloak %s %s: status %d", method, path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("keycloak %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Rep is a Keycloak representation, kept whole so that an update sends
// back every field it read.
type Rep map[string]any

func (r Rep) str(k string) string {
	s, _ := r[k].(string)
	return s
}

func (r Rep) strs(k string) []string {
	var out []string
	if l, ok := r[k].([]any); ok {
		for _, v := range l {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// Enabled is the representation's enabled field.
func (r Rep) Enabled() bool {
	b, _ := r["enabled"].(bool)
	return b
}

// one looks up exactly one representation at path (a search).
func (k *Keycloak) one(ctx context.Context, path, what string) (Rep, error) {
	var l []Rep
	if err := k.call(ctx, http.MethodGet, path, nil, &l); err != nil {
		return nil, err
	}
	if len(l) != 1 {
		return nil, fmt.Errorf("%s: %d found, want 1", what, len(l))
	}
	return l[0], nil
}

// Client is the client clientID's representation.
func (k *Keycloak) Client(ctx context.Context, clientID string) (Rep, error) {
	return k.one(ctx, "/clients?clientId="+url.QueryEscape(clientID), "client "+clientID)
}

// User is the user username's representation.
func (k *Keycloak) User(ctx context.Context, username string) (Rep, error) {
	return k.one(ctx, "/users?exact=true&username="+url.QueryEscape(username), "user "+username)
}

// Realm is realm spillway's representation.
func (k *Keycloak) Realm(ctx context.Context) (Rep, error) {
	var r Rep
	return r, k.call(ctx, http.MethodGet, "", nil, &r)
}

// RealmRoles are the names of the realm roles mapped to the user with ID
// id (directly, not through groups or composites).
func (k *Keycloak) RealmRoles(ctx context.Context, id string) ([]string, error) {
	var l []Rep
	if err := k.call(ctx, http.MethodGet, "/users/"+url.PathEscape(id)+"/role-mappings/realm", nil, &l); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range l {
		out = append(out, r.str("name"))
	}
	return out, nil
}

// ID is the representation's id.
func (r Rep) ID() string { return r.str("id") }

// IdentityProvider is the identity provider alias's representation; nil
// when there is none.
func (k *Keycloak) IdentityProvider(ctx context.Context, alias string) (Rep, error) {
	var l []Rep
	if err := k.call(ctx, http.MethodGet, "/identity-provider/instances", nil, &l); err != nil {
		return nil, err
	}
	for _, p := range l {
		if p.str("alias") == alias {
			return p, nil
		}
	}
	return nil, nil
}

// PostLogoutURIs is admin-ui's post.logout.redirect.uris attribute, split
// at Keycloak's "##".
func PostLogoutURIs(c Rep) []string {
	attrs, _ := c["attributes"].(map[string]any)
	s, _ := attrs["post.logout.redirect.uris"].(string)
	var out []string
	for u := range strings.SplitSeq(s, "##") {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// RedirectURIs and WebOrigins are a client's lists.
func RedirectURIs(c Rep) []string { return c.strs("redirectUris") }

// WebOrigins is the client's web origins.
func WebOrigins(c Rep) []string { return c.strs("webOrigins") }

// GitHub is the GitHub identity provider's settings (named tunnel only,
// ADR-0097 §3 step 5).
type GitHub struct {
	ClientID     string
	ClientSecret secret.Value
}

// ShareConfig is what share-config changes (ADR-0097 §3 step 5).
type ShareConfig struct {
	URL           string       // the public URL U: https, a host, no path
	AdminPassword secret.Value // test-admin's new password, DEMO_ADMIN_PASSWORD
	GitHub        *GitHub      // nil: no GitHub login
	// WriteSecret keeps a client's new secret (in a 0600 file); it never
	// prints it.
	WriteSecret func(client string, v secret.Value) error
}

// shareDisabledUsers are the realm's test users a share disables; it
// keeps test-viewer, the visitors' account, and test-admin, the owner's.
var shareDisabledUsers = []string{"test-editor", "test-publisher", "test-key-admin"}

// shareRotatedClients get new secrets for a share; rbac-tests is disabled.
var shareRotatedClients = []string{"spillway-ci", "gateway-tests"}

// devValue is the repository's public dev password and secret.
const devValue = "dev-only-not-secret" //nolint:gosec // G101: the public dev value a share must not accept

// CheckAdminPassword is DEMO_ADMIN_PASSWORD's rule (ADR-0097 §3 step 1):
// at least 16 characters, not the dev value.
func CheckAdminPassword(name string, v secret.Value) error {
	switch {
	case !v.IsSet():
		return fmt.Errorf("%s is not set", name)
	case len(v.Reveal()) < 16:
		return fmt.Errorf("%s is shorter than 16 characters", name)
	case v.Reveal() == devValue:
		return fmt.Errorf("%s is the public dev value", name)
	}
	return nil
}

// CheckPublicURL checks a share's URL: https, a host, no user info, path,
// query or fragment (a trailing / is dropped). It returns the URL as used.
func CheckPublicURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("the share URL %q: want https://<host> with no path", raw)
	}
	return "https://" + u.Host, nil
}

// Share applies cfg to realm spillway, printing one line per change to out
// (URLs and names only).
func (k *Keycloak) Share(ctx context.Context, cfg ShareConfig, out io.Writer) error {
	u, err := CheckPublicURL(cfg.URL)
	if err != nil {
		return err
	}
	if err := CheckAdminPassword("DEMO_ADMIN_PASSWORD", cfg.AdminPassword); err != nil {
		return err
	}
	// admin-ui: the public callback, post-logout URI and origin beside the
	// localhost ones.
	c, err := k.Client(ctx, "admin-ui")
	if err != nil {
		return err
	}
	c["redirectUris"] = addTo(RedirectURIs(c), u+"/oidc/callback")
	c["webOrigins"] = addTo(WebOrigins(c), u)
	attrs, _ := c["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs["post.logout.redirect.uris"] = strings.Join(addTo(PostLogoutURIs(c), u+"/signed-out"), "##")
	c["attributes"] = attrs
	if err := k.call(ctx, http.MethodPut, "/clients/"+url.PathEscape(c.str("id")), c, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "share-config: admin-ui: redirect URI %s/oidc/callback, post-logout redirect URI %s/signed-out and web origin %s added\n", u, u, u)

	// Users: only test-viewer (visitors) and test-admin (the owner, with
	// DEMO_ADMIN_PASSWORD) stay usable.
	for _, name := range shareDisabledUsers {
		usr, err := k.User(ctx, name)
		if err != nil {
			return err
		}
		if err := k.call(ctx, http.MethodPut, "/users/"+url.PathEscape(usr.str("id")), map[string]any{"enabled": false}, nil); err != nil {
			return err
		}
	}
	admin, err := k.User(ctx, "test-admin")
	if err != nil {
		return err
	}
	if err := k.call(ctx, http.MethodPut, "/users/"+url.PathEscape(admin.str("id"))+"/reset-password",
		map[string]any{"type": "password", "value": cfg.AdminPassword.Reveal(), "temporary": false}, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "share-config: users %s disabled; test-admin's password set from DEMO_ADMIN_PASSWORD; test-viewer kept for visitors\n", strings.Join(shareDisabledUsers, ", "))

	// Clients: new secrets for the service accounts, kept in files; no
	// password grants through rbac-tests.
	for _, name := range shareRotatedClients {
		cl, err := k.Client(ctx, name)
		if err != nil {
			return err
		}
		var s struct {
			Value string `json:"value"`
		}
		if err := k.call(ctx, http.MethodPost, "/clients/"+url.PathEscape(cl.str("id"))+"/client-secret", nil, &s); err != nil {
			return err
		}
		if s.Value == "" {
			return fmt.Errorf("client %s: Keycloak answered no new secret", name)
		}
		if err := cfg.WriteSecret(name, secret.New(s.Value)); err != nil {
			return fmt.Errorf("client %s: keeping its new secret: %w", name, err)
		}
	}
	rbac, err := k.Client(ctx, "rbac-tests")
	if err != nil {
		return err
	}
	if err := k.call(ctx, http.MethodPut, "/clients/"+url.PathEscape(rbac.str("id")), map[string]any{"enabled": false}, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "share-config: clients %s: new secrets kept in files (not printed); rbac-tests disabled\n", strings.Join(shareRotatedClients, " and "))

	// Realm: brute-force detection with Keycloak's defaults, since
	// admin-cli accepts password grants.
	if err := k.call(ctx, http.MethodPut, "", map[string]any{"bruteForceProtected": true}, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "share-config: realm spillway: brute-force detection on")

	if cfg.GitHub == nil {
		_, _ = fmt.Fprintln(out, "share-config: no GitHub login (it needs a named tunnel and GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET)")
		return nil
	}
	// GitHub visitors are viewers; the owner's admin login is test-admin.
	if err := k.call(ctx, http.MethodPost, "/identity-provider/instances", map[string]any{
		"alias": "github", "providerId": "github", "enabled": true, "trustEmail": false,
		"config": map[string]any{"clientId": cfg.GitHub.ClientID, "clientSecret": cfg.GitHub.ClientSecret.Reveal(), "syncMode": "IMPORT"},
	}, nil); err != nil {
		return err
	}
	if err := k.call(ctx, http.MethodPost, "/identity-provider/instances/github/mappers", map[string]any{
		"name": "viewer", "identityProviderAlias": "github", "identityProviderMapper": "oidc-hardcoded-role-idp-mapper",
		"config": map[string]any{"role": "viewer", "syncMode": "INHERIT"},
	}, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "share-config: identity provider github added: GitHub visitors get the viewer role")
	return nil
}

func addTo(l []string, v string) []string {
	if slices.Contains(l, v) {
		return l
	}
	return append(l, v)
}

// BrowserLogin is a browser's login to admin-ui through the demo proxy
// (ADR-0097 §4): the authorization request with publicURL's callback and
// PKCE, the login form posted with user's password, the redirect, and the
// code exchanged for a token. Requests go to proxy (the demo proxy inside
// the network) wherever Keycloak's pages point at publicURL; header pairs
// are added to every request.
type BrowserLogin struct {
	FormAction string // where the login form posts, as Keycloak wrote it
	Location   string // the redirect after the login, without its query
	Token      string // the access token (never printed)
}

var formAction = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*>|<form[^>]*action="[^"]*login-actions/authenticate[^"]*"[^>]*>`)
var actionAttr = regexp.MustCompile(`action="([^"]*)"`)

// Login runs the browser login; the error names the step that failed.
func Login(ctx context.Context, hc *http.Client, proxy, publicURL, user string, password secret.Value, header ...string) (BrowserLogin, error) {
	var bl BrowserLogin
	callback := publicURL + "/oidc/callback"
	verifier := randomString()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {"admin-ui"}, "redirect_uri": {callback}, "response_type": {"code"}, "scope": {"openid"},
		"state": {randomString()}, "nonce": {randomString()},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	authURL := proxy + "/auth/realms/" + Realm + "/protocol/openid-connect/auth?" + q.Encode()
	status, h, body, err := send(ctx, hc, http.MethodGet, authURL, "", nil, header)
	if err != nil {
		return bl, fmt.Errorf("the authorization request: %w", err)
	}
	if status != http.StatusOK {
		return bl, fmt.Errorf("the authorization request with redirect URI %s: status %d", callback, status)
	}
	form := formAction.Find(body)
	m := actionAttr.FindSubmatch(form)
	if m == nil {
		return bl, errors.New("the login page has no login form")
	}
	bl.FormAction = html.UnescapeString(string(m[1]))
	if !strings.HasPrefix(bl.FormAction, publicURL+"/auth/") {
		return bl, fmt.Errorf("the login form posts to %s, want %s/auth/…", stripQuery(bl.FormAction), publicURL)
	}
	cookies := cookieHeader(h)
	post := proxy + strings.TrimPrefix(bl.FormAction, publicURL)
	creds := url.Values{"username": {user}, "password": {password.Reveal()}, "credentialId": {""}}.Encode()
	status, h, _, err = send(ctx, hc, http.MethodPost, post, creds, append([]string{"Content-Type", "application/x-www-form-urlencoded", "Cookie", cookies}, header...), nil)
	if err != nil {
		return bl, fmt.Errorf("the login form: %w", err)
	}
	loc := h.Get("Location")
	bl.Location = stripQuery(loc)
	if status != http.StatusFound || bl.Location != callback {
		return bl, fmt.Errorf("the login form: status %d, redirect to %q, want 302 to %s", status, bl.Location, callback)
	}
	lu, err := url.Parse(loc)
	if err != nil || lu.Query().Get("code") == "" {
		return bl, errors.New("the redirect after the login has no code")
	}
	g, err := tokenThrough(ctx, hc, proxy, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {"admin-ui"}, "code": {lu.Query().Get("code")},
		"redirect_uri": {callback}, "code_verifier": {verifier},
	}, header)
	if err != nil {
		return bl, fmt.Errorf("the code exchange: %w", err)
	}
	if g.Token == "" {
		return bl, fmt.Errorf("the code exchange: status %d %s", g.Status, g.Error)
	}
	bl.Token = g.Token
	return bl, nil
}

// tokenThrough is TokenGrant through the proxy with header pairs added.
func tokenThrough(ctx context.Context, hc *http.Client, proxy string, form url.Values, header []string) (Grant, error) {
	status, _, body, err := send(ctx, hc, http.MethodPost, proxy+"/auth/realms/"+Realm+"/protocol/openid-connect/token", form.Encode(),
		append([]string{"Content-Type", "application/x-www-form-urlencoded"}, header...), nil)
	if err != nil {
		return Grant{}, err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	g := Grant{Status: status, Error: out.Error}
	if status == http.StatusOK {
		g.Token = out.AccessToken
	}
	return g, nil
}

// send sends one request with header pairs (and extra pairs) and returns
// the status, the headers and up to 1 MiB of the body; it never follows a
// redirect.
func send(ctx context.Context, hc *http.Client, method, u, body string, header, extra []string) (int, http.Header, []byte, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd) //nolint:gosec // G704: the demo proxy in the compose network is the point
	if err != nil {
		return 0, nil, nil, err
	}
	pairs := append(slices.Clone(header), extra...)
	for i := 0; i+1 < len(pairs); i += 2 {
		req.Header.Set(pairs[i], pairs[i+1])
	}
	resp, err := hc.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: transport error or timeout", method, stripQuery(u))
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, b, nil
}

// cookieHeader turns an answer's cookies into a Cookie field value, Secure
// or not: the proxy is reached over plain HTTP inside the network.
func cookieHeader(h http.Header) string {
	var parts []string
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.MaxAge >= 0 && c.Value != "" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

func stripQuery(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Claims are the parts of an access token's payload the checks print:
// its issuer and realm roles. The signature is not checked; the services
// that accept the token do that.
type Claims struct {
	Issuer string `json:"iss"`
	Realm  struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// TokenClaims decodes token's payload.
func TokenClaims(token string) (Claims, error) {
	var c Claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, errors.New("not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, errors.New("not a JWT")
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, errors.New("not a JWT")
	}
	return c, nil
}
