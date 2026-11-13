//go:build integration

package integration

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// Bearer tokens, RBAC and the audit log (CP-11, CP-12, CP-13, SEC-08,
// ADR-0040).

const testIssuerURL = "http://issuer.test/realms/spillway"

// testIssuer signs tokens for the control plane under test and serves its
// JWKS, as Keycloak does in compose.
type testIssuer struct {
	key  *rsa.PrivateKey
	jwks *httptest.Server
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &testIssuer{key: key}
	iss.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: key.Public(), KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(iss.jwks.Close)
	return iss
}

// verifier is a running verifier of the issuer's tokens.
func (iss *testIssuer) verifier(t *testing.T) *auth.Verifier {
	t.Helper()
	v, err := auth.New(auth.Config{Issuer: testIssuerURL, JWKSURL: iss.jwks.URL, Audience: "spillway-controlplane", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go v.Run(ctx)
	for deadline := time.Now().Add(5 * time.Second); !v.Ready() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	return v
}

// token is an access token for sub, as Keycloak shapes it.
func (iss *testIssuer) token(t *testing.T, sub, name string, roles ...string) string {
	t.Helper()
	return iss.sign(t, map[string]any{
		"iss": testIssuerURL, "sub": sub, "aud": "spillway-controlplane", "exp": time.Now().Add(5 * time.Minute).Unix(),
		"preferred_username": name, "realm_access": map[string]any{"roles": append(roles, "offline_access")},
	})
}

func (iss *testIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: iss.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := s.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// specRoles is every operation of the spec with its method, path and
// x-spillway-roles (the unit tests pin those to ADR-0040's matrix).
type specOperation struct {
	id, method, path string
	roles            []string
	body             bool
}

func specOperations(t *testing.T) []specOperation {
	t.Helper()
	spec, err := apiv1.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	var ops []specOperation
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			o := specOperation{id: op.OperationID, method: method, path: path, body: op.RequestBody != nil}
			raw, _ := op.Extensions["x-spillway-roles"].([]any)
			for _, r := range raw {
				o.roles = append(o.roles, r.(string))
			}
			ops = append(ops, o)
		}
	}
	return ops
}

// matrix calls every operation as each of tokens (role → token) and with
// no token: no token is 401, a role the spec does not list 403, a listed
// role anything else. IDs in paths are the largest int64, which no entity
// has, and bodies are {}, so no call changes anything.
func matrix(t *testing.T, c cpClient, tokens map[string]string) {
	t.Helper()
	ops := specOperations(t)
	if len(ops) < 43 {
		t.Fatalf("%d operations", len(ops))
	}
	id := fmt.Sprint(int64(math.MaxInt64))
	for _, op := range ops {
		path := strings.NewReplacer("{id}", id, "{eid}", id, "{v}", id).Replace(op.path)
		var body any
		if op.body {
			body = map[string]any{}
		}
		if a := c.as("").call(op.method, path, body, "If-Match", `"1"`); a.status != 401 {
			t.Errorf("%s without a token: %d %s", op.id, a.status, a.body)
		}
		for role, tok := range tokens {
			a := c.as(tok).call(op.method, path, body, "If-Match", `"1"`)
			allowed := slices.Contains(op.roles, role)
			if allowed && (a.status == 401 || a.status == 403) || !allowed && a.status != 403 {
				t.Errorf("%s as %s: %d %s, want it allowed: %v", op.id, role, a.status, a.body, allowed)
			}
		}
	}
}

// TestControlPlaneRoleMatrix runs the role matrix against a control plane
// on Postgres, with tokens of a test issuer (CP-12, SEC-08).
func TestControlPlaneRoleMatrix(t *testing.T) {
	c, iss, _ := startControlPlane(t, io.Discard)
	tokens := map[string]string{}
	for _, role := range api.Roles {
		tokens[role] = iss.token(t, "user-"+role, "test-"+role, role)
	}
	tokens["none"] = iss.token(t, "user-none", "test-none")
	matrix(t, c, tokens)
}

// TestControlPlaneTokens checks the token rules against a control plane on
// Postgres (CP-11, ADR-0039): expired, foreign, wrong audience or issuer,
// and a token whose sub is empty are 401, and none appears in a log line
// or an answer.
func TestControlPlaneTokens(t *testing.T) {
	var logs syncBuffer
	c, iss, _ := startControlPlane(t, &logs)
	other := newTestIssuer(t)
	base := func() map[string]any {
		return map[string]any{"iss": testIssuerURL, "sub": "s", "aud": "spillway-controlplane", "exp": time.Now().Add(time.Minute).Unix(), "realm_access": map[string]any{"roles": []string{"admin"}}}
	}
	with := func(k string, v any) map[string]any {
		m := base()
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	bad := map[string]string{
		"expired":        iss.sign(t, with("exp", time.Now().Add(-2*time.Minute).Unix())),
		"other key":      other.sign(t, base()),
		"gateway aud":    iss.sign(t, with("aud", "spillway-gateway")),
		"in-network iss": iss.sign(t, with("iss", "http://keycloak:8080/realms/spillway")),
		"no sub":         iss.sign(t, with("sub", nil)),
	}
	var answers strings.Builder
	for name, tok := range bad {
		a := c.as(tok).call("GET", "/plans", nil)
		answers.Write(a.body)
		if a.status != 401 || a.header.Get("WWW-Authenticate") != `Bearer realm="spillway", error="invalid_token"` {
			t.Errorf("%s: %d %s, WWW-Authenticate %q", name, a.status, a.body, a.header.Get("WWW-Authenticate"))
		}
	}
	good := iss.sign(t, base())
	c.as(good).must(200, "GET", "/plans", nil)
	c.as(good).must(201, "POST", "/plans", map[string]any{"name": "logged"})
	for name, tok := range map[string]string{"good": good, "expired": bad["expired"], "other key": bad["other key"]} {
		sig := tok[strings.LastIndexByte(tok, '.')+1:]
		if strings.Contains(logs.String(), sig) || strings.Contains(answers.String(), sig) {
			t.Errorf("the %s token appears in a log line or an answer", name)
		}
	}
}

// auditPage is GET /audit's answer.
func auditPage(t *testing.T, c cpClient, query string) apiv1.AuditList {
	t.Helper()
	var l apiv1.AuditList
	c.must(200, "GET", "/audit"+query, nil).decode(t, &l)
	return l
}

// TestControlPlaneAudit checks the audit log (CP-13, SEC-08, ADR-0040):
// one record per successful mutation with the actor, action, entity, the
// REST representation before and after, the request ID and the identity
// version; none for a refused change; notes of access-list entries and
// overrides; the IDs a consumer's delete cascaded to; filters and pages;
// admin only; and no token, key hash or password in any record or log.
func TestControlPlaneAudit(t *testing.T) {
	var logs syncBuffer
	c, iss, db := startControlPlane(t, &logs)
	editorTok := iss.token(t, "sub-editor", "test-editor", "editor")
	keyAdminTok := iss.token(t, "sub-key-admin", "test-key-admin", "key-admin")
	adminTok := iss.token(t, "sub-admin", "test-admin", "admin")
	editor, keyAdmin, admin := c.as(editorTok), c.as(keyAdminTok), c.as(adminTok)

	// Config: create, a refused duplicate, update, a stale update, drain, delete.
	up := map[string]any{"name": "orders", "endpoints": []any{map[string]any{"address": "172.29.0.10:8080"}}}
	created := editor.must(201, "POST", "/upstreams", up, "X-Request-Id", "req-create")
	var u apiv1.Upstream
	created.decode(t, &u)
	path := fmt.Sprintf("/upstreams/%d", *u.Id)
	editor.must(409, "POST", "/upstreams", up)
	up["lb"] = "round_robin"
	updated := editor.must(200, "PUT", path, up, "If-Match", created.etag(), "X-Request-Id", "req-update")
	editor.must(412, "PUT", path, up, "If-Match", created.etag())
	var u2 apiv1.Upstream
	updated.decode(t, &u2)
	drained := editor.must(200, "POST", fmt.Sprintf("%s/endpoints/%d/drain", path, *u2.Endpoints[0].Id), nil, "If-Match", updated.etag())
	editor.must(204, "DELETE", path, nil, "If-Match", drained.etag(), "X-Request-Id", "req-delete")

	recs := auditPage(t, admin, fmt.Sprintf("?entity=upstream&entity_id=%d", *u.Id)).Items
	if len(recs) != 4 {
		t.Fatalf("%d records for the upstream, want 4 (refused changes leave none): %+v", len(recs), recs)
	}
	asJSON := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		b, _ = json.Marshal(m)
		return string(b)
	}
	del, drain, upd, cre := recs[0], recs[1], recs[2], recs[3]
	for _, r := range []struct {
		rec                   apiv1.AuditRecord
		action, before, after string
	}{
		{cre, "upstream.create", "null", asJSON(json.RawMessage(created.body))},
		{upd, "upstream.update", asJSON(json.RawMessage(created.body)), asJSON(json.RawMessage(updated.body))},
		{drain, "endpoint.drain", asJSON(json.RawMessage(updated.body)), asJSON(json.RawMessage(drained.body))},
		{del, "upstream.delete", asJSON(json.RawMessage(drained.body)), "null"},
	} {
		if r.rec.Action != r.action || r.rec.Entity != "upstream" || *r.rec.EntityId != fmt.Sprint(*u.Id) {
			t.Errorf("%s: %+v", r.action, r.rec)
		}
		if got := asJSON(r.rec.Diff["before"]); got != r.before {
			t.Errorf("%s: before %s, want %s", r.action, got, r.before)
		}
		if got := asJSON(r.rec.Diff["after"]); got != r.after {
			t.Errorf("%s: after %s, want %s", r.action, got, r.after)
		}
		if r.rec.Actor.Sub != "sub-editor" || r.rec.Actor.Name != "test-editor" || !slices.Equal(r.rec.Actor.Roles, []string{"editor"}) {
			t.Errorf("%s: actor %+v", r.action, r.rec.Actor)
		}
		if r.rec.ConfigVersion != nil || r.rec.IdentityVersion != nil || r.rec.Note != nil || r.rec.RequestId == "" {
			t.Errorf("%s: versions %v %v, note %v, request ID %q", r.action, r.rec.ConfigVersion, r.rec.IdentityVersion, r.rec.Note, r.rec.RequestId)
		}
	}
	if cre.RequestId != "req-create" || upd.RequestId != "req-update" || del.RequestId != "req-delete" {
		t.Errorf("request IDs %q %q %q", cre.RequestId, upd.RequestId, del.RequestId)
	}

	// Identity: a tenant, a consumer with a key, an entry and an override,
	// then the consumer's delete, which cascades.
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	publish(t, owner, `{"plans": [{"id": 1, "name": "free"}], "policies": [{"id": 5, "name": "per-consumer"}]}`)
	var ten apiv1.Tenant
	admin.must(201, "POST", "/tenants", map[string]any{"name": "acme", "plan_id": 1}).decode(t, &ten)
	var con apiv1.Consumer
	keyAdmin.must(201, "POST", "/consumers", map[string]any{"tenant_id": *ten.Id, "name": "bot"}).decode(t, &con)
	const keyHash = "dev-only-not-secret-key-hash-0001"
	var keyID int64
	if err := owner.QueryRow(t.Context(), `INSERT INTO api_keys (consumer_id, prefix, hash, version) VALUES ($1, 'Ab3dE6gH', sha256($2::bytea), 9) RETURNING id`, *con.Id, keyHash).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	var entry, keyEntry apiv1.AccessListEntry
	keyAdmin.must(201, "POST", "/access-lists", map[string]any{"action": "allow", "subject_type": "consumer", "consumer_id": *con.Id, "note": "partner"}).decode(t, &entry)
	keyAdmin.must(201, "POST", "/access-lists", map[string]any{"action": "deny", "subject_type": "key", "api_key_id": keyID, "note": "leaked"}).decode(t, &keyEntry)
	var ov apiv1.Override
	keyAdmin.must(201, "POST", "/overrides", map[string]any{"consumer_id": *con.Id, "policy_id": 5, "limits": []any{map[string]any{"rate": 30, "period": "1s"}}, "note": "launch week"}).decode(t, &ov)
	keyAdmin.must(204, "DELETE", fmt.Sprintf("/consumers/%d", *con.Id), nil, "If-Match", fmt.Sprintf(`"%d"`, *con.Version))

	var tenRec apiv1.AuditRecord
	for _, r := range auditPage(t, admin, "?entity=tenant").Items {
		tenRec = r
	}
	if tenRec.Action != "tenant.create" || tenRec.IdentityVersion == nil || *tenRec.IdentityVersion != *ten.Version || tenRec.Actor.Sub != "sub-admin" {
		t.Errorf("tenant record %+v, want identity version %d", tenRec, *ten.Version)
	}
	entries := auditPage(t, admin, "?entity=access_list").Items
	if len(entries) != 2 || entries[1].Note == nil || *entries[1].Note != "partner" || *entries[0].Note != "leaked" {
		t.Errorf("access-list records %+v, want their notes", entries)
	}
	ovRecs := auditPage(t, admin, "?entity=override").Items
	if len(ovRecs) != 1 || ovRecs[0].Note == nil || *ovRecs[0].Note != "launch week" {
		t.Errorf("override records %+v", ovRecs)
	}
	conRecs := auditPage(t, admin, fmt.Sprintf("?entity=consumer&entity_id=%d", *con.Id)).Items
	if len(conRecs) != 2 || conRecs[0].Action != "consumer.delete" {
		t.Fatalf("consumer records %+v", conRecs)
	}
	removed := asJSON(conRecs[0].Diff["removed"])
	if want := asJSON(map[string][]int64{"access_list_ids": {*entry.Id, *keyEntry.Id}, "api_key_ids": {keyID}, "override_ids": {*ov.Id}}); removed != want {
		t.Errorf("consumer.delete removed %s, want %s", removed, want)
	}
	if v := conRecs[0].IdentityVersion; v == nil || *v <= *ov.Version {
		t.Errorf("consumer.delete identity version %v, want one above %d", v, *ov.Version)
	}

	// Filters and pages.
	if l := auditPage(t, admin, "?actor_sub=sub-editor"); len(l.Items) != 4 {
		t.Errorf("?actor_sub=sub-editor: %d records", len(l.Items))
	}
	if l := auditPage(t, admin, "?entity=upstream&actor_sub=sub-admin"); len(l.Items) != 0 {
		t.Errorf("?entity=upstream&actor_sub=sub-admin: %d records", len(l.Items))
	}
	all := auditPage(t, admin, "?limit=500").Items
	if len(all) != 10 {
		t.Fatalf("%d records in all, want 10", len(all))
	}
	var paged []int64
	for cursor := ""; ; {
		l := auditPage(t, admin, "?limit=4"+cursor)
		for _, r := range l.Items {
			paged = append(paged, r.Id)
		}
		if l.NextCursor == nil {
			break
		}
		cursor = "&cursor=" + *l.NextCursor
	}
	var ids []int64
	for _, r := range all {
		ids = append(ids, r.Id)
	}
	if !slices.Equal(paged, ids) || !slices.IsSortedFunc(ids, func(a, b int64) int { return cmp.Compare(b, a) }) {
		t.Errorf("pages %v, all %v: want the same, newest first", paged, ids)
	}
	mid := all[5].At
	since := url.QueryEscape(mid.Format(time.RFC3339Nano))
	if l := auditPage(t, admin, "?limit=500&since="+since); len(l.Items) != 6 {
		t.Errorf("?since: %d records, want 6", len(l.Items))
	}
	if l := auditPage(t, admin, "?limit=500&until="+since); len(l.Items) != 4 {
		t.Errorf("?until: %d records, want 4", len(l.Items))
	}
	admin.must(400, "GET", "/audit?entity_id=1", nil)
	admin.must(400, "GET", "/audit?since="+since+"&until="+since, nil)
	for _, tok := range []string{editorTok, keyAdminTok, iss.token(t, "v", "v", "viewer"), iss.token(t, "p", "p", "publisher")} {
		c.as(tok).must(403, "GET", "/audit", nil)
	}

	// No token, key hash or password in any record or log line.
	var rows strings.Builder
	r, err := owner.Query(t.Context(), "SELECT row_to_json(a)::text FROM audit_log a")
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var s string
		if err := r.Scan(&s); err != nil {
			t.Fatal(err)
		}
		rows.WriteString(s)
	}
	if r.Err() != nil {
		t.Fatal(r.Err())
	}
	var hash []byte
	if err := owner.QueryRow(t.Context(), "SELECT sha256($1::bytea)", keyHash).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{"key hash (hex)": fmt.Sprintf("%x", hash), "app password": pgAppPassword, "owner password": pgOwnerPassword}
	for name, tok := range map[string]string{"editor": editorTok, "key-admin": keyAdminTok, "admin": adminTok} {
		secrets[name+" token"] = tok[strings.LastIndexByte(tok, '.')+1:]
	}
	for name, s := range secrets {
		if strings.Contains(rows.String(), s) || strings.Contains(logs.String(), s) {
			t.Errorf("the %s appears in an audit record or a log line", name)
		}
	}
	if strings.Contains(rows.String(), `"hash"`) {
		t.Errorf("an audit record has a hash field: %s", rows.String())
	}
}

// TestAuditLogAppendOnly checks that Postgres keeps audit_log append-only
// (SEC-08, ADR-0040): the app role may insert and read but not update,
// delete or truncate (a permission error), and the owner is stopped by the
// triggers.
func TestAuditLogAppendOnly(t *testing.T) {
	db := migratedDatabase(t)
	st := openStore(t, db)
	if _, err := st.CreateConfig(t.Context(), testAudit(), store.Plans, store.ConfigInput{Name: "free", Spec: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	app := pgConnect(t, pgURL(t, "spillway_app", db), pgAppPassword)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	var n int
	if err := app.QueryRow(t.Context(), "SELECT count(*) FROM audit_log").Scan(&n); err != nil || n != 1 {
		t.Fatalf("app role reads %d records, %v", n, err)
	}
	for _, sql := range []string{"UPDATE audit_log SET note = 'x'", "DELETE FROM audit_log", "TRUNCATE audit_log"} {
		_, err := app.Exec(t.Context(), sql)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" {
			t.Errorf("app role, %s: %v, want permission denied (42501)", sql, err)
		}
		_, err = owner.Exec(t.Context(), sql)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "P0001" || pgErr.Message != "audit_log is append-only" {
			t.Errorf("owner, %s: %v, want the trigger's error", sql, err)
		}
	}
	if err := owner.QueryRow(t.Context(), "SELECT count(*) FROM audit_log").Scan(&n); err != nil || n != 1 {
		t.Errorf("%d records after the attempts, %v", n, err)
	}
}

// Compose: Keycloak's dev realm and the compose control plane.

// keycloakToken gets an access token from the compose Keycloak: the
// client-credentials grant for a confidential client, or a test user's
// password grant through the dev-only rbac-tests client.
func keycloakToken(t *testing.T, grant, client, username string) string {
	t.Helper()
	form := url.Values{"grant_type": {grant}, "client_id": {client}, "client_secret": {"dev-only-not-secret"}}
	if client == "spillway-ci" {
		form.Set("client_secret", cmp.Or(os.Getenv("SPILLWAY_CI_CLIENT_SECRET"), "dev-only-not-secret"))
	}
	if username != "" {
		form.Set("username", username)
		form.Set("password", "dev-only-not-secret")
	}
	req, err := http.NewRequestWithContext(t.Context(), "POST", "http://127.0.0.1:8180/auth/realms/spillway/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		t.Fatalf("Keycloak token for %s %s: %d %q %v (a stale realm? docker compose up -d --force-recreate keycloak)", client, username, resp.StatusCode, out.Error, err)
	}
	return out.AccessToken
}

// composeControlPlane is a client of the compose control plane, once it
// has its token keys: a control plane that started before Keycloak answers
// 503 until its next fetch (ADR-0039).
func composeControlPlane(t *testing.T) cpClient {
	t.Helper()
	c := cpClient{t: t, base: "http://127.0.0.1:8080"}
	tok := keycloakToken(t, "client_credentials", "spillway-ci", "")
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(time.Second) {
		a := c.as(tok).call("GET", "/plans?limit=1", nil)
		if a.status != 503 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the control plane has no token keys after 45 s: %s", a.body)
		}
	}
	return c
}

// TestComposeRBAC runs the role matrix against the compose control plane
// with tokens Keycloak issues to the dev realm's users, one per role, and
// to the spillway-ci client's service account (admin) (CP-11, CP-12): the
// realm's role claims reach the control plane's roles.
func TestComposeRBAC(t *testing.T) {
	c := composeControlPlane(t)
	tokens := map[string]string{}
	for _, role := range api.Roles {
		tokens[role] = keycloakToken(t, "password", "rbac-tests", "test-"+role)
	}
	matrix(t, c, tokens)
	matrix(t, c, map[string]string{"admin": keycloakToken(t, "client_credentials", "spillway-ci", "")})
	// The gateway's test client has no control-plane audience.
	c.as(keycloakToken(t, "client_credentials", "gateway-tests", "")).must(401, "GET", "/plans", nil)
}

// TestComposeAuditActor checks that a change in compose is audited with
// the Keycloak user who made it: its fixed ID as sub, its username and its
// roles (CP-11, CP-13).
func TestComposeAuditActor(t *testing.T) {
	c := composeControlPlane(t)
	editor := c.as(keycloakToken(t, "password", "rbac-tests", "test-editor"))
	ci := c.as(keycloakToken(t, "client_credentials", "spillway-ci", ""))
	name := fmt.Sprintf("it-audit-%d", time.Now().UnixNano()%1e9)
	created := editor.must(201, "POST", "/plans", map[string]any{"name": name})
	var p apiv1.Plan
	created.decode(t, &p)
	editor.must(204, "DELETE", fmt.Sprintf("/plans/%d", *p.Id), nil, "If-Match", created.etag())
	l := auditPage(t, ci, fmt.Sprintf("?entity=plan&entity_id=%d", *p.Id))
	if len(l.Items) != 2 {
		t.Fatalf("records %+v", l.Items)
	}
	for _, r := range l.Items {
		if r.Actor.Sub != "0b5e7c1a-5f1d-4c3e-9a00-000000000002" || r.Actor.Name != "test-editor" || !slices.Equal(r.Actor.Roles, []string{"editor"}) {
			t.Errorf("actor %+v, want test-editor's fixed ID, name and role", r.Actor)
		}
	}
	if l.Items[1].RequestId != created.header.Get("X-Request-Id") {
		t.Errorf("request ID %q, want %q", l.Items[1].RequestId, created.header.Get("X-Request-Id"))
	}
	editor.must(403, "GET", "/audit", nil)
}
