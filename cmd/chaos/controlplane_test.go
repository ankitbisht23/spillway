package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken  = "test-bearer-token-not-printed"
	testSecret = "test-client-secret-not-printed"
)

var testGateways = []string{"gateway-1", "gateway-2", "gateway-3", "gateway-4", "gateway-5"}

// fakeCP is the control plane, Keycloak and the gateways of make
// verify-m5, enough for every step of "chaos controlplane".
type fakeCP struct {
	t   *testing.T
	srv *httptest.Server
	gws map[string]*fakeGW

	mu          sync.Mutex
	imported    []byte
	version     int64
	publishedAt map[int64]time.Time
	nothing     bool                     // the next publish answers 409
	delays      map[string]time.Duration // apply delay per replica; absent: never
	header      string                   // m5-propagation's verify header
	nextID      int64
	tenants     map[int64]string
	consumers   map[int64]int64 // consumer -> tenant
	keys        map[int64]*fakeKey
	identity    int64
	live        bool
	down        bool // the control plane is stopped
	downAt      time.Time
}

type fakeKey struct {
	consumer int64
	key      string
	revoked  bool
}

// fakeGW is one gateway: traffic, /readyz and /metrics on one server.
type fakeGW struct {
	srv          *httptest.Server
	unreadyUntil time.Time
	config       int64 // 0: the control plane's version
	fail         int   // the next fail requests answer 503
}

func newFakeCP(t *testing.T) *fakeCP {
	t.Helper()
	f := &fakeCP{t: t, version: 100, publishedAt: map[int64]time.Time{100: time.Now()}, delays: map[string]time.Duration{},
		tenants: map[int64]string{}, consumers: map[int64]int64{}, keys: map[int64]*fakeKey{}, identity: 7, live: true, nextID: 10,
		gws: map[string]*fakeGW{}}
	for _, g := range testGateways {
		f.delays[g] = 3 * time.Millisecond
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/config/export", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"5"`)
		_, _ = io.WriteString(w, "{}\n")
	})
	api.HandleFunc("POST /api/v1/config/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != `"5"` || r.Header.Get("Content-Type") != "application/yaml" || r.URL.Query().Get("note") == "" {
			http.Error(w, `{"error":"bad import"}`, http.StatusBadRequest)
			return
		}
		f.imported, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "{}")
	})
	api.HandleFunc("POST /api/v1/config/validate", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"6"`)
		_, _ = io.WriteString(w, `{"valid":true,"errors":[]}`)
	})
	api.HandleFunc("POST /api/v1/config/publish", func(w http.ResponseWriter, r *http.Request) {
		var n struct{ Note string }
		if json.NewDecoder(r.Body).Decode(&n) != nil || n.Note == "" || r.Header.Get("If-Match") != `"6"` {
			http.Error(w, `{"error":"bad publish"}`, http.StatusBadRequest)
			return
		}
		if f.nothing {
			f.nothing = false
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"nothing to publish","code":"conflict"}`)
			return
		}
		f.version++
		f.publishedAt[f.version] = time.Now()
		_ = json.NewEncoder(w).Encode(map[string]any{"version": f.version, "published_at": f.publishedAt[f.version]})
	})
	api.HandleFunc("GET /api/v1/config/versions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"version": f.version, "published_at": f.publishedAt[f.version]}}})
	})
	api.HandleFunc("GET /api/v1/config/versions/{v}", func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.ParseInt(r.PathValue("v"), 10, 64)
		at := f.publishedAt[v]
		applied := []any{map[string]any{"replica": "gateway-9", "at": at}} // not one of -gateways
		for g, d := range f.delays {
			if time.Since(at) >= d {
				applied = append(applied, map[string]any{"replica": g, "at": at.Add(d)})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": v, "published_at": at, "applied": applied})
	})
	api.HandleFunc("GET /api/v1/plans", f.named(func() map[int64]string { return map[int64]string{80: m5Name} }))
	api.HandleFunc("GET /api/v1/routes", f.named(func() map[int64]string { return map[int64]string{3: m5Propagation} }))
	api.HandleFunc("GET /api/v1/routes/3", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"9"`)
		_, _ = io.WriteString(w, `{"id":3,"name":"m5-propagation","match":{"path_prefix":"/anything/m5"},"upstream":"m5","auth_mode":"none","version":9,"created_at":"2026-11-14T00:00:00Z","updated_at":"2026-11-14T00:00:00Z"}`)
	})
	api.HandleFunc("PUT /api/v1/routes/3", func(w http.ResponseWriter, r *http.Request) {
		var route struct {
			ID              int64 `json:"id"`
			Version         int64 `json:"version"`
			ResponseHeaders struct {
				Set []struct{ Name, Value string }
			} `json:"response_headers"`
		}
		if json.NewDecoder(r.Body).Decode(&route) != nil || route.ID != 0 || route.Version != 0 || r.Header.Get("If-Match") != `"9"` ||
			len(route.ResponseHeaders.Set) != 1 || route.ResponseHeaders.Set[0].Name != verifyHeader {
			http.Error(w, `{"error":"bad route"}`, http.StatusBadRequest)
			return
		}
		f.header = route.ResponseHeaders.Set[0].Value
		_, _ = io.WriteString(w, "{}")
	})
	api.HandleFunc("GET /api/v1/tenants", f.named(func() map[int64]string { return f.tenants }))
	api.HandleFunc("POST /api/v1/tenants", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name   string
			PlanID int64 `json:"plan_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.PlanID != 80 || slices.Contains(mapValues(f.tenants), b.Name) {
			http.Error(w, `{"error":"bad tenant"}`, http.StatusConflict)
			return
		}
		f.created(w, func(id int64) { f.tenants[id] = b.Name })
	})
	api.HandleFunc("GET /api/v1/tenants/{id}", f.versioned)
	api.HandleFunc("DELETE /api/v1/tenants/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := pathID(r)
		for _, t := range f.consumers {
			if t == id {
				http.Error(w, `{"error":"tenant has consumers"}`, http.StatusConflict)
				return
			}
		}
		f.deleted(w, r, func() { delete(f.tenants, id) })
	})
	api.HandleFunc("GET /api/v1/consumers", func(w http.ResponseWriter, r *http.Request) {
		tenant, _ := strconv.ParseInt(r.URL.Query().Get("tenant_id"), 10, 64)
		f.named(func() map[int64]string {
			m := map[int64]string{}
			for c, t := range f.consumers {
				if t == tenant {
					m[c] = m5Name
				}
			}
			return m
		})(w, r)
	})
	api.HandleFunc("POST /api/v1/consumers", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			TenantID int64 `json:"tenant_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.created(w, func(id int64) { f.consumers[id] = b.TenantID })
	})
	api.HandleFunc("GET /api/v1/consumers/{id}", f.versioned)
	api.HandleFunc("DELETE /api/v1/consumers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := pathID(r)
		f.deleted(w, r, func() {
			delete(f.consumers, id)
			for k, x := range f.keys {
				if x.consumer == id {
					delete(f.keys, k)
				}
			}
		})
	})
	api.HandleFunc("GET /api/v1/consumers/{id}/keys", func(w http.ResponseWriter, r *http.Request) {
		var items []any
		for id, k := range f.keys {
			if k.consumer == pathID(r) {
				item := map[string]any{"id": id, "revoked_at": nil}
				if k.revoked {
					item["revoked_at"] = time.Now()
				}
				items = append(items, item)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	api.HandleFunc("POST /api/v1/consumers/{id}/keys", func(w http.ResponseWriter, r *http.Request) {
		f.identity++
		f.nextID++
		key := fmt.Sprintf("spw_test%04d_secretpartnotprinted", f.nextID)
		f.keys[f.nextID] = &fakeKey{consumer: pathID(r), key: key}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.nextID, "key": key})
	})
	api.HandleFunc("POST /api/v1/keys/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		k, ok := f.keys[pathID(r)]
		if !ok {
			http.Error(w, `{"error":"no key"}`, http.StatusNotFound)
			return
		}
		f.identity++
		k.revoked = true
		_, _ = io.WriteString(w, "{}")
	})
	api.HandleFunc("GET /api/v1/identity/export", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "version: %d\ntenants: []\n", f.identity)
	})
	api.HandleFunc("GET /api/v1/replicas", func(w http.ResponseWriter, _ *http.Request) {
		var items []any
		for _, g := range testGateways {
			items = append(items, map[string]any{"id": g, "config_version": f.version, "identity_version": f.identity, "watch_connected": f.live, "live": f.live})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		api.ServeHTTP(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	for _, g := range testGateways {
		gw := &fakeGW{}
		gw.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.gateway(gw, w, r) }))
		t.Cleanup(gw.srv.Close)
		f.gws[g] = gw
	}
	return f
}

func mapValues(m map[int64]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func (f *fakeCP) token(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("grant_type") != "client_credentials" || r.FormValue("client_id") != ciClient || r.FormValue("client_secret") != testSecret {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized_client"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"access_token": testToken})
}

func (f *fakeCP) named(all func() map[int64]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items := []any{}
		for id, name := range all() {
			if name == r.URL.Query().Get("name") {
				items = append(items, map[string]any{"id": id, "name": name})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}
}

func (f *fakeCP) created(w http.ResponseWriter, add func(id int64)) {
	f.nextID++
	f.identity++
	add(f.nextID)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": f.nextID})
}

// versioned answers an identity row; its version is the identity version.
func (f *fakeCP) versioned(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"version": f.identity})
}

func (f *fakeCP) deleted(w http.ResponseWriter, r *http.Request, del func()) {
	if r.Header.Get("If-Match") != etag(f.identity) {
		http.Error(w, `{"error":"stale"}`, http.StatusPreconditionFailed)
		return
	}
	f.identity++
	del()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeCP) gateway(gw *fakeGW, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/readyz":
		if time.Now().Before(gw.unreadyUntil) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	case "/metrics":
		age := 0.5 // synced, or since the stop (a restarted gateway's snapshot is older)
		if f.down {
			age += time.Since(f.downAt).Seconds()
		}
		config := f.version
		if gw.config != 0 {
			config = gw.config
		}
		_, _ = fmt.Fprintf(w, "# TYPE gw_config_version gauge\ngw_config_version %d\ngw_identity_version %d\ngw_config_sync_age_seconds %g\n", config, f.identity, age)
	case "/status/200":
		if time.Now().Before(gw.unreadyUntil) {
			w.WriteHeader(http.StatusServiceUnavailable) // the restart: no request must reach it
			return
		}
		if gw.fail > 0 {
			gw.fail--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		for _, k := range f.keys {
			if k.key == r.Header.Get("X-API-Key") && !k.revoked {
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// run runs one step of chaos controlplane against f with flags, and
// returns its exit code, stdout and stderr. Nothing it prints may hold the
// token, the client secret or any key.
func (f *fakeCP) run(t *testing.T, stdin io.Reader, stderr io.Writer, args ...string) (int, string, string) {
	t.Helper()
	var errBuf bytes.Buffer
	if stderr == nil {
		stderr = &errBuf
	}
	o, err := parseControlPlaneFlags(append(args, "-controlplane", f.srv.URL+"/api/v1", "-controlplane-admin", f.srv.URL, "-token-url", f.srv.URL+"/token", "-run-id", "t1"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return map[string]string{ciSecretEnv: testSecret}[k] }
	sec, _ := secretFrom(getenv)
	r := newCPRun(o, newCPClient(o.controlPlane, o.tokenURL, sec, o.restTimeout))
	addr := func(g string) string { return strings.TrimPrefix(f.gws[g].srv.URL, "http://") }
	r.traffic, r.admin = addr, addr
	var out bytes.Buffer
	code := r.step(t.Context(), cmpReader(stdin), &out, stderr)
	all := out.String() + errBuf.String()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range []string{testToken, testSecret, "secretpartnotprinted"} {
		if strings.Contains(all, s) {
			t.Errorf("chaos controlplane %s printed %q", o.step, s)
		}
	}
	return code, out.String(), errBuf.String()
}

func cmpReader(r io.Reader) io.Reader {
	if r == nil {
		return strings.NewReader("")
	}
	return r
}

func TestParseControlPlaneFlags(t *testing.T) {
	o, err := parseControlPlaneFlags([]string{"outage"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	oo := o.outage
	if o.step != "outage" || o.publishes != 60 || o.rounds != 20 || o.limit != 5*time.Second || o.restTimeout != 10*time.Second ||
		len(o.gateways) != 5 || oo.rate != 500 || oo.before != 30*time.Second || oo.length != 600*time.Second || oo.restart != "gateway-3" ||
		oo.restartAt != 300*time.Second || oo.after != time.Minute || oo.every != 5*time.Second || oo.within != time.Minute ||
		oo.slack != 10*time.Second || oo.timeout != 20*time.Second || oo.maxInFlight != 2000 {
		t.Errorf("defaults are not ADR-0042's: %+v", o)
	}
	for _, args := range [][]string{
		{}, {"publish"}, {"-publishes", "5"}, {"setup", "extra"}, {"outage", "-restart-at", "600s"},
		{"outage", "-restart", "gateway-9"}, {"setup", "-gateways", ""}, {"propagation", "-publishes", "0"},
	} {
		if _, err := parseControlPlaneFlags(args, io.Discard); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
}

func TestNearestRank(t *testing.T) {
	var s []float64
	for i := range 60 {
		s = append(s, float64(i+1))
	}
	if p99, p50 := nearestRank(s, 0.99), nearestRank(s, 0.5); p99 != 60 || p50 != 30 {
		t.Errorf("N=60: p99 %v (want the maximum, 60), p50 %v", p99, p50)
	}
	if nearestRank(nil, 0.99) != 0 || nearestRank([]float64{7}, 0.5) != 7 {
		t.Error("empty or single")
	}
}

func TestControlPlaneSetupAndCleanup(t *testing.T) {
	f := newFakeCP(t)
	yaml := "routes: []\n"
	code, out, errs := f.run(t, strings.NewReader(yaml), nil, "setup")
	var rep setupReport
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.ConfigVersion != 101 || rep.PlanID != 80 || rep.TenantID == 0 || rep.ConsumerID == 0 {
		t.Fatalf("setup: exit %d, %+v, %s", code, rep, errs)
	}
	if string(f.imported) != yaml {
		t.Errorf("imported %q", f.imported)
	}
	// A second run: the first run's tenant goes, the working set is
	// already published (409), a new tenant and consumer.
	f.keys[99] = &fakeKey{consumer: rep.ConsumerID, key: "spw_left0099_secretpartnotprinted"}
	f.nothing = true
	code, out, errs = f.run(t, strings.NewReader(yaml), nil, "setup")
	var rep2 setupReport
	if code != 0 || json.Unmarshal([]byte(out), &rep2) != nil || rep2.ConfigVersion != 101 || rep2.TenantID == rep.TenantID {
		t.Fatalf("setup again: exit %d, %+v, %s", code, rep2, errs)
	}
	if len(f.tenants) != 1 || len(f.consumers) != 1 || len(f.keys) != 0 {
		t.Errorf("after setup again: tenants %v, consumers %v, keys %v", f.tenants, f.consumers, f.keys)
	}
	// Cleanup revokes the keys and deletes consumer and tenant.
	f.keys[100] = &fakeKey{consumer: rep2.ConsumerID, key: "spw_left0100_secretpartnotprinted"}
	if code, _, errs := f.run(t, nil, nil, "cleanup"); code != 0 || len(f.tenants) != 0 || len(f.consumers) != 0 {
		t.Fatalf("cleanup: exit %d, tenants %v, %s", code, f.tenants, errs)
	}
	if code, _, errs := f.run(t, nil, nil, "cleanup"); code != 0 {
		t.Fatalf("cleanup with nothing to clean: exit %d, %s", code, errs)
	}
	// No config on stdin; a wrong secret fails without printing it.
	if code, _, _ := f.run(t, nil, nil, "setup"); code != 2 {
		t.Errorf("setup without a config: exit %d, want 2", code)
	}
}

func TestControlPlaneReady(t *testing.T) {
	f := newFakeCP(t)
	if code, out, errs := f.run(t, nil, nil, "ready"); code != 0 || !strings.Contains(out, `"identity_version": 7`) {
		t.Fatalf("ready: exit %d, %s %s", code, out, errs)
	}
	f.live = false
	code, _, errs := f.run(t, nil, nil, "ready", "-ready-wait", "600ms")
	if code != 1 || !strings.Contains(errs, "FAIL  setup") || !strings.Contains(errs, "gateway-3 live false") {
		t.Fatalf("not live: exit %d, %s", code, errs)
	}
}

func TestControlPlanePropagation(t *testing.T) {
	f := newFakeCP(t)
	f.delays["gateway-4"] = 40 * time.Millisecond
	code, out, errs := f.run(t, nil, nil, "propagation", "-publishes", "4", "-pause", "10ms", "-poll", "10ms")
	var rep propagationReport
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !rep.Pass || rep.N != 4 || f.version != 104 || f.header != "t1-3" {
		t.Fatalf("propagation: exit %d, %+v, version %d, header %q, %s", code, rep, f.version, f.header, errs)
	}
	for _, s := range rep.Samples {
		if s.SampleMS != 40 || len(s.AppliedMS) != 5 {
			t.Errorf("sample %+v: want the latest replica's 40 ms over the 5 replicas", s)
		}
	}
	if !strings.HasPrefix(errs, "PASS  (a) publish on all 5 replicas, p99 < 5 s") || !strings.Contains(errs, "p99 40 ms") {
		t.Errorf("line: %s", errs)
	}

	// gateway-2 never applies: its sample is the apply wait, a FAIL.
	delete(f.delays, "gateway-2")
	code, out, errs = f.run(t, nil, nil, "propagation", "-publishes", "2", "-pause", "0s", "-poll", "10ms", "-apply-wait", "200ms", "-limit", "150ms")
	rep = propagationReport{}
	if code != 1 || json.Unmarshal([]byte(out), &rep) != nil || rep.Pass || rep.P99MS != 200 || !slices.Equal(rep.Samples[0].Missing, []string{"gateway-2"}) {
		t.Fatalf("missing replica: exit %d, %+v", code, rep)
	}
	if !strings.HasPrefix(errs, "FAIL  (a)") || !strings.Contains(errs, "version 105 missing on gateway-2") {
		t.Errorf("lines: %s", errs)
	}
}

func TestControlPlaneRevocation(t *testing.T) {
	f := newFakeCP(t)
	if code, _, errs := f.run(t, strings.NewReader("routes: []\n"), nil, "setup"); code != 0 {
		t.Fatal(errs)
	}
	code, out, errs := f.run(t, nil, nil, "revocation", "-rounds", "2")
	var rep revocationReport
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !rep.Pass || len(rep.Rounds) != 2 || len(rep.Rounds[0].RejectedMS) != 5 {
		t.Fatalf("revocation: exit %d, %+v, %s", code, rep, errs)
	}
	if !strings.HasPrefix(errs, "PASS  (b) revoked key rejected on all 5 replicas < 5 s") {
		t.Errorf("line: %s", errs)
	}
	for id, k := range f.keys {
		if !k.revoked {
			t.Errorf("key %d not revoked", id)
		}
	}
}

// fakeScript plays test/verify/m5.sh's part in the outage: it reads the
// sender's stderr for actions and answers on its stdin.
type fakeScript struct {
	f       *fakeCP
	answers *io.PipeWriter
	mu      sync.Mutex
	all     bytes.Buffer
	pending string
	actions []string
}

func (s *fakeScript) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all.Write(p)
	s.pending += string(p)
	for {
		i := strings.IndexByte(s.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		l := s.pending[:i]
		s.pending = s.pending[i+1:]
		if strings.HasPrefix(l, "action ") {
			s.actions = append(s.actions, l)
			go s.act(l)
		}
	}
}

func (s *fakeScript) act(action string) {
	f := s.f
	answer := func(l string) { _, _ = io.WriteString(s.answers, l+"\n") }
	switch {
	case action == "action stop":
		answer("stopping")
		f.mu.Lock()
		f.down, f.downAt = true, time.Now()
		f.mu.Unlock()
		answer("stopped 0")
	case strings.HasPrefix(action, "action restart "):
		f.mu.Lock()
		gw := f.gws[strings.TrimPrefix(action, "action restart ")]
		gw.unreadyUntil = time.Now().Add(300 * time.Millisecond)
		f.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		answer("restarted 0")
	case action == "action start":
		f.mu.Lock()
		f.down = false
		f.mu.Unlock()
		answer("healthy 0")
	}
}

func (s *fakeScript) lines() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all.String()
}

var shortOutage = []string{"outage", "-rate", "200", "-before", "300ms", "-outage", "2s", "-restart-at", "800ms", "-after", "500ms",
	"-scrape-every", "100ms", "-within", "1s", "-slack", "1s", "-request-timeout", "2s", "-action-wait", "3s"}

func runFakeOutage(t *testing.T, f *fakeCP) (int, outageReport, string) {
	t.Helper()
	if code, _, errs := f.run(t, strings.NewReader("routes: []\n"), nil, "setup"); code != 0 {
		t.Fatal(errs)
	}
	stdin, answers := io.Pipe()
	s := &fakeScript{f: f, answers: answers}
	var out bytes.Buffer
	o, err := parseControlPlaneFlags(append(slices.Clone(shortOutage), "-controlplane", f.srv.URL+"/api/v1", "-controlplane-admin", f.srv.URL, "-token-url", f.srv.URL+"/token"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := secretFrom(func(k string) string { return map[string]string{ciSecretEnv: testSecret}[k] })
	r := newCPRun(o, newCPClient(o.controlPlane, o.tokenURL, sec, o.restTimeout))
	addr := func(g string) string { return strings.TrimPrefix(f.gws[g].srv.URL, "http://") }
	r.traffic, r.admin = addr, addr
	code := r.step(context.Background(), stdin, &out, s)
	_ = answers.Close()
	var rep outageReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.actions, []string{"action stop", "action restart gateway-3", "action start"}) {
		t.Errorf("actions %q", s.actions)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if !k.revoked {
			t.Error("the load key was not revoked")
		}
		if strings.Contains(out.String()+s.lines(), k.key) {
			t.Error("the report or the lines hold the load key")
		}
	}
	if strings.Contains(out.String()+s.lines(), testToken) {
		t.Error("the report or the lines hold the token")
	}
	return code, rep, s.lines()
}

func TestControlPlaneOutage(t *testing.T) {
	f := newFakeCP(t)
	code, rep, lines := runFakeOutage(t, f)
	if code != 0 || !rep.Pass {
		t.Fatalf("exit %d:\n%s", code, lines)
	}
	for _, c := range []string{"(c1)", "(c2)", "(c3)", "(c4)", "(c5)"} {
		if !strings.Contains(lines, "PASS  "+c) {
			t.Errorf("no PASS line for %s:\n%s", c, lines)
		}
	}
	// gateway-3's share went to the other four while it was paused, and
	// it served again after its restart.
	if rep.Load.Sent == 0 || rep.Load.OK != rep.Load.Sent || rep.OKAfterResume == 0 {
		t.Errorf("load %+v, %d 2xx after the resume", rep.Load, rep.OKAfterResume)
	}
	if g3, g4 := rep.PerGateway["gateway-3"].Sent, rep.PerGateway["gateway-4"].Sent; g3 >= g4 {
		t.Errorf("gateway-3 got %d requests, gateway-4 %d: the paused share was not moved", g3, g4)
	}
}

func TestControlPlaneOutageFails(t *testing.T) {
	f := newFakeCP(t)
	f.gws["gateway-5"].config = 12345 // its version before the stop
	go func() {
		// Under load, gateway-2 answers three 503s; in the outage,
		// gateway-5 moves to another version.
		time.Sleep(200 * time.Millisecond)
		f.mu.Lock()
		f.gws["gateway-2"].fail = 3
		f.mu.Unlock()
		time.Sleep(800 * time.Millisecond)
		f.mu.Lock()
		f.gws["gateway-5"].config = 0
		f.mu.Unlock()
	}()
	code, rep, lines := runFakeOutage(t, f)
	if code != 1 || rep.Pass {
		t.Fatalf("exit %d, want FAIL:\n%s", code, lines)
	}
	for _, want := range []string{"FAIL  (c1) zero failed requests", "other statuses 503 3", "FAIL  (c2) config frozen at last good", "gateway-5 at", "failure: gateway-2"} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %q in:\n%s", want, lines)
		}
	}
}

// at is a scrape of g at d after stop.
func at(stop time.Time, g string, d time.Duration, config int64, age float64) outageScrape {
	return outageScrape{Gateway: g, Sent: stop.Add(d), Recv: stop.Add(d + 3*time.Millisecond), OK: true, Ready: 200, Config: config, Identity: 9, SyncAge: age}
}

func TestAnalyzeOutage(t *testing.T) {
	o := outageOptions{length: 600 * time.Second, restart: "gateway-3", within: time.Minute, slack: 10 * time.Second}
	gws := []string{"gateway-1", "gateway-3"}
	stop := time.Unix(1_000_000, 0)
	tl := outageTimes{Stop: stop, Pause: stop.Add(300 * time.Second), Restarted: stop.Add(306 * time.Second), Ready: stop.Add(311 * time.Second),
		Resume: stop.Add(311 * time.Second), Healthy: stop.Add(606 * time.Second), Live: stop.Add(616 * time.Second)}
	good := func() []outageScrape {
		var s []outageScrape
		for d := -10 * time.Second; d <= 666*time.Second; d += 5 * time.Second {
			for _, g := range gws {
				age := 2 + d.Seconds()
				if d >= 606*time.Second+5*time.Second {
					age = 3
				}
				x := at(stop, g, d, 41, age)
				if g == "gateway-3" && d >= 300*time.Second && d < 316*time.Second {
					x = outageScrape{Gateway: g, Sent: stop.Add(d), Recv: stop.Add(d), Err: "readyz: dial"} // exempt: paused, restarting
				}
				s = append(s, x)
			}
		}
		return s
	}
	load := loadCounts{Sent: 1000, OK: 1000}
	check := func(name string, scrapes []outageScrape, tl outageTimes, load loadCounts, want [5]bool) {
		t.Helper()
		rules, _ := analyzeOutage(o, gws, scrapes, tl, load, 50)
		for i, r := range rules {
			if r.Pass != want[i] {
				t.Errorf("%s: %s pass %v, want %v: %s", name, r.Check, r.Pass, want[i], r.Measured)
			}
		}
	}
	check("good", good(), tl, load, [5]bool{true, true, true, true, true})

	failed := load
	failed.Dropped, failed.Status = 1, map[string]int64{"503": 1}
	check("a drop and a 503", good(), tl, failed, [5]bool{false, true, true, true, true})

	s := good()
	s[40].Err, s[40].OK = "metrics: timeout", false // gateway-1 in the outage
	check("a failed scrape", s, tl, load, [5]bool{true, false, false, true, true})

	s = good()
	if s[2].Gateway != "gateway-1" || s[2].Sent != stop.Add(-5*time.Second) {
		t.Fatalf("scrape 2 is %s at %v", s[2].Gateway, s[2].Sent.Sub(stop))
	}
	s[2].Err, s[2].OK = "metrics: timeout", false // gateway-1's last scrape before the stop: no baseline
	check("a failed last scrape before the stop", s, tl, load, [5]bool{true, false, true, true, false})

	s = good()
	s[41].Config = 42 // gateway-3 at another version before its pause
	check("another version", s, tl, load, [5]bool{true, false, true, true, true})

	s = good()
	s[60].SyncAge = 1 // gateway-1 synced during the outage
	check("sync age reset", s, tl, load, [5]bool{true, true, false, true, true})

	s = good()
	if s[246].Gateway != "gateway-1" || s[246].Sent != stop.Add(605*time.Second) {
		t.Fatalf("scrape 246 is %s at %v", s[246].Gateway, s[246].Sent.Sub(stop))
	}
	s[246].Recv, s[246].SyncAge = tl.Healthy.Add(time.Millisecond), 1 // answered after t_healthy, synced again
	check("a scrape answered after healthy", s, tl, load, [5]bool{true, true, true, true, true})

	slow := tl
	slow.Ready = slow.Restarted.Add(61 * time.Second)
	check("gateway-3 ready late", good(), slow, load, [5]bool{true, true, true, false, true})

	s = good()
	for i := range s {
		if s[i].Gateway == "gateway-3" && s[i].Sent.After(tl.Restarted) && s[i].Sent.Before(tl.Healthy) && s[i].OK {
			s[i].Identity = 8 // booted from an older snapshot
		}
	}
	check("gateway-3 at older versions", s, tl, load, [5]bool{true, false, true, false, true})

	late := tl
	late.Live = tl.Healthy.Add(61 * time.Second)
	check("replicas live late", good(), late, load, [5]bool{true, true, true, true, false})

	s = good()
	for i := range s {
		if s[i].Sent.After(tl.Healthy) {
			s[i].SyncAge = 30 // never synced again
		}
	}
	check("never synced after", s, tl, load, [5]bool{true, true, true, true, false})

	noStop := tl
	noStop.StopCode = 1
	check("docker compose stop failed", good(), noStop, load, [5]bool{true, false, true, true, true})
	noStart := tl
	noStart.StartCode = 1
	check("docker compose up failed", good(), noStart, load, [5]bool{true, true, true, true, false})
}
