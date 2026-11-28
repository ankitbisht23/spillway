package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/demo"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// The public demo's commands (SC-09, ADR-0097 §3, §4):
//
//	demo-traffic share-config --url <url> [--secrets-dir /run/demo]
//	demo-traffic share-check --url <url> [--after-stop] [--only s9b]
//	demo-traffic stand-in-tunnel --url https://<host>.example
//
// share-config (make demo-share) changes realm spillway through Keycloak's
// admin API for a share; share-check (make demo-share-test) checks every
// setting of the share, or with --after-stop that make demo-share-stop
// restored them; stand-in-tunnel prints a quick tunnel's lines for a URL
// that resolves nowhere and waits, so make demo-share-test runs the whole
// share path without a tunnel. No password, secret, code or token is ever
// printed: output names URLs, users, clients and booleans only.

const (
	devValue = "dev-only-not-secret" //nolint:gosec // G101: the realm's public dev value
	// forgedIP is the address s9 puts in CF-Connecting-IP (TEST-NET-3).
	forgedIP = "203.0.113.7"
	// localIssuer and localGrafana are the dev settings make
	// demo-share-stop restores.
	localIssuer  = "http://localhost:8180/auth/realms/spillway"
	localGrafana = "http://localhost:8888/grafana/"
)

// localOrigins are admin-ui's localhost origins in realm.json, which a
// share keeps.
var localOrigins = []string{"http://localhost:8080", "http://localhost:5173", "http://localhost:8888"}

// runShareConfig is "demo-traffic share-config --url <url>".
func runShareConfig(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic share-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "the share's public URL, https://<host>")
	dir := fs.String("secrets-dir", "/run/demo", "where the rotated client secrets are written (0600 files <client>-secret)")
	timeout := fs.Duration("timeout", 10*time.Second, "per-call timeout of Keycloak's admin API")
	if code, done := parse(fs, args); done {
		return code
	}
	u, err := demo.CheckPublicURL(*rawURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic share-config:", err)
		return 2
	}
	kcPassword, err := secret.Required(getenv, "KEYCLOAK_ADMIN_PASSWORD")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic share-config:", err)
		return 2
	}
	adminPassword, err := secret.FromEnv(getenv, "DEMO_ADMIN_PASSWORD")
	if err == nil {
		err = demo.CheckAdminPassword("DEMO_ADMIN_PASSWORD", adminPassword)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic share-config:", err)
		return 2
	}
	var gh *demo.GitHub
	ghID := getenv("GITHUB_OAUTH_CLIENT_ID")
	ghSecret, err := secret.FromEnv(getenv, "GITHUB_OAUTH_CLIENT_SECRET")
	switch {
	case err != nil:
		_, _ = fmt.Fprintln(stderr, "demo-traffic share-config:", err)
		return 2
	case (ghID != "") != ghSecret.IsSet():
		_, _ = fmt.Fprintln(stderr, "demo-traffic share-config: set GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET together, or neither")
		return 2
	case ghID != "":
		gh = &demo.GitHub{ClientID: ghID, ClientSecret: ghSecret}
	}
	kc := demo.NewKeycloak(setting(getenv, "SPILLWAY_DEMO_KEYCLOAK_URL", "http://keycloak:8080/auth"), "admin", kcPassword, *timeout)
	err = kc.Share(ctx, demo.ShareConfig{URL: u, AdminPassword: adminPassword, GitHub: gh, WriteSecret: func(client string, v secret.Value) error {
		return writeSecret(*dir, client, v)
	}}, stdout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic share-config: FAIL:", err)
		return 1
	}
	return 0
}

// writeSecret writes v to dir/<client>-secret, mode 0600, replacing it.
func writeSecret(dir, client string, v secret.Value) error {
	name := filepath.Join(dir, client+"-secret")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: the share's own directory
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := io.WriteString(f, v.Reveal()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readSecret reads dir/<client>-secret.
func readSecret(dir, client string) (secret.Value, error) {
	name := filepath.Join(dir, client+"-secret")
	return secret.Required(func(k string) string {
		if k == "S_FILE" {
			return name
		}
		return ""
	}, "S")
}

// checker prints one PASS or FAIL line per check and counts the FAILs.
type checker struct {
	out    io.Writer
	failed int
}

func (c *checker) report(id string, ok bool, format string, args ...any) {
	verdict := "PASS"
	if !ok {
		verdict = "FAIL"
		c.failed++
	}
	_, _ = fmt.Fprintf(c.out, "share-check: %s %s: %s\n", id, verdict, fmt.Sprintf(format, args...))
}

// shareCheck holds what the checks reach.
type shareCheck struct {
	u             string // the public URL; "" after the stop
	proxy, cp, kc string
	gateways      []string
	secrets       string
	kcPassword    secret.Value
	adminPassword secret.Value
	ciSecret      secret.Value // after the stop: the dev stack's spillway-ci secret
	hc            *http.Client
	c             *checker
}

// runShareCheck is "demo-traffic share-check --url <url> [--after-stop]
// [--only s9b]".
func runShareCheck(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic share-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "", "the share's public URL, https://<host>")
	afterStop := fs.Bool("after-stop", false, "check that make demo-share-stop restored the localhost settings (s10)")
	only := fs.String("only", "", "s9b: run only s9's tunnel part (inside the tunnel container)")
	proxy := fs.String("proxy", "http://caddy:8888", "the demo proxy inside the network")
	secrets := fs.String("secrets-dir", "/run/demo", "where share-config wrote the rotated client secrets")
	timeout := fs.Duration("timeout", 15*time.Second, "per-request timeout")
	if code, done := parse(fs, args); done {
		return code
	}
	u, err := demo.CheckPublicURL(*rawURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic share-check:", err)
		return 2
	}
	s := &shareCheck{
		u: u, proxy: strings.TrimSuffix(*proxy, "/"), secrets: *secrets,
		cp: strings.TrimSuffix(setting(getenv, "SPILLWAY_DEMO_CP_URL", "http://controlplane:8080"), "/"),
		kc: strings.TrimSuffix(setting(getenv, "SPILLWAY_DEMO_KEYCLOAK_URL", "http://keycloak:8080/auth"), "/"),
		hc: &http.Client{Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		c:  &checker{out: stdout},
	}
	for g := range strings.SplitSeq(setting(getenv, "SPILLWAY_DEMO_GATEWAYS", "http://gateway-1:8000,http://gateway-2:8000,http://gateway-3:8000"), ",") {
		if g = strings.TrimSpace(g); g != "" {
			s.gateways = append(s.gateways, strings.TrimSuffix(g, "/"))
		}
	}
	switch {
	case *only == "s9b":
		s.s9b(ctx)
	case *only != "":
		_, _ = fmt.Fprintf(stderr, "demo-traffic share-check: --only %q: only s9b\n", *only)
		return 2
	case *afterStop:
		if s.ciSecret, err = secretSetting(getenv); err != nil {
			_, _ = fmt.Fprintln(stderr, "demo-traffic share-check:", err)
			return 2
		}
		s.s10(ctx)
	default:
		for _, name := range []string{"KEYCLOAK_ADMIN_PASSWORD", "DEMO_ADMIN_PASSWORD"} {
			v, err := secret.Required(getenv, name)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, "demo-traffic share-check:", err)
				return 2
			}
			if name == "KEYCLOAK_ADMIN_PASSWORD" {
				s.kcPassword = v
			} else {
				s.adminPassword = v
			}
		}
		s.during(ctx)
	}
	if s.c.failed > 0 {
		_, _ = fmt.Fprintf(stdout, "share-check: %d FAIL\n", s.c.failed)
		return 1
	}
	return 0
}

// get sends GET u with header pairs and decodes a JSON answer into out
// unless nil; it returns the status (0 on a transport error).
func (s *shareCheck) get(ctx context.Context, u string, out any, header ...string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil) //nolint:gosec // G704: the compose network is the point
	if err != nil {
		return 0
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := s.hc.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

// grant is a token from realm spillway's in-network token endpoint.
func (s *shareCheck) grant(ctx context.Context, form url.Values) demo.Grant {
	g, err := demo.TokenGrant(ctx, s.hc, s.kc, demo.Realm, form)
	if err != nil {
		return demo.Grant{}
	}
	return g
}

func (s *shareCheck) clientToken(ctx context.Context, client string, sec secret.Value) demo.Grant {
	return s.grant(ctx, url.Values{"grant_type": {"client_credentials"}, "client_id": {client}, "client_secret": {sec.Reveal()}})
}

func (s *shareCheck) passwordToken(ctx context.Context, client, clientSecret, user, password string) demo.Grant {
	f := url.Values{"grant_type": {"password"}, "client_id": {client}, "username": {user}, "password": {password}}
	if clientSecret != "" {
		f.Set("client_secret", clientSecret)
	}
	return s.grant(ctx, f)
}

// issuers checks s1 and s2 against issuer: Keycloak's discovery through
// the proxy and the UI's OIDC authority.
func (s *shareCheck) issuers(ctx context.Context, id1, id2, issuer, keycloak string) {
	var disc struct {
		Issuer string `json:"issuer"`
		Auth   string `json:"authorization_endpoint"`
	}
	code := s.get(ctx, s.proxy+"/auth/realms/spillway/.well-known/openid-configuration", &disc)
	s.c.report(id1, code == 200 && disc.Issuer == issuer && strings.HasPrefix(disc.Auth, keycloak+"/"),
		"Keycloak through the proxy: status %d, issuer %s, authorization_endpoint %s", code, disc.Issuer, disc.Auth)
	var ui struct {
		Issuer string `json:"issuer"`
	}
	code = s.get(ctx, s.proxy+"/api/ui-config", &ui)
	s.c.report(id2, code == 200 && ui.Issuer == issuer, "the UI's OIDC authority (/api/ui-config): status %d, issuer %s", code, ui.Issuer)
}

// grafanaURL checks s5's appUrl and datasources.
func (s *shareCheck) grafanaURL(ctx context.Context, id, want string) {
	var settings struct {
		AppURL      string `json:"appUrl"`
		Datasources map[string]struct {
			Type string `json:"type"`
			Meta struct {
				ID string `json:"id"`
			} `json:"meta"`
		} `json:"datasources"`
	}
	code := s.get(ctx, s.proxy+"/grafana/api/frontend/settings", &settings)
	var sources []string
	for name, d := range settings.Datasources {
		if d.Type != "datasource" {
			sources = append(sources, name+"/"+d.Type)
		}
	}
	slices.Sort(sources)
	s.c.report(id, code == 200 && settings.AppURL == want && slices.Equal(sources, []string{"Prometheus/prometheus"}),
		"the demo Grafana (anonymous): status %d, appUrl %s, datasources %v", code, settings.AppURL, sources)
}

// during runs s1–s9 and s12 while a share runs.
func (s *shareCheck) during(ctx context.Context) {
	issuer := s.u + "/auth/realms/spillway"
	s.issuers(ctx, "s1", "s2", issuer, s.u+"/auth")

	// s3: the control plane expects the new issuer.
	ciSecret, err := readSecret(s.secrets, "spillway-ci")
	var ci demo.Grant
	if err == nil {
		ci = s.clientToken(ctx, "spillway-ci", ciSecret)
	}
	cl, _ := demo.TokenClaims(ci.Token)
	code := 0
	if ci.Token != "" {
		code = s.get(ctx, s.proxy+"/api/v1/me", nil, "Authorization", "Bearer "+ci.Token)
	}
	s.c.report("s3", err == nil && cl.Issuer == issuer && code == 200,
		"spillway-ci with its rotated secret (file readable %v): token status %d, iss %s; GET /api/v1/me through the proxy: %d", err == nil, ci.Status, cl.Issuer, code)

	// s4: each gateway expects the new issuer.
	gwSecret, err := readSecret(s.secrets, "gateway-tests")
	var gw demo.Grant
	if err == nil {
		gw = s.clientToken(ctx, "gateway-tests", gwSecret)
	}
	gcl, _ := demo.TokenClaims(gw.Token)
	var codes []int
	ok := err == nil && gcl.Issuer == issuer && len(s.gateways) == 3
	for _, g := range s.gateways {
		c := 0
		if gw.Token != "" {
			c = s.get(ctx, g+"/v1/service", nil, "Authorization", "Bearer "+gw.Token)
		}
		codes = append(codes, c)
		ok = ok && c == 204
	}
	s.c.report("s4", ok, "gateway-tests with its rotated secret: token status %d, iss %s; GET /v1/service on %v: %v", gw.Status, gcl.Issuer, s.gateways, codes)

	s.grafanaURL(ctx, "s5", s.u+"/grafana/")

	// s6, s7: Keycloak's admin API.
	kc := demo.NewKeycloak(s.kc, "admin", s.kcPassword, s.hc.Timeout)
	c, err := kc.Client(ctx, "admin-ui")
	redirects, logouts, origins := demo.RedirectURIs(c), demo.PostLogoutURIs(c), demo.WebOrigins(c)
	ok = err == nil && slices.Contains(redirects, s.u+"/oidc/callback") && slices.Contains(logouts, s.u+"/signed-out") && slices.Contains(origins, s.u)
	for _, o := range localOrigins {
		ok = ok && slices.Contains(redirects, o+"/oidc/callback") && slices.Contains(logouts, o+"/signed-out") && slices.Contains(origins, o)
	}
	s.c.report("s6", ok, "admin-ui (admin API, %v): redirect URIs %v, post-logout %v, web origins %v", errText(err), redirects, logouts, origins)

	admin := s.passwordToken(ctx, "admin-cli", "", "test-admin", s.adminPassword.Reveal())
	var adminRoles []string
	adminUser, kcErr := kc.User(ctx, "test-admin")
	if kcErr == nil {
		adminRoles, kcErr = kc.RealmRoles(ctx, adminUser.ID())
	}
	adminDev := s.passwordToken(ctx, "admin-cli", "", "test-admin", devValue)
	viewer := s.passwordToken(ctx, "admin-cli", "", "test-viewer", devValue)
	ciDev := s.clientToken(ctx, "spillway-ci", secret.New(devValue))
	gwDev := s.clientToken(ctx, "gateway-tests", secret.New(devValue))
	disabled := map[string]bool{}
	for _, name := range []string{"test-editor", "test-publisher", "test-key-admin"} {
		usr, err := kc.User(ctx, name)
		kcErr = errors.Join(kcErr, err)
		disabled[name] = err == nil && !usr.Enabled()
	}
	rbac, err := kc.Client(ctx, "rbac-tests")
	kcErr = errors.Join(kcErr, err)
	realm, err := kc.Realm(ctx)
	kcErr = errors.Join(kcErr, err)
	brute, _ := realm["bruteForceProtected"].(bool)
	// A refused password grant is Keycloak's invalid_grant (400 with
	// brute-force detection on, 401 without); a refused client secret is
	// 401 invalid_client.
	refused := func(g demo.Grant) bool {
		return g.Token == "" && (g.Status == 400 || g.Status == 401) && g.Error == "invalid_grant"
	}
	s.c.report("s7", admin.Status == 200 && slices.Contains(adminRoles, "admin") && refused(adminDev) && viewer.Status == 200 &&
		ciDev.Status == 401 && gwDev.Status == 401 && kcErr == nil && !rbac.Enabled() && brute &&
		disabled["test-editor"] && disabled["test-publisher"] && disabled["test-key-admin"],
		"test-admin with DEMO_ADMIN_PASSWORD: %d (realm role admin %v), with the dev password: %d %s; test-viewer: %d; spillway-ci and gateway-tests with the dev secret: %d %s, %d %s; disabled: %v, rbac-tests %v; brute-force detection %v; admin API %v",
		admin.Status, slices.Contains(adminRoles, "admin"), adminDev.Status, adminDev.Error, viewer.Status, ciDev.Status, ciDev.Error, gwDev.Status, gwDev.Error, disabled, !rbac.Enabled(), brute, errText(kcErr))

	// s8: the admin console and API are not behind the proxy.
	c1, c2 := s.get(ctx, s.proxy+"/auth/admin/", nil), s.get(ctx, s.proxy+"/auth/admin/realms/spillway", nil)
	s.c.report("s8", c1 == 404 && c2 == 404, "through the proxy: /auth/admin/ %d, /auth/admin/realms/spillway %d", c1, c2)

	// s9 a and c (b runs inside the tunnel container): a forged
	// CF-Connecting-IP from anywhere but the tunnel is ignored.
	self := ownAddress(ctx, s.cp)
	var viaProxy, direct struct {
		ClientIP string `json:"client_ip"`
	}
	ca, cc := 0, 0
	if ci.Token != "" {
		ca = s.get(ctx, s.proxy+"/api/v1/demo", &viaProxy, "Authorization", "Bearer "+ci.Token, "CF-Connecting-IP", forgedIP)
		cc = s.get(ctx, s.cp+"/api/v1/demo", &direct, "Authorization", "Bearer "+ci.Token, "CF-Connecting-IP", forgedIP)
	}
	s.c.report("s9a", ca == 200 && viaProxy.ClientIP != forgedIP && viaProxy.ClientIP == self,
		"through the proxy from this container (%s) with CF-Connecting-IP %s: status %d, client_ip %s", self, forgedIP, ca, viaProxy.ClientIP)
	s.c.report("s9c", cc == 200 && direct.ClientIP == self,
		"straight to the control plane from this container (%s) with CF-Connecting-IP %s: status %d, client_ip %s", self, forgedIP, cc, direct.ClientIP)

	// s12: a browser's login through the proxy uses the public URL.
	bl, err := demo.Login(ctx, s.hc, s.proxy, s.u, "test-viewer", secret.New(devValue))
	me := 0
	if err == nil {
		lc, _ := demo.TokenClaims(bl.Token)
		me = s.get(ctx, s.proxy+"/api/v1/me", nil, "Authorization", "Bearer "+bl.Token)
		s.c.report("s12", me == 200 && lc.Issuer == issuer,
			"test-viewer's browser login through the proxy: form posts to %s, redirect to %s, token iss %s, GET /api/v1/me %d", stripQuery(bl.FormAction), bl.Location, lc.Issuer, me)
	} else {
		s.c.report("s12", false, "test-viewer's browser login through the proxy: %v", err)
	}
}

// s9b runs inside the tunnel container, whose address the proxy trusts
// for CF-Connecting-IP: the visitor's address reaches getDemo. It logs in
// as test-viewer (the visitors' account) through the proxy.
func (s *shareCheck) s9b(ctx context.Context) {
	bl, err := demo.Login(ctx, s.hc, s.proxy, s.u, "test-viewer", secret.New(devValue))
	if err != nil {
		s.c.report("s9b", false, "test-viewer's login through the proxy from the tunnel container: %v", err)
		return
	}
	var d struct {
		ClientIP string `json:"client_ip"`
	}
	code := s.get(ctx, s.proxy+"/api/v1/demo", &d, "Authorization", "Bearer "+bl.Token, "CF-Connecting-IP", forgedIP)
	s.c.report("s9b", code == 200 && d.ClientIP == forgedIP,
		"through the proxy from the tunnel container (%s) with CF-Connecting-IP %s: status %d, client_ip %s", ownAddress(ctx, s.proxy), forgedIP, code, d.ClientIP)
}

// s10 checks, after make demo-share-stop, that the localhost settings and
// the dev realm are back. Every part uses public endpoints and dev values.
func (s *shareCheck) s10(ctx context.Context) {
	s.issuers(ctx, "s10", "s10", localIssuer, "http://localhost:8180/auth")
	s.grafanaURL(ctx, "s10", localGrafana)
	auth := func(redirect string) int {
		q := url.Values{"client_id": {"admin-ui"}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {"openid"},
			"code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "code_challenge_method": {"S256"}}
		return s.get(ctx, s.proxy+"/auth/realms/spillway/protocol/openid-connect/auth?"+q.Encode(), nil)
	}
	logout := func(redirect string) int {
		q := url.Values{"client_id": {"admin-ui"}, "post_logout_redirect_uri": {redirect}}
		return s.get(ctx, s.proxy+"/auth/realms/spillway/protocol/openid-connect/logout?"+q.Encode(), nil)
	}
	pubAuth, localAuth := auth(s.u+"/oidc/callback"), auth("http://localhost:8888/oidc/callback")
	pubOut, localOut := logout(s.u+"/signed-out"), logout("http://localhost:8888/signed-out")
	// Keycloak answers a valid logout with its confirmation page or a
	// redirect to the URI, an unregistered URI with 400.
	s.c.report("s10", pubAuth == 400 && localAuth == 200 && pubOut == 400 && (localOut == 200 || localOut == 302),
		"admin-ui: authorization with redirect URI %s/oidc/callback %d (localhost:8888 %d); logout to %s/signed-out %d (localhost:8888 %d)",
		s.u, pubAuth, localAuth, s.u, pubOut, localOut)
	editor := s.passwordToken(ctx, "admin-cli", "", "test-editor", devValue)
	admin := s.passwordToken(ctx, "admin-cli", "", "test-admin", devValue)
	ci := s.clientToken(ctx, "spillway-ci", s.ciSecret)
	gw := s.clientToken(ctx, "gateway-tests", secret.New(devValue))
	rbac := s.passwordToken(ctx, "rbac-tests", devValue, "test-viewer", devValue)
	s.c.report("s10", editor.Status == 200 && admin.Status == 200 && ci.Status == 200 && gw.Status == 200 && rbac.Status == 200,
		"the dev realm: test-editor %d, test-admin with the dev password %d, spillway-ci %d, gateway-tests %d, rbac-tests %d",
		editor.Status, admin.Status, ci.Status, gw.Status, rbac.Status)
}

// ownAddress is this container's address towards target's host.
func ownAddress(ctx context.Context, target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	host, _, _ := net.SplitHostPort(conn.LocalAddr().String())
	return host
}

// standInHost reports whether u's host is under .example (RFC 2606).
func standInHost(u string) bool {
	p, err := url.Parse(u)
	return err == nil && strings.HasSuffix(strings.ToLower(p.Hostname()), ".example")
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func stripQuery(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

// runStandInTunnel is "demo-traffic stand-in-tunnel --url <url>": it
// prints the lines cloudflared prints for a new quick tunnel, for a URL
// under .example (RFC 2606, resolves nowhere), and waits until stopped.
// It opens no connection: nothing is published (ADR-0097 §4).
func runStandInTunnel(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo-traffic stand-in-tunnel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rawURL := fs.String("url", "https://stand-in.spillway.example", "the stand-in URL, under .example")
	if code, done := parse(fs, args); done {
		return code
	}
	u, err := demo.CheckPublicURL(*rawURL)
	if err == nil && !standInHost(u) {
		err = fmt.Errorf("the stand-in URL %q: want a host under .example (RFC 2606), which resolves nowhere", u)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "demo-traffic stand-in-tunnel:", err)
		return 2
	}
	now := time.Now().UTC().Format(time.RFC3339)
	const border = "+--------------------------------------------------------------------------------------------+"
	_, _ = fmt.Fprintf(stdout, "%s INF stand-in tunnel (make demo-share-test): nothing is published, no connection is opened\n", now)
	_, _ = fmt.Fprintf(stdout, "%s INF %s\n", now, border)
	_, _ = fmt.Fprintf(stdout, "%s INF |  %-90s|\n", now, "Your quick Tunnel has been created! Visit it at (it may take some time to be reachable):")
	_, _ = fmt.Fprintf(stdout, "%s INF |  %-90s|\n", now, u)
	_, _ = fmt.Fprintf(stdout, "%s INF %s\n", now, border)
	<-ctx.Done()
	return 0
}
