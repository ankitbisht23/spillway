package demo

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/secret"
)

const (
	kcAdminPassword = "kc-admin-dev-only-not-secret-1"    //nolint:gosec // G101: test values the tests look for in output
	demoAdminPass   = "demo-admin-dev-only-not-secret-2"  //nolint:gosec // G101: as above
	ghSecretValue   = "github-secret-dev-only-not-secret" //nolint:gosec // G101: as above
	adminToken      = "admin.token.value"                 //nolint:gosec // G101: as above
	newSecretPrefix = "rotated-secret-"                   //nolint:gosec // G101: as above
)

// fakeKeycloak is the admin API and token endpoint of a Keycloak with
// realm spillway as realm.json has it, recording every change.
type fakeKeycloak struct {
	mu       sync.Mutex
	clients  map[string]Rep // by clientId
	users    map[string]Rep // by username
	realm    Rep
	idps     []Rep
	mappers  []Rep
	password map[string]string // user ID → new password
	calls    []string
}

func newFakeKeycloak() *fakeKeycloak {
	f := &fakeKeycloak{clients: map[string]Rep{}, users: map[string]Rep{}, realm: Rep{"realm": "spillway", "bruteForceProtected": false}, password: map[string]string{}}
	f.clients["admin-ui"] = Rep{"id": "uuid-admin-ui", "clientId": "admin-ui", "enabled": true,
		"redirectUris": []any{"http://localhost:8080/oidc/callback", "http://localhost:5173/oidc/callback", "http://localhost:8888/oidc/callback"},
		"webOrigins":   []any{"http://localhost:8080", "http://localhost:5173", "http://localhost:8888"},
		"attributes":   map[string]any{"pkce.code.challenge.method": "S256", "post.logout.redirect.uris": "http://localhost:8080/signed-out##http://localhost:5173/signed-out##http://localhost:8888/signed-out"}}
	for _, c := range []string{"spillway-ci", "gateway-tests", "rbac-tests"} {
		f.clients[c] = Rep{"id": "uuid-" + c, "clientId": c, "enabled": true}
	}
	for _, u := range []string{"test-viewer", "test-editor", "test-publisher", "test-key-admin", "test-admin"} {
		f.users[u] = Rep{"id": "uuid-" + u, "username": u, "enabled": true}
	}
	return f
}

func (f *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/auth/realms/master/protocol/openid-connect/token" {
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != "admin-cli" || r.PostForm.Get("username") != "admin" || r.PostForm.Get("password") != kcAdminPassword {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"`+adminToken+`"}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+adminToken {
		w.WriteHeader(401)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/auth/admin/realms/spillway")
	var body Rep
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	byID := func(m map[string]Rep, id string) Rep {
		for _, v := range m {
			if v.str("id") == id {
				return v
			}
		}
		return nil
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case r.Method == "GET" && p == "/clients":
		if c, ok := f.clients[r.URL.Query().Get("clientId")]; ok {
			write([]Rep{c})
			return
		}
		write([]Rep{})
	case r.Method == "GET" && p == "/users":
		if u, ok := f.users[r.URL.Query().Get("username")]; ok && r.URL.Query().Get("exact") == "true" {
			write([]Rep{u})
			return
		}
		write([]Rep{})
	case r.Method == "PUT" && len(parts) == 2 && parts[0] == "clients":
		c := byID(f.clients, parts[1])
		for k, v := range body {
			c[k] = v
		}
		w.WriteHeader(204)
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "clients" && parts[2] == "client-secret":
		c := byID(f.clients, parts[1])
		c["secret"] = newSecretPrefix + c.str("clientId")
		write(map[string]string{"type": "secret", "value": c.str("secret")})
	case r.Method == "PUT" && len(parts) == 2 && parts[0] == "users":
		u := byID(f.users, parts[1])
		for k, v := range body {
			u[k] = v
		}
		w.WriteHeader(204)
	case r.Method == "PUT" && len(parts) == 3 && parts[0] == "users" && parts[2] == "reset-password":
		if body["type"] != "password" || body["temporary"] != false {
			w.WriteHeader(400)
			return
		}
		f.password[parts[1]], _ = body["value"].(string)
		w.WriteHeader(204)
	case r.Method == "PUT" && p == "":
		for k, v := range body {
			f.realm[k] = v
		}
		w.WriteHeader(204)
	case r.Method == "GET" && p == "":
		write(f.realm)
	case r.Method == "POST" && p == "/identity-provider/instances":
		f.idps = append(f.idps, body)
		w.WriteHeader(201)
	case r.Method == "GET" && p == "/identity-provider/instances":
		write(f.idps)
	case r.Method == "POST" && p == "/identity-provider/instances/github/mappers":
		f.mappers = append(f.mappers, body)
		w.WriteHeader(201)
	default:
		w.WriteHeader(404)
	}
}

// TestShare: share-config's changes (ADR-0097 §3 step 5): admin-ui gets
// the public callback, post-logout URI and origin beside the localhost
// ones; test-editor, test-publisher and test-key-admin are disabled,
// test-admin's password is DEMO_ADMIN_PASSWORD (not temporary), test-viewer
// stays; spillway-ci and gateway-tests get new secrets, handed to
// WriteSecret only; rbac-tests is disabled; brute-force detection is on;
// no GitHub login without its settings. Nothing printed carries a
// password, a secret or a token, and running it twice adds nothing twice.
func TestShare(t *testing.T) {
	f := newFakeKeycloak()
	srv := httptest.NewServer(f)
	defer srv.Close()
	kc := NewKeycloak(srv.URL+"/auth", "admin", secret.New(kcAdminPassword), 5*time.Second)
	kept := map[string]string{}
	cfg := ShareConfig{URL: "https://demo.spillway.example/", AdminPassword: secret.New(demoAdminPass), WriteSecret: func(c string, v secret.Value) error {
		kept[c] = v.Reveal()
		return nil
	}}
	var out bytes.Buffer
	for range 2 {
		if err := kc.Share(t.Context(), cfg, &out); err != nil {
			t.Fatal(err)
		}
	}
	const u = "https://demo.spillway.example"
	ui := f.clients["admin-ui"]
	if got := RedirectURIs(ui); !slices.Equal(got, []string{"http://localhost:8080/oidc/callback", "http://localhost:5173/oidc/callback", "http://localhost:8888/oidc/callback", u + "/oidc/callback"}) {
		t.Errorf("redirect URIs %v", got)
	}
	if got := WebOrigins(ui); !slices.Equal(got, []string{"http://localhost:8080", "http://localhost:5173", "http://localhost:8888", u}) {
		t.Errorf("web origins %v", got)
	}
	if got := PostLogoutURIs(ui); !slices.Equal(got, []string{"http://localhost:8080/signed-out", "http://localhost:5173/signed-out", "http://localhost:8888/signed-out", u + "/signed-out"}) {
		t.Errorf("post-logout URIs %v", got)
	}
	if ui["attributes"].(map[string]any)["pkce.code.challenge.method"] != "S256" {
		t.Error("admin-ui lost its other attributes")
	}
	for name, enabled := range map[string]bool{"test-viewer": true, "test-editor": false, "test-publisher": false, "test-key-admin": false, "test-admin": true} {
		if f.users[name].Enabled() != enabled {
			t.Errorf("%s enabled %v, want %v", name, f.users[name].Enabled(), enabled)
		}
	}
	if len(f.password) != 1 || f.password["uuid-test-admin"] != demoAdminPass {
		t.Errorf("passwords set for %d users, test-admin's right: %v", len(f.password), f.password["uuid-test-admin"] == demoAdminPass)
	}
	if kept["spillway-ci"] != newSecretPrefix+"spillway-ci" || kept["gateway-tests"] != newSecretPrefix+"gateway-tests" || len(kept) != 2 {
		t.Errorf("kept secrets for %d clients", len(kept))
	}
	if f.clients["rbac-tests"].Enabled() || !f.clients["spillway-ci"].Enabled() || f.realm["bruteForceProtected"] != true || len(f.idps) != 0 {
		t.Errorf("rbac-tests enabled %v, brute force %v, identity providers %d", f.clients["rbac-tests"].Enabled(), f.realm["bruteForceProtected"], len(f.idps))
	}
	for _, bad := range []string{kcAdminPassword, demoAdminPass, adminToken, newSecretPrefix} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("the output has %q:\n%s", bad, out.String())
		}
	}
	if !strings.Contains(out.String(), "redirect URI "+u+"/oidc/callback") || !strings.Contains(out.String(), "brute-force detection on") {
		t.Errorf("output:\n%s", out.String())
	}
}

// TestShareGitHub: with GitHub's settings the realm brokers GitHub logins
// with a hardcoded viewer role: GitHub visitors are viewers (D20).
func TestShareGitHub(t *testing.T) {
	f := newFakeKeycloak()
	srv := httptest.NewServer(f)
	defer srv.Close()
	kc := NewKeycloak(srv.URL+"/auth", "admin", secret.New(kcAdminPassword), 5*time.Second)
	var out bytes.Buffer
	err := kc.Share(t.Context(), ShareConfig{URL: "https://admin.spillway.example", AdminPassword: secret.New(demoAdminPass),
		GitHub:      &GitHub{ClientID: "gh-client-id", ClientSecret: secret.New(ghSecretValue)},
		WriteSecret: func(string, secret.Value) error { return nil }}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.idps) != 1 || f.idps[0]["alias"] != "github" || f.idps[0]["providerId"] != "github" || f.idps[0]["trustEmail"] != false {
		t.Fatalf("identity providers %v", len(f.idps))
	}
	cfg := f.idps[0]["config"].(map[string]any)
	if cfg["clientId"] != "gh-client-id" || cfg["clientSecret"] != ghSecretValue || cfg["syncMode"] != "IMPORT" {
		t.Errorf("github config: client ID %v, sync mode %v", cfg["clientId"], cfg["syncMode"])
	}
	if len(f.mappers) != 1 || f.mappers[0]["identityProviderMapper"] != "oidc-hardcoded-role-idp-mapper" || f.mappers[0]["config"].(map[string]any)["role"] != "viewer" {
		t.Errorf("mappers %v", f.mappers)
	}
	if strings.Contains(out.String(), ghSecretValue) {
		t.Error("the output has the GitHub client secret")
	}
}

// TestShareRefuses: a bad URL or admin password stops before any change;
// a wrong Keycloak admin password fails without naming it.
func TestShareRefuses(t *testing.T) {
	f := newFakeKeycloak()
	srv := httptest.NewServer(f)
	defer srv.Close()
	write := func(string, secret.Value) error { return nil }
	for _, c := range []struct {
		url, password, want string
	}{
		{"http://demo.example", demoAdminPass, "want https://<host> with no path"},
		{"https://demo.example/app", demoAdminPass, "want https://<host> with no path"},
		{"https://demo.example", "short", "shorter than 16 characters"},
		{"https://demo.example", devValue, "is the public dev value"},
		{"https://demo.example", "", "DEMO_ADMIN_PASSWORD is not set"},
	} {
		kc := NewKeycloak(srv.URL+"/auth", "admin", secret.New(kcAdminPassword), 5*time.Second)
		err := kc.Share(t.Context(), ShareConfig{URL: c.url, AdminPassword: secret.New(c.password), WriteSecret: write}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s / %q: %v, want %q", c.url, c.password, err, c.want)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("Keycloak was called: %v", f.calls)
	}
	if err := CheckAdminPassword("X", secret.New(devValue+"-padded")); err != nil {
		t.Errorf("a long password: %v", err)
	}
	kc := NewKeycloak(srv.URL+"/auth", "admin", secret.New("wrong-dev-only-not-secret"), 5*time.Second)
	err := kc.Share(t.Context(), ShareConfig{URL: "https://demo.example", AdminPassword: secret.New(demoAdminPass), WriteSecret: write}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "admin login: status 401") || strings.Contains(err.Error(), "wrong-dev") {
		t.Errorf("a wrong admin password: %v", err)
	}
}

func TestCheckPublicURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://abc.trycloudflare.com":      "https://abc.trycloudflare.com",
		"https://abc.trycloudflare.com/":     "https://abc.trycloudflare.com",
		"https://stand-in.spillway.example":  "https://stand-in.spillway.example",
		"https://admin.example.org:8443":     "https://admin.example.org:8443",
		"http://abc.trycloudflare.com":       "",
		"https://abc.trycloudflare.com/auth": "",
		"https://u:p@abc.example":            "",
		"https://abc.example?x=1":            "",
		"https://abc.example#f":              "",
		"https://":                           "",
		"abc.example":                        "",
	} {
		got, err := CheckPublicURL(in)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("CheckPublicURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// fakeLogin is Keycloak's login pages behind the demo proxy for public
// URL u: the authorization request answers the login form (posting to
// u/auth/…), which redirects to the callback with a code that the token
// endpoint exchanges for its PKCE verifier.
func fakeLogin(t *testing.T, u *string, wantHeader string) http.Handler {
	t.Helper()
	var challenge string
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantHeader != "" && r.Header.Get("CF-Connecting-IP") != wantHeader {
			w.WriteHeader(418)
			return
		}
		switch r.URL.Path {
		case "/auth/realms/spillway/protocol/openid-connect/auth":
			q := r.URL.Query()
			if q.Get("redirect_uri") != *u+"/oidc/callback" || q.Get("client_id") != "admin-ui" || q.Get("code_challenge_method") != "S256" {
				w.WriteHeader(400)
				return
			}
			challenge = q.Get("code_challenge")
			http.SetCookie(w, &http.Cookie{Name: "AUTH_SESSION_ID", Value: "sess", Path: "/auth/realms/spillway/", Secure: true, HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "KC_RESTART", Value: "", MaxAge: -1})
			_, _ = fmt.Fprintf(w, `<html><form id="kc-form-login" onsubmit="x" action="%s/auth/realms/spillway/login-actions/authenticate?session_code=a&amp;execution=b&amp;client_id=admin-ui" method="post"></form></html>`, *u)
		case "/auth/realms/spillway/login-actions/authenticate":
			_ = r.ParseForm()
			if r.URL.Query().Get("execution") != "b" || !strings.Contains(r.Header.Get("Cookie"), "AUTH_SESSION_ID=sess") ||
				r.PostForm.Get("username") != "test-viewer" || r.PostForm.Get("password") != devValue {
				w.WriteHeader(200)
				return
			}
			w.Header().Set("Location", *u+"/oidc/callback?state=s&session_state=x&code=the-code")
			w.WriteHeader(302)
		case "/auth/realms/spillway/protocol/openid-connect/token":
			_ = r.ParseForm()
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if r.PostForm.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.PostForm.Get("redirect_uri") != *u+"/oidc/callback" {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + *u + `/auth/realms/spillway","realm_access":{"roles":["viewer"]}}`))
			_, _ = io.WriteString(w, `{"access_token":"h.`+payload+`.s"}`)
		default:
			w.WriteHeader(404)
		}
	})
}

// TestLogin: the browser login follows Keycloak's pages through the proxy
// for the public URL, with the session cookie (Secure or not) and PKCE, and
// returns a token whose claims name the public issuer; a login form that
// posts elsewhere fails, naming where.
func TestLogin(t *testing.T) {
	u := "https://stand-in.spillway.example"
	srv := httptest.NewServer(fakeLogin(t, &u, "203.0.113.7"))
	defer srv.Close()
	hc := &http.Client{CheckRedirect: noRedirect, Timeout: 5 * time.Second}
	bl, err := Login(t.Context(), hc, srv.URL, u, "test-viewer", secret.New(devValue), "CF-Connecting-IP", "203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	c, err := TokenClaims(bl.Token)
	if err != nil || c.Issuer != u+"/auth/realms/spillway" || !slices.Equal(c.Realm.Roles, []string{"viewer"}) {
		t.Errorf("claims %+v, %v", c, err)
	}
	if bl.Location != u+"/oidc/callback" || !strings.HasPrefix(bl.FormAction, u+"/auth/realms/spillway/login-actions/authenticate?") {
		t.Errorf("form action %q, location %q", bl.FormAction, bl.Location)
	}
	if _, err := Login(t.Context(), hc, srv.URL, "https://other.example", "test-viewer", secret.New(devValue), "CF-Connecting-IP", "203.0.113.7"); err == nil ||
		!strings.Contains(err.Error(), "redirect URI https://other.example/oidc/callback: status 400") {
		t.Errorf("an unregistered callback: %v", err)
	}
	if _, err := Login(t.Context(), hc, srv.URL, u, "test-viewer", secret.New("wrong-dev-only-not-secret"), "CF-Connecting-IP", "203.0.113.7"); err == nil ||
		strings.Contains(err.Error(), "wrong-dev") || !strings.Contains(err.Error(), "the login form: status 200") {
		t.Errorf("a wrong password: %v", err)
	}
	// Keycloak writing another host into the form (KC_HOSTNAME not set).
	other := "http://localhost:8180"
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `<form id="kc-form-login" action="%s/auth/realms/spillway/login-actions/authenticate?x=1"></form>`, other)
	}))
	defer srv2.Close()
	if _, err := Login(t.Context(), hc, srv2.URL, u, "test-viewer", secret.New(devValue)); err == nil ||
		!strings.Contains(err.Error(), "posts to http://localhost:8180/auth/realms/spillway/login-actions/authenticate, want "+u) {
		t.Errorf("a form posting to localhost: %v", err)
	}
}

func TestTokenClaims(t *testing.T) {
	if _, err := TokenClaims("not-a-token"); err == nil {
		t.Error("not a JWT: no error")
	}
	if _, err := TokenClaims("a.%%%.c"); err == nil {
		t.Error("bad base64: no error")
	}
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://x.example/auth/realms/spillway","realm_access":{"roles":["admin"]}}`))
	c, err := TokenClaims("h." + p + ".s")
	if err != nil || c.Issuer != "https://x.example/auth/realms/spillway" || !slices.Contains(c.Realm.Roles, "admin") {
		t.Errorf("%+v %v", c, err)
	}
}
