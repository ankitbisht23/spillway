package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/secret"
)

const (
	testSecret = "dev-only-test-secret-value"    //nolint:gosec // G101: a test value
	testToken  = "test-token-dev-only-not-a-jwt" //nolint:gosec // G101: a test value
)

// fakeCP is the part of the control plane's REST API the seed uses, with
// a token endpoint at /token.
type fakeCP struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	working   []byte // the working set's YAML
	revision  int64
	published int64 // the revision of the latest version
	version   int64
	nextID    int64
	tenants   map[int64]map[string]any
	consumers map[int64]map[string]any
	writes    int // creates, updates, imports that changed and publishes
}

func newFakeCP(t *testing.T) *fakeCP {
	f := &fakeCP{t: t, revision: 1, nextID: 100, tenants: map[int64]map[string]any{}, consumers: map[int64]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCP) client() *Client {
	return NewClient(f.srv.URL+"/api/v1", f.srv.URL+"/token", "spillway-ci", secret.New(testSecret), 5*time.Second)
}

func writeJSON(w http.ResponseWriter, status int, v any, header ...string) {
	for i := 0; i+1 < len(header); i += 2 {
		w.Header().Set(header[i], header[i+1])
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeCP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("client_secret") != testSecret || r.PostForm.Get("client_id") != "spillway-ci" {
			writeJSON(w, 401, map[string]string{"error": "unauthorized_client"})
			return
		}
		writeJSON(w, 200, map[string]string{"access_token": testToken})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		writeJSON(w, 401, map[string]string{"error": "no token"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	tag := func(v int64) string { return strconv.Quote(strconv.FormatInt(v, 10)) }
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == "GET" && path == "/config/export":
		w.Header().Set("ETag", tag(f.revision))
		_, _ = w.Write(f.working)
	case r.Method == "POST" && path == "/config/import":
		if r.Header.Get("If-Match") != tag(f.revision) || r.Header.Get("Content-Type") != "application/yaml" || r.URL.Query().Get("note") != SeedNote {
			writeJSON(w, 412, map[string]string{"error": "bad import"})
			return
		}
		if !bytes.Equal(body, f.working) {
			f.working = body
			f.revision++
			f.writes++
		}
		writeJSON(w, 200, map[string]any{"working_set_revision": f.revision}, "ETag", tag(f.revision))
	case r.Method == "POST" && path == "/config/validate":
		writeJSON(w, 200, map[string]any{"valid": true, "errors": []any{}}, "ETag", tag(f.revision))
	case r.Method == "POST" && path == "/config/publish":
		if r.Header.Get("If-Match") != tag(f.revision) {
			writeJSON(w, 412, map[string]string{"error": "stale"})
			return
		}
		if f.published == f.revision {
			writeJSON(w, 409, map[string]string{"error": fmt.Sprintf("nothing to publish: the working set has not changed since version %d", f.version), "code": "conflict"})
			return
		}
		f.published = f.revision
		f.version++
		f.writes++
		writeJSON(w, 200, map[string]any{"version": f.version})
	case r.Method == "GET" && path == "/config/versions":
		writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"version": f.version}}})
	case r.Method == "GET" && path == "/plans":
		ids := map[string]int64{"free": 1, "pro": 2, "enterprise": 3}
		items := []any{}
		if id, ok := ids[r.URL.Query().Get("name")]; ok && f.version > 0 {
			items = append(items, map[string]any{"id": id})
		}
		writeJSON(w, 200, map[string]any{"items": items})
	case path == "/tenants" || strings.HasPrefix(path, "/tenants/"):
		f.entity(w, r, body, f.tenants, "/tenants")
	case path == "/consumers" || strings.HasPrefix(path, "/consumers/"):
		f.entity(w, r, body, f.consumers, "/consumers")
	default:
		writeJSON(w, 404, map[string]string{"error": "not found " + r.Method + " " + path})
	}
}

// entity serves list (by name, and tenant_id), create and replace.
func (f *fakeCP) entity(w http.ResponseWriter, r *http.Request, body []byte, rows map[int64]map[string]any, collection string) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch r.Method {
	case "GET":
		items := []any{}
		for _, row := range rows {
			if row["name"] != r.URL.Query().Get("name") {
				continue
			}
			if tid := r.URL.Query().Get("tenant_id"); tid != "" && fmt.Sprint(row["tenant_id"]) != tid {
				continue
			}
			items = append(items, row)
		}
		writeJSON(w, 200, map[string]any{"items": items})
	case "POST":
		var in map[string]any
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		f.nextID++
		in["id"], in["version"] = f.nextID, int64(1)
		rows[f.nextID] = in
		f.writes++
		writeJSON(w, 201, in)
	case "PUT":
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, collection+"/"), 10, 64)
		row, ok := rows[id]
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "no such entity"})
			return
		}
		if r.Header.Get("If-Match") != strconv.Quote(fmt.Sprint(row["version"])) {
			writeJSON(w, 412, map[string]string{"error": "stale"})
			return
		}
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		v, _ := strconv.ParseInt(fmt.Sprint(row["version"]), 10, 64)
		in["id"], in["version"] = id, v+1
		rows[id] = in
		f.writes++
		writeJSON(w, 200, in)
	default:
		writeJSON(w, 405, map[string]string{"error": "method"})
	}
}

func loadSeed(t *testing.T) SeedFile {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/demo/seed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSeedFile: deploy/demo/seed.yaml has ADR-0094 §3's three tenants on
// free, pro and enterprise and their eleven consumers.
func TestSeedFile(t *testing.T) {
	s := loadSeed(t)
	want := map[string]string{"free-co": "free", "pro-co": "pro", "enterprise-co": "enterprise"}
	consumers := 0
	for _, tn := range s.Tenants {
		if want[tn.Name] != tn.Plan {
			t.Errorf("tenant %s on plan %s", tn.Name, tn.Plan)
		}
		delete(want, tn.Name)
		for _, c := range tn.Consumers {
			consumers++
			if !strings.HasPrefix(c.Name, tn.Name+"-") {
				t.Errorf("consumer %s of tenant %s", c.Name, tn.Name)
			}
			if (c.JWTSubject != "") != (c.Name == "pro-co-service") {
				t.Errorf("consumer %s: jwt_subject %q", c.Name, c.JWTSubject)
			}
		}
	}
	if len(want) != 0 || consumers != 11 {
		t.Errorf("missing tenants %v; %d consumers, want 11", want, consumers)
	}
}

func TestParseSeedRejects(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"empty", "", "no tenants"},
		{"unknown field", "tenants: [{name: a, plan: p, quota: 1}]", "quota"},
		{"bad tenant name", "tenants: [{name: 'a b', plan: p}]", "tenants[0].name"},
		{"no plan", "tenants: [{name: a}]", `plan ""`},
		{"tenant twice", "tenants: [{name: a, plan: p}, {name: a, plan: p}]", "given twice"},
		{"bad consumer name", "tenants: [{name: a, plan: p, consumers: [{name: ''}]}]", "consumers[0].name"},
		{"consumer twice", "tenants: [{name: a, plan: p, consumers: [{name: c}, {name: c}]}]", `consumer "c" is given twice`},
		{"subject twice", "tenants: [{name: a, plan: p, consumers: [{name: c, jwt_subject: s}]}, {name: b, plan: p, consumers: [{name: d, jwt_subject: s}]}]", "jwt_subject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSeed([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want one with %q", err, tc.want)
			}
		})
	}
}

// TestSeedIdempotent: the first run publishes the config and creates every
// tenant and consumer; the second writes nothing; a tenant moved to another
// plan, a suspended consumer and a consumer with its own plan are put back
// by a third. No output holds the client secret or the token.
func TestSeedIdempotent(t *testing.T) {
	f := newFakeCP(t)
	s := loadSeed(t)
	var out bytes.Buffer
	cfg := []byte("routes: []\n")
	rep, err := Seed(t.Context(), f.client(), cfg, s, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Published || rep.ConfigVersion != 1 || rep.Created != 14 || rep.Updated != 0 || rep.Unchanged != 0 {
		t.Fatalf("first run: %+v\n%s", rep, out.String())
	}
	var svc map[string]any
	for _, c := range f.consumers {
		if c["name"] == "pro-co-service" {
			svc = c
		}
		if c["plan_id"] != nil || c["status"] != "active" {
			t.Errorf("consumer %v", c)
		}
	}
	if svc == nil || svc["jwt_subject"] != "0b5e7c1a-5f1d-4c3e-9a00-0000000000c2" {
		t.Errorf("pro-co-service: %v", svc)
	}

	writes := f.writes
	rep, err = Seed(t.Context(), f.client(), cfg, s, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Published || rep.ConfigVersion != 1 || rep.Created != 0 || rep.Updated != 0 || rep.Unchanged != 14 || f.writes != writes {
		t.Fatalf("second run: %+v, %d writes\n%s", rep, f.writes-writes, out.String())
	}
	if !strings.Contains(out.String(), "config version 1 is already the seed's") {
		t.Errorf("output:\n%s", out.String())
	}

	f.mu.Lock()
	for _, tn := range f.tenants {
		if tn["name"] == "pro-co" {
			tn["plan_id"] = 1
		}
	}
	for _, c := range f.consumers {
		switch c["name"] {
		case "free-co-web":
			c["status"] = "suspended"
		case "enterprise-co-web":
			c["plan_id"] = 1
		case "pro-co-service":
			c["jwt_subject"] = nil
		}
	}
	f.mu.Unlock()
	rep, err = Seed(t.Context(), f.client(), cfg, s, &out)
	if err != nil || rep.Updated != 4 || rep.Unchanged != 10 || rep.Created != 0 {
		t.Fatalf("third run: %+v %v\n%s", rep, err, out.String())
	}
	if all := out.String(); strings.Contains(all, testSecret) || strings.Contains(all, testToken) {
		t.Errorf("a secret or token was printed:\n%s", all)
	}
}

// TestSeedErrors: a plan missing from the config and an unwanted status
// are errors that name the call and never the token; a 401 is NotReady, a
// 422 is not.
func TestSeedErrors(t *testing.T) {
	f := newFakeCP(t)
	s := SeedFile{Tenants: []SeedTenant{{Name: "x-co", Plan: "gold"}}}
	_, err := Seed(t.Context(), f.client(), []byte("a: 1\n"), s, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `plan "gold" is not in the demo config`) || NotReady(err) {
		t.Errorf("err %v", err)
	}
	bad := NewClient(f.srv.URL+"/api/v1", f.srv.URL+"/token", "spillway-ci", secret.New("wrong-dev-only"), 5*time.Second)
	_, err = Seed(t.Context(), bad, []byte("a: 1\n"), s, io.Discard)
	if err == nil || !NotReady(err) || strings.Contains(err.Error(), "wrong-dev-only") {
		t.Errorf("wrong secret: %v", err)
	}
	if !NotReady(&StatusError{Status: 401}) || !NotReady(&StatusError{Status: 503}) || NotReady(&StatusError{Status: 422}) {
		t.Error("NotReady")
	}
}
