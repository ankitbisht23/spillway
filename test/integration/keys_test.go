//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// Managed API keys and the identity path (CP-15 - CP-17, CP-20, GW-17,
// ADR-0037, ADR-0039). Keys are made at run time; none is ever written to
// a file of this repository (the gitleaks rule).

// keyParts are what must never show outside the issue or rotate answer:
// the key and its secret; and outside the store and the identity
// snapshot, its stored hash too.
type keyParts struct{ key, secret, hash string }

func partsOf(t *testing.T, key string) keyParts {
	t.Helper()
	_, sec, ok := apikey.Parse(key)
	if !ok {
		t.Fatalf("a key of length %d does not follow the grammar", len(key))
	}
	h := apikey.HashSecret(sec)
	return keyParts{key, sec, hex.EncodeToString(h[:])}
}

// noKeyIn fails the test if out holds any of the keys' parts; withHash
// false allows the stored hash (the identity snapshot and export).
func noKeyIn(t *testing.T, what, out string, withHash bool, keys ...keyParts) {
	t.Helper()
	for _, k := range keys {
		if strings.Contains(out, k.key) || strings.Contains(out, k.secret) {
			t.Errorf("%s holds a key or its secret", what)
		}
		if withHash && strings.Contains(out, k.hash) {
			t.Errorf("%s holds a key's hash", what)
		}
	}
}

// keyEnv is a control plane on a fresh database with a published config
// (plan free), tenant acme and consumer bot.
type keyEnv struct {
	r        roles
	log      *syncBuffer
	owner    *pgx.Conn
	consumer int64
}

func newKeyEnv(t *testing.T) keyEnv {
	t.Helper()
	log := &syncBuffer{}
	c, iss, db := startControlPlane(t, log)
	e := keyEnv{r: newRoles(t, c, iss), log: log, owner: pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)}
	plan, _ := created(t, e.r.editor, "/plans", map[string]any{"name": "free"})
	_, tag := validate(t, e.r.viewer)
	e.r.publisher.must(200, "POST", "/config/publish", note("v1"), "If-Match", tag)
	tenant, _ := created(t, e.r.admin, "/tenants", map[string]any{"name": "acme", "plan_id": plan})
	e.consumer, _ = created(t, e.r.keyAdmin, "/consumers", map[string]any{"tenant_id": tenant, "name": "bot"})
	return e
}

func (e keyEnv) identityVersion(t *testing.T) int64 {
	t.Helper()
	var v int64
	if err := e.owner.QueryRow(t.Context(), "SELECT version FROM identity_state").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// issue issues a key for the consumer and checks the answer.
func (e keyEnv) issue(t *testing.T, body any) apiv1.IssuedApiKey {
	t.Helper()
	a := e.r.keyAdmin.must(201, "POST", fmt.Sprintf("/consumers/%d/keys", e.consumer), body)
	if a.header.Get("Cache-Control") != "no-store" {
		t.Errorf("issue: Cache-Control %q, want no-store", a.header.Get("Cache-Control"))
	}
	var k apiv1.IssuedApiKey
	a.decode(t, &k)
	return k
}

var keyGrammar = regexp.MustCompile("^" + apikey.Pattern + "$")

// TestControlPlaneKeys checks issue, list, rotate and revoke (CP-15 -
// CP-17, ADR-0039): the key's format, shown once with no-store, stored as
// prefix and SHA-256 of the secret; rotation's grace and its limits;
// revocation, idempotent; each change one identity version and one audit
// record; and no key, secret or hash in any log line, audit record, error
// body or listing.
func TestControlPlaneKeys(t *testing.T) {
	e := newKeyEnv(t)
	keysPath := fmt.Sprintf("/consumers/%d/keys", e.consumer)
	var keys []keyParts
	var bodies strings.Builder // every answer but the issue and rotate ones

	// Issue, with no body and with an empty one.
	v0 := e.identityVersion(t)
	k1 := e.issue(t, nil)
	k2 := e.issue(t, map[string]any{})
	for _, k := range []apiv1.IssuedApiKey{k1, k2} {
		keys = append(keys, partsOf(t, k.Key))
		if !keyGrammar.MatchString(k.Key) || k.Key[4:12] != k.Prefix || k.ConsumerId != e.consumer || k.ExpiresAt != nil || k.RotatedFrom != nil {
			t.Errorf("issued %+v (key of length %d)", k, len(k.Key))
		}
	}
	if k1.Key == k2.Key || k1.Prefix == k2.Prefix {
		t.Error("two issues gave the same key or prefix")
	}
	if got := e.identityVersion(t); got != v0+2 || k2.Version != v0+2 {
		t.Errorf("identity version %d, key version %d; want %d", got, k2.Version, v0+2)
	}
	// Stored: the prefix and SHA-256 of the 43 secret characters.
	var prefix string
	var hash []byte
	if err := e.owner.QueryRow(t.Context(), "SELECT prefix, hash FROM api_keys WHERE id = $1", k1.Id).Scan(&prefix, &hash); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte(keys[0].secret)); prefix != k1.Prefix || !bytes.Equal(hash, sum[:]) {
		t.Error("api_keys does not hold the prefix and the secret's SHA-256")
	}

	// Listed: prefix and fields only, never again the key.
	list := e.r.viewer.must(200, "GET", keysPath, nil)
	bodies.Write(list.body)
	var l apiv1.ApiKeyList
	list.decode(t, &l)
	if len(l.Items) != 2 || l.Items[0].Prefix != k1.Prefix || strings.Contains(string(list.body), `"key"`) || strings.Contains(string(list.body), `"hash"`) {
		t.Errorf("listing %s", list.body)
	}

	// Expiry at issue must be in the future; unknown consumers are 404;
	// a viewer may not issue.
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	k3 := e.issue(t, map[string]any{"expires_at": future})
	keys = append(keys, partsOf(t, k3.Key))
	if k3.ExpiresAt == nil || !k3.ExpiresAt.Equal(future) {
		t.Errorf("expires_at %v, want %v", k3.ExpiresAt, future)
	}
	for _, a := range []cpAnswer{
		e.r.keyAdmin.call("POST", keysPath, map[string]any{"expires_at": time.Now().Add(-time.Minute)}),
		e.r.keyAdmin.call("POST", "/consumers/999999/keys", nil),
		e.r.viewer.call("POST", keysPath, nil),
		e.r.keyAdmin.call("POST", keysPath, map[string]any{"key": "x"}),
	} {
		bodies.Write(a.body)
		if a.status != 422 && a.status != 404 && a.status != 403 && a.status != 400 {
			t.Errorf("a refused issue answered %d %s", a.status, a.body)
		}
	}

	// Rotate with the default grace: the old key expires 24 h from now.
	rotate := func(id int64, body any) apiv1.KeyRotation {
		t.Helper()
		a := e.r.keyAdmin.must(200, "POST", fmt.Sprintf("/keys/%d/rotate", id), body)
		if a.header.Get("Cache-Control") != "no-store" {
			t.Errorf("rotate: Cache-Control %q, want no-store", a.header.Get("Cache-Control"))
		}
		var r apiv1.KeyRotation
		a.decode(t, &r)
		keys = append(keys, partsOf(t, r.New.Key))
		if r.New.RotatedFrom == nil || *r.New.RotatedFrom != id || r.New.ConsumerId != e.consumer || r.New.ExpiresAt != nil || r.Old.Id != id {
			t.Errorf("rotation %+v", r)
		}
		return r
	}
	v := e.identityVersion(t)
	r1 := rotate(k1.Id, map[string]any{"note": "yearly rotation"})
	if d := r1.Old.ExpiresAt.Sub(r1.Old.UpdatedAt) - 24*time.Hour; r1.Old.ExpiresAt == nil || d < -time.Second || d > time.Second {
		t.Errorf("old key expires at %v, updated at %v; want 24 h later", r1.Old.ExpiresAt, r1.Old.UpdatedAt)
	}
	if got := e.identityVersion(t); got != v+1 || r1.Old.Version != v+1 || r1.New.Version != v+1 {
		t.Errorf("rotation: identity version %d, versions %d and %d; want one change, %d", got, r1.Old.Version, r1.New.Version, v+1)
	}
	// A key that expires sooner keeps its expiry.
	if r := rotate(k3.Id, nil); !r.Old.ExpiresAt.Equal(future) {
		t.Errorf("k3 rotated: expires_at %v, want its own %v", r.Old.ExpiresAt, future)
	}
	// Grace 0s ends the old key at once; it cannot be rotated again.
	r2 := rotate(k2.Id, map[string]any{"grace": "0s"})
	if r2.Old.ExpiresAt.After(time.Now().Add(time.Second)) {
		t.Errorf("grace 0s: expires_at %v", r2.Old.ExpiresAt)
	}
	for _, tt := range []struct {
		id   int64
		body any
		want int
	}{
		{k2.Id, nil, 409},
		{r1.New.Id, map[string]any{"grace": "169h"}, 422},
		{r1.New.Id, map[string]any{"grace": "-1s"}, 422},
		{r1.New.Id, map[string]any{"grace": "a day"}, 422},
		{r1.New.Id, map[string]any{"note": ""}, 422},
		{999999, nil, 404},
	} {
		a := e.r.keyAdmin.call("POST", fmt.Sprintf("/keys/%d/rotate", tt.id), tt.body)
		bodies.Write(a.body)
		if a.status != tt.want {
			t.Errorf("rotate %d %v: %d %s, want %d", tt.id, tt.body, a.status, a.body, tt.want)
		}
	}

	// Revoke: one identity change; again, nothing.
	v = e.identityVersion(t)
	rev := e.r.keyAdmin.must(200, "POST", fmt.Sprintf("/keys/%d/revoke", r1.New.Id), map[string]any{"note": "leaked"})
	bodies.Write(rev.body)
	var revoked apiv1.ApiKey
	rev.decode(t, &revoked)
	if revoked.RevokedAt == nil || revoked.Version != v+1 || e.identityVersion(t) != v+1 {
		t.Errorf("revoked %+v, identity version %d; want revoked at version %d", revoked, e.identityVersion(t), v+1)
	}
	records := len(auditPage(t, e.r.admin, "?entity=key&limit=500").Items)
	again := e.r.keyAdmin.must(200, "POST", fmt.Sprintf("/keys/%d/revoke", r1.New.Id), nil)
	bodies.Write(again.body)
	var revokedAgain apiv1.ApiKey
	again.decode(t, &revokedAgain)
	if !revokedAgain.RevokedAt.Equal(*revoked.RevokedAt) || revokedAgain.Version != revoked.Version || e.identityVersion(t) != v+1 ||
		len(auditPage(t, e.r.admin, "?entity=key&limit=500").Items) != records {
		t.Error("revoking a revoked key changed something")
	}
	for _, a := range []cpAnswer{
		e.r.keyAdmin.call("POST", fmt.Sprintf("/keys/%d/rotate", r1.New.Id), nil),
		e.r.keyAdmin.call("POST", "/keys/999999/revoke", nil),
		e.r.viewer.call("POST", fmt.Sprintf("/keys/%d/revoke", k1.Id), nil),
	} {
		bodies.Write(a.body)
		if a.status != 409 && a.status != 404 && a.status != 403 {
			t.Errorf("refused: %d %s", a.status, a.body)
		}
	}

	// The audit log: one record per change, with its identity version and
	// note; a rotation's diff names the new key.
	var actions []string
	byAction := map[string]apiv1.AuditRecord{}
	for _, rec := range auditPage(t, e.r.admin, "?entity=key&limit=500").Items {
		actions = append(actions, rec.Action)
		byAction[rec.Action] = rec
		if rec.IdentityVersion == nil {
			t.Errorf("%s record without an identity version", rec.Action)
		}
	}
	slices.Sort(actions)
	if want := []string{"key.issue", "key.issue", "key.issue", "key.revoke", "key.rotate", "key.rotate", "key.rotate"}; !slices.Equal(actions, want) {
		t.Errorf("audit actions %v, want %v", actions, want)
	}
	if rec := byAction["key.revoke"]; rec.Note == nil || *rec.Note != "leaked" {
		t.Errorf("revoke record note %v", rec.Note)
	}
	if diff := mustJSON(t, byAction["key.rotate"].Diff); !strings.Contains(diff, `"new_key":{`) || !strings.Contains(diff, `"before":{`) {
		t.Errorf("rotate record diff %s", diff)
	}

	// No key, secret or hash anywhere but the issue and rotate answers
	// (the identity export, admin only, holds hashes by design).
	var audit string
	if err := e.owner.QueryRow(t.Context(), "SELECT coalesce(string_agg(row_to_json(a)::text, E'\\n'), '') FROM audit_log a").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	identity := e.r.admin.must(200, "GET", "/identity/export", nil)
	noKeyIn(t, "the audit log", audit, true, keys...)
	noKeyIn(t, "an API answer", bodies.String(), true, keys...)
	noKeyIn(t, "the control plane's log", e.log.String(), true, keys...)
	noKeyIn(t, "the identity export", string(identity.body), false, keys...)
	if !strings.Contains(string(identity.body), keys[len(keys)-2].hash) || strings.Contains(string(identity.body), r1.New.Prefix) {
		t.Error("the identity export does not list the live keys' hashes alone (revoked keys stay out)")
	}
}

// TestIdentityVersionsConcurrent issues, rotates and revokes keys from
// many callers at once (CP-20, ADR-0037): every change gets its own
// identity version, versions have no gaps, and they follow commit order,
// which the audit records' order shows.
func TestIdentityVersionsConcurrent(t *testing.T) {
	e := newKeyEnv(t)
	v0 := e.identityVersion(t)
	const n = 24
	var mu sync.Mutex
	var ids, versions []int64
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			a := e.r.keyAdmin.call("POST", fmt.Sprintf("/consumers/%d/keys", e.consumer), nil)
			var k apiv1.IssuedApiKey
			if a.status != 201 || json.Unmarshal(a.body, &k) != nil {
				t.Errorf("issue: %d", a.status)
				return
			}
			mu.Lock()
			ids, versions = append(ids, k.Id), append(versions, k.Version)
			mu.Unlock()
		})
	}
	wg.Wait()
	for i, id := range ids {
		wg.Go(func() {
			op, want := "revoke", 200
			if i%2 == 0 {
				op = "rotate"
			}
			a := e.r.keyAdmin.call("POST", fmt.Sprintf("/keys/%d/%s", id, op), nil)
			var v struct {
				Version int64
				New     struct{ Version int64 }
			}
			if a.status != want || json.Unmarshal(a.body, &v) != nil {
				t.Errorf("%s: %d", op, a.status)
				return
			}
			mu.Lock()
			versions = append(versions, max(v.Version, v.New.Version))
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(versions)
	want := make([]int64, 2*n)
	for i := range want {
		want[i] = v0 + 1 + int64(i)
	}
	if !slices.Equal(versions, want) || e.identityVersion(t) != v0+2*n {
		t.Errorf("versions %v, identity version %d; want %v", versions, e.identityVersion(t), want)
	}
	// Audit records in insertion order carry the versions in order: each
	// change wrote its record under the identity version's row lock.
	rows, err := e.owner.Query(t.Context(), "SELECT identity_version FROM audit_log WHERE identity_version > $1 ORDER BY id", v0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("audit records' identity versions in order %v, want %v", got, want)
	}
}

// TestConfigRollbackKeepsKeys: a config publish and rollback never touch
// identity (CP-04, CP-20): keys issued and revoked before stay as they
// were, with the same identity version.
func TestConfigRollbackKeepsKeys(t *testing.T) {
	e := newKeyEnv(t)
	live := e.issue(t, nil)
	gone := e.issue(t, nil)
	e.r.keyAdmin.must(200, "POST", fmt.Sprintf("/keys/%d/revoke", gone.Id), nil)
	before := identityState(t, e.owner)

	created(t, e.r.editor, "/plans", map[string]any{"name": "pro"})
	_, tag := validate(t, e.r.viewer)
	e.r.publisher.must(200, "POST", "/config/publish", note("v2"), "If-Match", tag)
	_, tag = validate(t, e.r.viewer)
	e.r.publisher.must(200, "POST", "/config/versions/1/rollback", note("back to v1"), "If-Match", tag)
	if after := identityState(t, e.owner); after != before {
		t.Errorf("identity changed across a publish and a rollback:\n%s\n---\n%s", before, after)
	}
	var l apiv1.ApiKeyList
	e.r.viewer.must(200, "GET", fmt.Sprintf("/consumers/%d/keys", e.consumer), nil).decode(t, &l)
	if len(l.Items) != 2 || l.Items[0].Id != live.Id || l.Items[0].RevokedAt != nil || l.Items[1].RevokedAt == nil {
		t.Errorf("keys after the rollback %+v", l.Items)
	}
}

// runChaosRevocation runs "chaos revocation" (cmd/chaos) with the token on
// stdin and returns its report and its PASS/FAIL line.
func runChaosRevocation(t *testing.T, cmd *exec.Cmd, token string) (chaosRevocationReport, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(token+"\n"), &out, &errOut
	err := cmd.Run()
	var rep chaosRevocationReport
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("chaos revocation: %v %v\n%s\n%s", err, jerr, out.String(), errOut.String())
	}
	var line string // its verdict; docker compose run adds lines of its own
	for l := range strings.Lines(errOut.String()) {
		if strings.HasPrefix(l, "PASS ") || strings.HasPrefix(l, "FAIL ") {
			line = strings.TrimSpace(l)
		}
	}
	if err != nil || !rep.Pass {
		t.Errorf("chaos revocation failed: %v\n%s\n%s", err, line, out.String())
	}
	return rep, line
}

type chaosRevocationReport struct {
	Rounds []struct {
		KeyID      int64              `json:"key_id"`
		RejectedMS map[string]float64 `json:"rejected_ms"`
		MaxMS      float64            `json:"max_ms"`
		Error      string             `json:"error"`
	} `json:"rounds"`
	P50MS float64 `json:"p50_ms"`
	MaxMS float64 `json:"max_ms"`
	Pass  bool    `json:"pass"`
}

// TestManagedKeys runs managed keys end to end with host processes (CP-15
// - CP-17, CP-20, GW-17, NFR-09): five managed gateways, gateway-1 …
// gateway-5, resolve keys the control plane issues; a revoked key is
// rejected on all five in < 5 s (chaos revocation, 10 rounds, every
// sample); identity applies with no config change; a rotated key works
// for its grace and stops on each gateway's clock with no identity change;
// and no key, secret or hash shows in any process log, nor key or secret
// in the on-disk snapshots. Every wait allows 30 s, far above the 820 ms
// host stalls of ADR-0034; the 5 s rule is NFR-09's own.
func TestManagedKeys(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", db, grpcAddr, iss)
	base := publishRoute(t, cp.rest, upstreamAnswering(t, "ok"), "base")
	tenant, _ := created(t, cp.rest, "/tenants", map[string]any{"name": "acme", "plan_id": planID(t, cp.rest)})
	consumer, _ := created(t, cp.rest, "/consumers", map[string]any{"tenant_id": tenant, "name": "bot"})
	var gws []*hostGW
	var dirs, addrs []string
	for i := range 5 {
		dir := t.TempDir()
		g := startManaged(t, fmt.Sprintf("gateway-%d", i+1), grpcAddr, dir)
		gws, dirs, addrs = append(gws, g), append(dirs, dir), append(addrs, strings.TrimPrefix(g.url, "http://"))
	}
	for _, g := range gws {
		g.waitVersion(t, int(base))
	}
	answers := func(key string) []int {
		var codes []int
		for _, g := range gws {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.url+"/r", nil)
			req.Header.Set("X-API-Key", key)
			code := 0
			if resp, err := http.DefaultClient.Do(req); err == nil {
				code = resp.StatusCode
				_ = resp.Body.Close()
			}
			cancel()
			codes = append(codes, code)
		}
		return codes
	}
	all := func(codes []int, want int) bool {
		return !slices.ContainsFunc(codes, func(c int) bool { return c != want })
	}

	// Revocation on all five, measured by the chaos sender.
	cmd := exec.CommandContext(t.Context(), binary(t, "chaos"), "revocation", //nolint:gosec // G204: a binary built by binary
		"-controlplane", cp.rest.base+"/api/v1", "-consumer", fmt.Sprint(consumer), "-gateways", strings.Join(addrs, ","), "-path", "/r", "-rounds", "10")
	rep, line := runChaosRevocation(t, cmd, cp.rest.token)
	t.Logf("host processes, 5 gateways: %s", line)
	if len(rep.Rounds) != 10 {
		t.Errorf("%d rounds, want 10", len(rep.Rounds))
	}
	for _, g := range gws {
		if g.metric("gw_config_version") != float64(base) {
			t.Errorf("%s: config version %v; identity must apply with no config change", g.name, g.metric("gw_config_version"))
		}
	}

	// A key works, and is in each on-disk snapshot as prefix and hash.
	var k apiv1.IssuedApiKey
	cp.rest.must(201, "POST", fmt.Sprintf("/consumers/%d/keys", consumer), nil).decode(t, &k)
	old := partsOf(t, k.Key)
	eventually(t, "the new key works on all five gateways", func() bool { return all(answers(k.Key), 200) })
	for i, dir := range dirs {
		eventually(t, fmt.Sprintf("gateway-%d's snapshot holds the key's hash", i+1), func() bool {
			b, err := os.ReadFile(filepath.Join(dir, "snapshot.pb")) //nolint:gosec // G304: the test's own directory
			return err == nil && bytes.Contains(b, []byte(old.hash))
		})
	}

	// Rotation with a 5 s grace: both keys work, then the old one stops on
	// every gateway's clock with no identity change.
	var rot apiv1.KeyRotation
	rotated := time.Now()
	cp.rest.must(200, "POST", fmt.Sprintf("/keys/%d/rotate", k.Id), map[string]any{"grace": "5s"}).decode(t, &rot)
	newKey := partsOf(t, rot.New.Key)
	eventually(t, "the rotated key works on all five gateways", func() bool { return all(answers(newKey.key), 200) })
	if since := time.Since(rotated); since < 4*time.Second {
		if codes := answers(old.key); !all(codes, 200) {
			t.Errorf("%v after the rotation, the old key answers %v; want 200 during the grace", since, codes)
		}
	}
	iv := gws[0].metric("gw_identity_version")
	eventually(t, "the old key rejected on all five gateways", func() bool { return all(answers(old.key), 401) })
	if since := time.Since(rotated); since < 4*time.Second {
		t.Errorf("the old key stopped %v after the rotation, before its 5 s grace", since)
	}
	if codes := answers(newKey.key); !all(codes, 200) {
		t.Errorf("the new key answers %v after the old one's grace", codes)
	}
	for _, g := range gws {
		if g.metric("gw_identity_version") != iv {
			t.Errorf("%s: identity version %v, want %v: expiry needs no identity change", g.name, g.metric("gw_identity_version"), iv)
		}
	}

	// No key material in any process log, nor key or secret on disk.
	cp.rest.must(200, "POST", fmt.Sprintf("/keys/%d/revoke", rot.New.Id), nil)
	for _, g := range gws {
		g.stop(syscall.SIGTERM)
		noKeyIn(t, g.name+"'s log", g.out.String(), true, old, newKey)
	}
	cp.stop(syscall.SIGTERM)
	noKeyIn(t, "the control plane's log", cp.out.String(), true, old, newKey)
	for i, dir := range dirs {
		b, err := os.ReadFile(filepath.Join(dir, "snapshot.pb")) //nolint:gosec // G304: the test's own directory
		if err != nil {
			t.Fatal(err)
		}
		noKeyIn(t, fmt.Sprintf("gateway-%d's on-disk snapshot", i+1), string(b), false, old, newKey)
	}
}

// TestComposeRevocation measures key revocation on the compose gateways
// gateway-1 … gateway-5 in managed mode (--profile replicas), as make
// verify-m5's rule (b) will (NFR-09, ADR-0042): chaos revocation runs in
// the compose network, 20 rounds, every sample < 5 s. The tenant and
// consumer it makes are deleted afterwards, with their keys, so later
// imports of other configs can still publish.
func TestComposeRevocation(t *testing.T) {
	c := composeControlPlane(t)
	token := keycloakToken(t, "client_credentials", "spillway-ci", "")
	c = c.as(token)
	compose(t, nil, "build", "--quiet", "chaos")

	// The compose config plus plan it-keys, published.
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", "gateway", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("plans:\n  - name: it-keys\n")...)
	_, tag := exportYAML(t, c, "")
	importYAML(t, c, 200, data, tag, "?note=TestComposeRevocation")
	_, tag = validate(t, c)
	if a := c.call("POST", "/config/publish", note("TestComposeRevocation: the compose config and plan it-keys"), "If-Match", tag); a.status != 200 && a.status != 409 {
		t.Fatalf("publish: %d %s", a.status, a.body)
	}
	plan, _ := lookup(t, c, "/plans", "it-keys")
	removeTestTenants(t, c, "it-keys-") // left by a run that did not finish
	name := fmt.Sprintf("it-keys-%d", time.Now().UnixNano()%1e9)
	tenant, _ := created(t, c, "/tenants", map[string]any{"name": name, "plan_id": plan})
	consumer, _ := created(t, c, "/consumers", map[string]any{"tenant_id": tenant, "name": name})
	t.Cleanup(func() { removeTestTenants(t, c, "it-keys-") })
	composeManaged(t)
	var cons apiv1.Consumer
	c.must(200, "GET", fmt.Sprintf("/consumers/%d", consumer), nil).decode(t, &cons)
	eventually(t, "gateway-1 … gateway-5 live with the consumer's identity version", func() bool {
		n := 0
		for _, r := range listReplicas(t, c, "").Items {
			if slices.Contains(composeGateways, r.Id) && r.Live && r.WatchConnected && r.IdentityVersion >= *cons.Version {
				n++
			}
		}
		return n == len(composeGateways)
	})

	full := []string{"compose", "--env-file", ".env.example"}
	if _, err := os.Stat(filepath.Join("..", "..", ".env.local")); err == nil {
		full = append(full, "--env-file", ".env.local")
	}
	cmd := exec.CommandContext(t.Context(), "docker", append(full, "--profile", "harness", "run", "--rm", "-T", "chaos", //nolint:gosec // G204: fixed docker compose commands
		"revocation", "-consumer", fmt.Sprint(consumer), "-rounds", "20")...)
	cmd.Dir = filepath.Join("..", "..")
	rep, line := runChaosRevocation(t, cmd, token)
	t.Logf("compose, gateway-1 … gateway-5 managed: %s", line)
	var samples []string
	for _, r := range rep.Rounds {
		samples = append(samples, fmt.Sprintf("%.0f", r.MaxMS))
	}
	t.Logf("samples (ms, latest replica's first 401 after the revoke was sent): %s", strings.Join(samples, " "))
	if len(rep.Rounds) != 20 {
		t.Errorf("%d rounds, want 20", len(rep.Rounds))
	}
}

// removeTestTenants deletes a compose test's tenants, those whose names
// start with prefix (it-keys-…, it-jwt-…), and their consumers, whose keys
// go with them, from the compose control plane. It runs its own requests,
// as a cleanup's t.Context() is done.
func removeTestTenants(t *testing.T, c cpClient, prefix string) {
	t.Helper()
	call := func(method, path, ifMatch string, out any) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v1"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var tenants apiv1.TenantList
	call("GET", "/tenants?limit=500", "", &tenants)
	for _, tn := range tenants.Items {
		if !strings.HasPrefix(tn.Name, prefix) {
			continue
		}
		var consumers apiv1.ConsumerList
		call("GET", fmt.Sprintf("/consumers?limit=500&tenant_id=%d", *tn.Id), "", &consumers)
		for _, cn := range consumers.Items {
			call("DELETE", fmt.Sprintf("/consumers/%d", *cn.Id), fmt.Sprintf(`"%d"`, *cn.Version), nil)
		}
		var cur apiv1.Tenant
		call("GET", fmt.Sprintf("/tenants/%d", *tn.Id), "", &cur)
		call("DELETE", fmt.Sprintf("/tenants/%d", *tn.Id), fmt.Sprintf(`"%d"`, *cur.Version), nil)
	}
}
