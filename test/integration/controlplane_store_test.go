//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
)

// The control plane's store tests run on a Postgres 18.6 testcontainer, the
// compose image, started once for the package; each test gets its own
// database (ADR-0036).

// Dev-only passwords of the testcontainer's roles.
const (
	pgOwnerPassword = "dev-only-not-secret"     //nolint:gosec // G101: dev-only
	pgAppPassword   = "app-dev-only-not-secret" //nolint:gosec // G101: dev-only
)

var pgServer struct {
	once sync.Once
	host string
	port string
	err  error
}

// postgresAddr starts the testcontainer on first use and returns its
// address.
func postgresAddr(t *testing.T) (host, port string) {
	t.Helper()
	pgServer.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		c, err := tcpostgres.Run(ctx, "postgres:18.6",
			tcpostgres.WithDatabase("spillway"), tcpostgres.WithUsername("spillway"), tcpostgres.WithPassword(pgOwnerPassword),
			tcpostgres.BasicWaitStrategies())
		if err != nil {
			pgServer.err = err
			return
		}
		pgServer.host, pgServer.err = c.Host(ctx)
		if pgServer.err == nil {
			p, err := c.MappedPort(ctx, "5432/tcp")
			pgServer.port, pgServer.err = p.Port(), err
		}
	})
	if pgServer.err != nil {
		t.Fatalf("Postgres testcontainer: %v", pgServer.err)
	}
	return pgServer.host, pgServer.port
}

// pgURL is a URL without a password, as the control plane takes them.
func pgURL(t *testing.T, user, database string) string {
	host, port := postgresAddr(t)
	return (&url.URL{Scheme: "postgres", User: url.User(user), Host: host + ":" + port, Path: "/" + database, RawQuery: "sslmode=disable"}).String()
}

// emptyDatabase creates a database for the test and returns its name.
func emptyDatabase(t *testing.T) string {
	t.Helper()
	name := "cp_" + strings.ToLower(rand.Text()[:10])
	conn := pgConnect(t, pgURL(t, "spillway", "spillway"), pgOwnerPassword)
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	return name
}

// migratedDatabase creates a database for the test and migrates it.
func migratedDatabase(t *testing.T) string {
	t.Helper()
	name := emptyDatabase(t)
	if _, err := store.Migrate(t.Context(), pgURL(t, "spillway", name), pgOwnerPassword, pgAppPassword); err != nil {
		t.Fatal(err)
	}
	return name
}

func pgConnect(t *testing.T, dsn, password string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Password = password
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// openStore opens the store as the app role, as `controlplane serve` does.
func openStore(t *testing.T, database string) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), pgURL(t, "spillway_app", database), pgAppPassword)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestControlPlaneMigrate checks `controlplane migrate` (CP-19, ADR-0036):
// it applies every migration once, a second run applies nothing, two runs at
// once do not interleave, `serve` refuses a database it has not migrated,
// and the app role logs in with its password and cannot change the schema.
func TestControlPlaneMigrate(t *testing.T) {
	ctx := t.Context()
	db := emptyDatabase(t)
	if _, err := store.Open(ctx, pgURL(t, "spillway", db), pgOwnerPassword); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("database schema 0 is older than this binary needs (%d): run controlplane migrate", store.SchemaVersion())) {
		t.Errorf("serve on an empty database: %v", err)
	}

	applied, err := store.Migrate(ctx, pgURL(t, "spillway", db), pgOwnerPassword, pgAppPassword)
	if err != nil {
		t.Fatal(err)
	}
	if want := store.SchemaVersion(); len(applied) != int(want) || applied[len(applied)-1] != want {
		t.Errorf("first run applied %v, want 1 … %d", applied, want)
	}
	if again, err := store.Migrate(ctx, pgURL(t, "spillway", db), pgOwnerPassword, pgAppPassword); err != nil || len(again) != 0 {
		t.Errorf("second run applied %v, %v; want none", again, err)
	}

	// Two runs at once: the session lock makes one wait for the other.
	db2 := emptyDatabase(t)
	var wg sync.WaitGroup
	results := make([][]int64, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			results[i], errs[i] = store.Migrate(ctx, pgURL(t, "spillway", db2), pgOwnerPassword, pgAppPassword)
		})
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || len(results[0])+len(results[1]) != int(store.SchemaVersion()) {
		t.Errorf("concurrent runs: applied %v and %v, errors %v and %v", results[0], results[1], errs[0], errs[1])
	}

	openStore(t, db) // the app role logs in with the password migrate set
	if _, err := store.Open(ctx, pgURL(t, "spillway_app", db), "wrong-dev-only-not-secret"); err == nil {
		t.Error("the app role logged in with a wrong password")
	}
	app := pgConnect(t, pgURL(t, "spillway_app", db), pgAppPassword)
	for _, stmt := range []string{"CREATE TABLE x (id int)", "DROP TABLE tenants", "ALTER TABLE tenants ADD COLUMN x int"} {
		_, err := app.Exec(ctx, stmt)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "42501" && pgErr.Code != "42P01" {
			t.Errorf("app role: %s: %v, want a permission error", stmt, err)
		}
	}
	var verifier string
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	if err := owner.QueryRow(ctx, "SELECT rolpassword FROM pg_authid WHERE rolname = 'spillway_app'").Scan(&verifier); err == nil {
		if !strings.HasPrefix(verifier, "SCRAM-SHA-256$4096:") || strings.Contains(verifier, pgAppPassword) {
			t.Errorf("the app role's stored password is not a SCRAM verifier: %.20s…", verifier)
		}
	} // pg_authid needs a superuser; the testcontainer's owner is one
}

// TestControlPlaneQueriesUseIndexes runs EXPLAIN on every named sqlc query
// with sequential scans off, so the planner picks an index whenever one can
// serve the query however small the tables, and fails on any Seq Scan, and
// on any index scan without an index condition (a whole index read in
// order), outside the reads ADR-0036 lists as intended (ADR-0036).
func TestControlPlaneQueriesUseIndexes(t *testing.T) {
	// Query name → why it may read a whole table or index. The whole-table
	// reads of export and the identity snapshot arrive with their tasks.
	wholeTable := map[string]string{
		"LatestConfigVersion": "the first row of a backward scan of config_versions_pkey",
		"AllUpstreams":        "validate, publish and rollback read the whole working set",
		"AllEndpoints":        "validate, publish and rollback read the whole working set",
		"AllRoutes":           "validate, publish and rollback read the whole working set",
		"AllPolicies":         "validate, publish and rollback read the whole working set",
		"AllPlans":            "validate, publish and rollback read the whole working set",
		"DeleteAllUpstreams":  "a rollback replaces the working tables",
		"DeleteAllRoutes":     "a rollback replaces the working tables",
		"DeleteAllPolicies":   "a rollback replaces the working tables",
		"DeleteAllPlans":      "a rollback replaces the working tables",
	}
	queries := sqlcQueries(t)
	if len(queries) < 40 {
		t.Fatalf("found %d sqlc queries", len(queries))
	}
	conn := pgConnect(t, pgURL(t, "spillway_app", migratedDatabase(t)), pgAppPassword)
	if _, err := conn.Exec(t.Context(), "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	for name, sql := range queries {
		// The simple protocol sends the $n placeholders to the server as
		// they are, which GENERIC_PLAN plans for.
		results, err := conn.PgConn().Exec(t.Context(), "EXPLAIN (GENERIC_PLAN) "+sql).ReadAll()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var plan []string
		for _, row := range results[0].Rows {
			plan = append(plan, string(row[0]))
		}
		if _, ok := wholeTable[name]; !ok && !indexed(plan) {
			t.Errorf("%s reads a whole table or index:\n%s", name, strings.Join(plan, "\n"))
		}
	}
}

// indexed reports whether a plan has no Seq Scan and every index scan in it
// has an index condition.
func indexed(plan []string) bool {
	for i, line := range plan {
		if strings.Contains(line, "Seq Scan") {
			return false
		}
		if !strings.Contains(line, "Index Scan") && !strings.Contains(line, "Index Only Scan") {
			continue
		}
		cond := false
		for _, detail := range plan[i+1:] {
			if strings.Contains(detail, "->") {
				break
			}
			cond = cond || strings.Contains(detail, "Index Cond:")
		}
		if !cond {
			return false
		}
	}
	return true
}

// sqlcQueries returns every query that sqlc generated, by name, from the
// generated Go files.
func sqlcQueries(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("../../internal/controlplane/store/db/*.sql.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("generated query files: %v %v", files, err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		src, err := os.ReadFile(f) //nolint:gosec // G304: a generated file of this repo
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || len(vs.Values) != 1 {
				return true
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sql, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(sql, "-- name: ") {
				return true
			}
			out[strings.Fields(sql)[2]] = sql
			return true
		})
	}
	return out
}

// TestControlPlaneVersions checks the two counters every change bumps
// first (ADR-0037): config edits write the new working-set revision as the
// row's version, identity changes the new identity version; both only grow,
// and a failed change bumps neither.
func TestControlPlaneVersions(t *testing.T) {
	ctx := t.Context()
	db := migratedDatabase(t)
	st := openStore(t, db)
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	counter := func(sql string) int64 {
		var v int64
		if err := owner.QueryRow(ctx, sql).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	revision := func() int64 { return counter("SELECT revision FROM config_working_set") }
	identity := func() int64 { return counter("SELECT version FROM identity_state") }
	if revision() != 1 || identity() != 1 {
		t.Fatalf("seeded revision %d, identity version %d; want 1 and 1", revision(), identity())
	}
	a := testAudit()
	plan, err := st.CreateConfig(ctx, a, store.Plans, store.ConfigInput{Name: "free", Spec: []byte("{}")})
	if err != nil || plan.Version != 2 || revision() != 2 {
		t.Fatalf("plan version %d, revision %d, %v; want 2", plan.Version, revision(), err)
	}
	if _, err := st.CreateConfig(ctx, a, store.Plans, store.ConfigInput{Name: "free", Spec: []byte("{}")}); !isConflict(err) {
		t.Errorf("a second plan named free: %v, want a conflict", err)
	}
	if _, err := st.UpdateConfig(ctx, a, store.Plans, plan.ID, 1, store.ConfigInput{Name: "free", Spec: []byte("{}")}); !isPrecondition(err, 2) {
		t.Errorf("a stale If-Match: %v, want a precondition error naming version 2", err)
	}
	if revision() != 2 {
		t.Errorf("failed edits bumped the revision to %d", revision())
	}
	if err := st.DeleteConfig(ctx, a, store.Plans, plan.ID, 2); err != nil || revision() != 3 {
		t.Errorf("delete: %v, revision %d", err, revision())
	}
	if _, err := st.GetConfig(ctx, store.Plans, plan.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted plan: %v", err)
	}

	if _, err := st.CreateTenant(ctx, a, tenantParams("acme", 1)); err == nil || identity() != 1 {
		t.Errorf("a tenant before any published config: %v, identity version %d; want a refusal at 1", err, identity())
	}
	publish(t, owner, `{"plans": [{"id": 7, "name": "free"}], "policies": [{"id": 9, "name": "p"}]}`)
	tenant, err := st.CreateTenant(ctx, a, tenantParams("acme", 7))
	if err != nil || tenant.Version != 2 || identity() != 2 {
		t.Fatalf("tenant version %d, identity version %d, %v; want 2", tenant.Version, identity(), err)
	}
	if _, err := st.CreateTenant(ctx, a, tenantParams("other", 8)); err == nil || !strings.Contains(err.Error(), "plan 8 is not in the published config version 1") {
		t.Errorf("a tenant on an unpublished plan: %v", err)
	}
	if identity() != 2 {
		t.Errorf("failed identity changes bumped the version to %d", identity())
	}
}

func publish(t *testing.T, owner *pgx.Conn, snapshot string) {
	t.Helper()
	_, err := owner.Exec(t.Context(), `INSERT INTO config_versions (version, published_at, published_by, note, kind, working_set_revision, schema_major, schema_minor, snapshot)
		SELECT coalesce(max(version), 0) + 1, now(), 'test', 'test', 'publish', 1, 1, 0, $1 FROM config_versions`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
}

func isConflict(err error) bool {
	_, ok := errors.AsType[*store.ConflictError](err)
	return ok
}

func isPrecondition(err error, current int64) bool {
	e, ok := errors.AsType[*store.PreconditionError](err)
	return ok && e.Current == current
}

// testAudit is an audit context for changes made through the store.
func testAudit() store.Audit {
	return store.Audit{Actor: store.Actor{Sub: "test", Name: "test", Roles: []string{"admin"}}, RequestID: "test", Show: func(r any) (any, error) { return r, nil }}
}

func tenantParams(name string, plan int64) db.CreateTenantParams {
	return db.CreateTenantParams{Name: name, PlanID: plan, Status: "active"}
}
