//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// gatewayTestsSubject is the sub of the gateway-tests client's tokens: its
// service account's fixed ID in the dev realm (ADR-0040, ADR-0043).
const gatewayTestsSubject = "0b5e7c1a-5f1d-4c3e-9a00-0000000000c2"

// TestComposeJWT sends Keycloak's client-credentials tokens through
// gateway-1 in managed mode (GW-18 - GW-20, SEC-04, ADR-0039): a jwt route
// on the compose config, a consumer whose jwt_subject is gateway-tests'
// service account. A gateway-tests token, whose iss is the browser-facing
// issuer while gateway-1 fetches the keys from keycloak:8080, is proxied
// with its Authorization header unchanged; no token, a token for the
// control plane's audience, and a token with a broken signature are 401;
// and no token appears in gateway-1's log. Every wait allows 30 s, far
// above the 820 ms host stalls of ADR-0034.
func TestComposeJWT(t *testing.T) {
	c := composeControlPlane(t)
	c = c.as(keycloakToken(t, "client_credentials", "spillway-ci", ""))
	root := filepath.Join("..", "..")

	// The compose config plus a jwt route and plan it-keys, published.
	data, err := os.ReadFile(filepath.Join(root, "deploy", "compose", "gateway", "config.yaml")) //nolint:gosec // G304: a config file of this repo
	if err != nil {
		t.Fatal(err)
	}
	const route = "  - { name: it-jwt, match: { path_prefix: /anything/it-jwt }, upstream: httpbin, auth_mode: jwt }\n"
	cfg := strings.Replace(string(data), "\nroutes:\n", "\nroutes:\n"+route, 1)
	if cfg == string(data) {
		t.Fatal("the compose config has no routes: line")
	}
	cfg += "plans:\n  - name: it-keys\n"
	_, tag := exportYAML(t, c, "")
	importYAML(t, c, 200, []byte(cfg), tag, "?note=TestComposeJWT")
	_, tag = validate(t, c)
	if a := c.call("POST", "/config/publish", note("TestComposeJWT: the compose config, route it-jwt and plan it-keys"), "If-Match", tag); a.status != 200 && a.status != 409 {
		t.Fatalf("publish: %d %s", a.status, a.body)
	}
	var latest apiv1.ConfigVersionList
	c.must(200, "GET", "/config/versions?limit=1", nil).decode(t, &latest)
	plan, _ := lookup(t, c, "/plans", "it-keys")

	removeTestTenants(t, c, "it-jwt-") // left by a run that did not finish
	name := fmt.Sprintf("it-jwt-%d", time.Now().UnixNano()%1e9)
	tenant, _ := created(t, c, "/tenants", map[string]any{"name": name, "plan_id": plan})
	consumer, _ := created(t, c, "/consumers", map[string]any{"tenant_id": tenant, "name": name, "jwt_subject": gatewayTestsSubject})
	t.Cleanup(func() { removeTestTenants(t, c, "it-jwt-") })
	var cons apiv1.Consumer
	c.must(200, "GET", fmt.Sprintf("/consumers/%d", consumer), nil).decode(t, &cons)
	composeManaged(t)
	gw1 := &proc{name: "gateway-1", admin: "http://127.0.0.1:8001"}
	eventually(t, "gateway-1 with the route's config and the consumer's identity", func() bool {
		return gw1.metric("gw_config_version") >= float64(latest.Items[0].Version) && gw1.metric("gw_identity_version") >= float64(*cons.Version)
	})

	token := keycloakToken(t, "client_credentials", "gateway-tests", "")
	other := keycloakToken(t, "client_credentials", "spillway-ci", "") // audience spillway-controlplane
	sig := token[strings.LastIndexByte(token, '.')+1:]
	broken := token[:len(token)-len(sig)] + map[bool]string{true: "A", false: "B"}[sig[0] != 'A'] + sig[1:]
	send := func(authz string) (int, string) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8000/anything/it-jwt/orders", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, ""
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	// The first fetch of gateway-1's keys runs in the background.
	var body string
	eventually(t, "a gateway-tests token proxied through gateway-1", func() bool {
		var code int
		code, body = send("Bearer " + token)
		return code == 200
	})
	if !strings.Contains(body, token) { // the upstream echoes the headers it got; never printed
		t.Error("the upstream did not get the Authorization header unchanged")
	}
	for what, tt := range map[string]struct{ authz, want string }{
		"no token":                       {"", `{"error":"bearer token required"}`},
		"the control plane's audience":   {"Bearer " + other, `{"error":"invalid token"}`},
		"a broken signature":             {"Bearer " + broken, `{"error":"invalid token"}`},
		"the token as a Basic password":  {"Basic " + token, `{"error":"invalid token"}`},
		"the token without its envelope": {token, `{"error":"invalid token"}`},
	} {
		if code, got := send(tt.authz); code != 401 || got != tt.want {
			t.Errorf("%s: %d %q, want 401 %s", what, code, got, tt.want)
		}
	}

	// gateway-1's own log: access lines and events, never a token.
	cmd := exec.CommandContext(t.Context(), "docker", "compose", "--env-file", ".env.example", "logs", "--no-log-prefix", "gateway-1") //nolint:gosec // G204: fixed docker compose commands
	cmd.Dir = root
	logs, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), `"route":"it-jwt"`) {
		t.Error("gateway-1 logged no access line for route it-jwt")
	}
	for what, s := range map[string]string{"token": token, "other token": other, "broken token": broken, "signature": sig, "payload": strings.Split(token, ".")[1]} {
		if strings.Contains(string(logs), s) {
			t.Errorf("gateway-1's log holds a %s", what)
		}
	}
}
