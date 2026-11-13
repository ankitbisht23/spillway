package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
)

// tokenSecret is part of every fake token, so the tests can look for it in
// logs and answers.
const tokenSecret = "tok-dev-only-not-secret" //nolint:gosec // G101: a fake token part the tests look for

// token is a fake bearer token for fakeAuth carrying roles, comma-separated.
func token(roles string) string { return roles + "." + tokenSecret }

// fakeAuth accepts token(roles) as a caller with those roles, whose sub is
// "user-<roles>"; "bad" is invalid and "cold" finds no keys.
type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, tok string) (store.Actor, error) {
	roles, secret, _ := strings.Cut(tok, ".")
	switch {
	case secret != tokenSecret || roles == "bad":
		return store.Actor{}, auth.ErrInvalid
	case roles == "cold":
		return store.Actor{}, auth.ErrUnavailable
	}
	return store.Actor{Sub: "user-" + roles, Name: "name-" + roles, Roles: spillwayRoles(strings.Split(roles, ","))}, nil
}

// all is every role: each may read everything but the audit log (D4).
var all = Roles

// matrix is ADR-0040's RBAC matrix for the operations in api/openapi.yaml,
// written out independently of the spec's x-spillway-roles.
var matrix = map[string][]string{
	"listUpstreams": all, "getUpstream": all, "listRoutes": all, "getRoute": all,
	"listPolicies": all, "getPolicy": all, "listPlans": all, "getPlan": all,
	"createUpstream": {"editor", "admin"}, "updateUpstream": {"editor", "admin"}, "deleteUpstream": {"editor", "admin"},
	"drainEndpoint": {"editor", "admin"}, "createRoute": {"editor", "admin"}, "updateRoute": {"editor", "admin"},
	"deleteRoute": {"editor", "admin"}, "createPolicy": {"editor", "admin"}, "updatePolicy": {"editor", "admin"},
	"deletePolicy": {"editor", "admin"}, "createPlan": {"editor", "admin"}, "updatePlan": {"editor", "admin"},
	"deletePlan":  {"editor", "admin"},
	"listTenants": all, "getTenant": all,
	"createTenant": {"admin"}, "updateTenant": {"admin"}, "deleteTenant": {"admin"},
	"listConsumers": all, "getConsumer": all, "listConsumerKeys": all, "listOverrides": all,
	"getOverride": all, "listAccessLists": all, "getAccessList": all,
	"createConsumer": {"key-admin", "admin"}, "updateConsumer": {"key-admin", "admin"}, "deleteConsumer": {"key-admin", "admin"},
	"createOverride": {"key-admin", "admin"}, "updateOverride": {"key-admin", "admin"}, "deleteOverride": {"key-admin", "admin"},
	"createAccessList": {"key-admin", "admin"}, "updateAccessList": {"key-admin", "admin"}, "deleteAccessList": {"key-admin", "admin"},
	"listAudit": {"admin"},
}

// specOp is one operation of the spec as a request.
type specOp struct {
	id, method, url, body string
}

func specOps(t *testing.T) []specOp {
	t.Helper()
	var ops []specOp
	for path, item := range spec(t).Paths.Map() {
		for method, op := range item.Operations() {
			o := specOp{id: operationID(op), method: method, url: BasePath + strings.NewReplacer("{id}", "1", "{eid}", "2").Replace(path)}
			if op.RequestBody != nil {
				o.body = "{}"
			}
			ops = append(ops, o)
		}
	}
	return ops
}

// TestEveryOperationHasARule fails when an operation of the spec is not in
// the matrix, or its x-spillway-roles differ from it (ADR-0040).
func TestEveryOperationHasARule(t *testing.T) {
	seen := map[string]bool{}
	for path, item := range spec(t).Paths.Map() {
		for method, op := range item.Operations() {
			id := operationID(op)
			seen[id] = true
			want, ok := matrix[id]
			if !ok {
				t.Errorf("%s %s (%s): no rule in the RBAC matrix", method, path, id)
				continue
			}
			if got := rolesOf(op); !slices.Equal(got, want) {
				t.Errorf("%s: x-spillway-roles %v, the matrix says %v", id, got, want)
			}
		}
	}
	for id := range matrix {
		if !seen[id] {
			t.Errorf("%s: in the matrix, not in the spec", id)
		}
	}
}

// TestRoleMatrix calls every operation with a token of each role and with
// none (CP-12, SEC-08): no token is 401, a role the matrix leaves out 403,
// and a listed role reaches the handler, whose answer is anything but 401
// or 403. A token with several roles has their union; one with no Spillway
// role is denied everything.
func TestRoleMatrix(t *testing.T) {
	h := newHandler(t, io.Discard)
	ops := specOps(t)
	if len(ops) != len(matrix) {
		t.Fatalf("%d operations, %d rules", len(ops), len(matrix))
	}
	for _, op := range ops {
		t.Run(op.id, func(t *testing.T) {
			a := do(t, h, op.method, op.url, op.body, "If-Match", `"1"`, "Authorization", "")
			if a.status != http.StatusUnauthorized || a.err.Code != "unauthenticated" || a.header.Get("WWW-Authenticate") != `Bearer realm="spillway"` {
				t.Errorf("no token: %d %s, WWW-Authenticate %q", a.status, a.err.Code, a.header.Get("WWW-Authenticate"))
			}
			for _, role := range append(slices.Clone(Roles), "operator", "editor,publisher") {
				allowed := false
				for _, r := range strings.Split(role, ",") {
					allowed = allowed || slices.Contains(matrix[op.id], r)
				}
				a := do(t, h, op.method, op.url, op.body, "If-Match", `"1"`, "Authorization", "Bearer "+token(role))
				switch {
				case allowed && (a.status == http.StatusUnauthorized || a.status == http.StatusForbidden):
					t.Errorf("%s: %d %q, want it allowed", role, a.status, a.err.Error)
				case !allowed && (a.status != http.StatusForbidden || a.err.Code != "forbidden"):
					t.Errorf("%s: %d %s, want 403 forbidden", role, a.status, a.err.Code)
				}
			}
		})
	}
}

// TestAuthenticationAnswers checks the answers before any role is looked
// at: a bad token, a malformed header, keys never fetched, and that a
// request without a token is refused before it is read (no 400, 413, 415
// or 428 that would answer an anonymous caller about its request).
func TestAuthenticationAnswers(t *testing.T) {
	h := newHandler(t, io.Discard)
	for _, tt := range []struct {
		name, method, path, body string
		header                   []string
		status                   int
		msg, wwwAuth             string
	}{
		{"bad token", "GET", "/api/v1/routes", "", []string{"Authorization", "Bearer " + token("bad")}, 401, "invalid token", `Bearer realm="spillway", error="invalid_token"`},
		{"basic scheme", "GET", "/api/v1/routes", "", []string{"Authorization", "Basic " + token("admin")}, 401, "invalid token", `Bearer realm="spillway", error="invalid_token"`},
		{"empty bearer", "GET", "/api/v1/routes", "", []string{"Authorization", "Bearer "}, 401, "invalid token", `Bearer realm="spillway", error="invalid_token"`},
		{"lower-case scheme", "GET", "/api/v1/routes/abc", "", []string{"Authorization", "bearer " + token("viewer")}, 400, "id", ""},
		{"keys never fetched", "GET", "/api/v1/routes", "", []string{"Authorization", "Bearer " + token("cold")}, 503, "token keys unavailable", ""},
		{"no token, bad JSON", "POST", "/api/v1/routes", `{"name": `, []string{"Authorization", ""}, 401, "a bearer token is required", `Bearer realm="spillway"`},
		{"no token, no If-Match", "DELETE", "/api/v1/tenants/1", "", []string{"Authorization", ""}, 401, "a bearer token is required", `Bearer realm="spillway"`},
		{"no token, bad ID", "GET", "/api/v1/routes/abc", "", []string{"Authorization", ""}, 401, "a bearer token is required", `Bearer realm="spillway"`},
		{"no token, too large", "POST", "/api/v1/routes", `{"name": "` + strings.Repeat("x", MaxBody) + `"}`, []string{"Authorization", ""}, 401, "a bearer token is required", `Bearer realm="spillway"`},
		{"viewer, no If-Match", "DELETE", "/api/v1/tenants/1", "", []string{"Authorization", "Bearer " + token("viewer")}, 403, "needs one of the roles admin", ""},
		{"viewer, not JSON", "POST", "/api/v1/routes", "x", []string{"Authorization", "Bearer " + token("viewer"), "Content-Type", "text/plain"}, 403, "needs one of the roles editor, admin", ""},
		{"no token, unknown path", "GET", "/api/v1/nope", "", []string{"Authorization", ""}, 404, "no such path", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := do(t, h, tt.method, tt.path, tt.body, tt.header...)
			if a.status != tt.status || !strings.Contains(a.err.Error, tt.msg) || a.header.Get("WWW-Authenticate") != tt.wwwAuth {
				t.Errorf("%d %q, WWW-Authenticate %q; want %d %q, %q", a.status, a.err.Error, a.header.Get("WWW-Authenticate"), tt.status, tt.msg, tt.wwwAuth)
			}
			if tt.status == 503 && a.header.Get("Retry-After") != "1" {
				t.Errorf("Retry-After %q", a.header.Get("Retry-After"))
			}
		})
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "/api/v1/routes", nil)
	req.Header.Add("Authorization", "Bearer "+token("admin"))
	req.Header.Add("Authorization", "Bearer "+token("viewer"))
	if _, err := newServerForTest().authenticate(req); err == nil {
		t.Error("two Authorization headers: accepted")
	}
}

func newServerForTest() *Server { return &Server{auth: fakeAuth{}, log: logger(io.Discard)} }

func logger(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }

// doRaw sends a request as do does and returns the answer's body.
func doRaw(t *testing.T, h http.Handler, method, path, body string, header ...string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] != "" {
			req.Header.Set(header[i], header[i+1])
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

// TestDeniedEverywhere checks that an operation without x-spillway-roles,
// with an empty list or with unknown role names only is denied to every
// role, admin included (ADR-0040).
func TestDeniedEverywhere(t *testing.T) {
	for name, ext := range map[string]map[string]any{
		"no extension":  {},
		"empty list":    {"x-spillway-roles": []any{}},
		"unknown names": {"x-spillway-roles": []any{"operator", "root"}},
		"not a list":    {"x-spillway-roles": "admin"},
	} {
		op := operation{id: "x", roles: rolesOf(&openapi3.Operation{Extensions: ext})}
		for _, role := range Roles {
			if op.allows([]string{role}) {
				t.Errorf("%s: %s allowed", name, role)
			}
		}
		var logs bytes.Buffer
		s := newServerForTest()
		s.log = logger(&logs)
		h := s.guard(op, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Errorf("%s: reached the handler", name) }))
		if a := do(t, requestID(h), "GET", "/api/v1/x", ""); a.status != 403 || !strings.Contains(a.err.Error, "not allowed to any role") {
			t.Errorf("%s: %d %q", name, a.status, a.err.Error)
		}
	}
}

// TestDenialsLogged checks that 401 and 403 answers are logged with the
// operation, the sub (403) and the request ID, and that no token, valid or
// not, appears in a log line or an answer (ADR-0040).
func TestDenialsLogged(t *testing.T) {
	var logs bytes.Buffer
	h := newHandler(t, &logs)
	var bodies strings.Builder
	for _, tok := range []string{"", "Bearer " + token("bad"), "Bearer " + token("viewer"), "Bearer " + token("cold"), "Bearer " + token("admin")} {
		rec := doRaw(t, h, "POST", "/api/v1/tenants", `{"name": "acme"}`, "Authorization", tok, "X-Request-Id", "req-denied")
		bodies.WriteString(rec)
	}
	out := logs.String()
	for _, want := range []string{`"operation":"createTenant","status":401,"request_id":"req-denied"`, `"operation":"createTenant","status":403,"sub":"user-viewer","request_id":"req-denied"`} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, tokenSecret) || strings.Contains(bodies.String(), tokenSecret) {
		t.Errorf("a token appears in the logs or an answer:\n%s\n%s", out, bodies.String())
	}
	if strings.Contains(out, "name-viewer") {
		t.Errorf("logs carry the display name, which may be personal data:\n%s", out)
	}
}

// TestOIDCRoles checks the claim mapping of Keycloak tokens: realm roles
// that are Spillway's, in a fixed order; others ignored.
func TestOIDCRoles(t *testing.T) {
	if got := spillwayRoles([]string{"offline_access", "admin", "default-roles-spillway", "viewer", "operator"}); !slices.Equal(got, []string{"viewer", "admin"}) {
		t.Errorf("roles = %v", got)
	}
	if got := spillwayRoles(nil); got == nil || len(got) != 0 {
		t.Errorf("no roles = %#v, want an empty list", got)
	}
}
