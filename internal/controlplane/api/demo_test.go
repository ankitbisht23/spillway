package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/demo"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// countingAuth is fakeAuth that counts its calls.
type countingAuth struct{ calls *atomic.Int64 }

func (c countingAuth) Authenticate(ctx context.Context, tok string) (store.Actor, error) {
	c.calls.Add(1)
	return fakeAuth{}.Authenticate(ctx, tok)
}

// demoTargets are the requests the demo guard must cover: every demo
// operation of the spec, with its parameters filled in, and paths under
// the prefix that name no operation.
func demoTargets(t *testing.T) []string {
	t.Helper()
	targets := []string{"/api/v1/demo", "/api/v1/demo/", "/api/v1/demo/anything", "/api/v1/demo/playground/runs", "/api/v1/demo/faults/redis-cut"}
	for _, op := range specOps(t) {
		if demoPath(op.url) && !slices.Contains(targets, op.url) {
			targets = append(targets, op.url)
		}
	}
	return targets
}

var everyMethod = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// answerShape is an answer without its request ID, to compare two answers.
func answerShape(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	h := rec.Header().Clone()
	h.Del("X-Request-Id")
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("a body that is not JSON: %q", rec.Body.String())
		}
		delete(body, "request_id")
	}
	b, _ := json.Marshal(map[string]any{"status": rec.Code, "header": h, "body": body})
	return string(b)
}

func serve(t *testing.T, h http.Handler, method, path, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] != "" {
			req.Header.Set(header[i], header[i+1])
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestDemoGuardOff: with demo mode off, every request under /api/v1/demo
// (each demo operation of the spec and paths naming none), with every
// method, with no token, a bad token, a viewer's and an admin's, is 404
// not_found exactly as an unknown path is (status, headers, body), before
// authentication and without reaching a handler or the store (SC-08,
// ADR-0095 §2).
func TestDemoGuardOff(t *testing.T) {
	var calls atomic.Int64
	var logs bytes.Buffer
	srv := New(nil, nil, nil, countingAuth{&calls}, secret.Value{}, logger(&logs)) // a nil store panics if reached
	h := srv.Handler()
	targets := demoTargets(t)
	if !slices.ContainsFunc(specOps(t), func(op specOp) bool { return op.id == "getDemo" }) {
		t.Fatal("the spec has no getDemo")
	}
	for _, method := range everyMethod {
		unknown := answerShape(t, serve(t, h, method, "/api/v1/no-such-path", ""))
		for _, path := range targets {
			for _, tok := range []string{"", "Bearer " + token("bad"), "Bearer " + token("viewer"), "Bearer " + token("admin")} {
				rec := serve(t, h, method, path, `{"active": true}`, "Authorization", tok, "Content-Type", "application/json")
				if got := answerShape(t, rec); got != unknown {
					t.Errorf("%s %s (%q): %s\nwant the unknown path's %s", method, path, tok, got, unknown)
				}
				if rec.Code != http.StatusNotFound || method != "HEAD" && !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
					t.Errorf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
				}
			}
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the authenticator was called %d times; the guard comes first", n)
	}
	if strings.Contains(logs.String(), tokenSecret) {
		t.Error("a token appears in the logs")
	}
}

// TestDemoGuardOn: with demo mode on, the demo operations go through the
// usual guard: no token 401, a bad token 401, a role the spec does not
// list 403, a listed role reaches the handler (here the store, which
// panics: 599); a path under the prefix that names no operation is the
// unknown path's 404.
func TestDemoGuardOn(t *testing.T) {
	h := newHandlerDemo(t, io.Discard, true)
	for _, op := range specOps(t) {
		if !demoPath(op.url) {
			continue
		}
		t.Run(op.id, func(t *testing.T) {
			if a := do(t, h, op.method, op.url, op.body, "Authorization", ""); a.status != 401 || a.err.Code != "unauthenticated" {
				t.Errorf("no token: %d %s", a.status, a.err.Code)
			}
			if a := do(t, h, op.method, op.url, op.body, "Authorization", "Bearer "+token("bad")); a.status != 401 {
				t.Errorf("bad token: %d", a.status)
			}
			for _, role := range Roles {
				a := do(t, h, op.method, op.url, op.body, "Authorization", "Bearer "+token(role))
				if allowed := slices.Contains(matrix[op.id], role); allowed != (a.status != 403) || a.status == 401 {
					t.Errorf("%s: %d, allowed by the matrix: %v", role, a.status, allowed)
				}
			}
		})
	}
	if a := do(t, h, "GET", "/api/v1/demo/anything", ""); a.status != 404 || a.err.Error != "no such path" {
		t.Errorf("an unknown demo path: %d %q", a.status, a.err.Error)
	}
}

// TestEveryDemoPathIsGuarded: every path of the spec under /demo is one
// the guard covers and is tagged demo, and every operation tagged demo is
// under /demo, so a later demo operation cannot miss the guard (ADR-0095
// §2).
func TestEveryDemoPathIsGuarded(t *testing.T) {
	n := 0
	for path, item := range spec(t).Paths.Map() {
		for method, op := range item.Operations() {
			tagged := slices.Contains(op.Tags, "demo")
			under := strings.HasPrefix(path, "/demo")
			if under != tagged || under != demoPath(BasePath+path) {
				t.Errorf("%s %s: under /demo %v, tagged demo %v, guarded %v", method, path, under, tagged, demoPath(BasePath+path))
			}
			if under {
				n++
			}
		}
	}
	if n == 0 {
		t.Error("the spec has no demo operation")
	}
	for path, want := range map[string]bool{
		"/api/v1/demo": true, "/api/v1/demo/": true, "/api/v1/demo/faults/x": true,
		"/api/v1/demos": false, "/api/v1/demox": false, "/api/v1/stream": false, "/demo": false, "/api/v1/Demo": false,
	} {
		if demoPath(path) != want {
			t.Errorf("demoPath(%q) = %v", path, !want)
		}
	}
}

// TestIssueKeyDemoRule: in demo mode IssueKey refuses an expiry more than
// 30 minutes ahead on the control plane's clock (a fake one here, an hour
// ahead of the real clock) with 422 naming expires_at, before the store;
// 29 minutes passes the rule and reaches the store (599); off, any future
// expiry reaches the store. The answers to the Playground generator's path
// share this rule (demo.Keys, tested in its package).
func TestIssueKeyDemoRule(t *testing.T) {
	now := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	newH := func(on bool) http.Handler {
		s := New(nil, nil, nil, fakeAuth{}, secret.Value{}, logger(io.Discard)).WithDemo(on)
		s.demoKeys.Now = func() time.Time { return now }
		h := s.Handler()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recover() != nil {
					w.WriteHeader(599)
				}
			}()
			h.ServeHTTP(w, r)
		})
	}
	body := func(d time.Duration) string { return `{"expires_at": "` + now.Add(d).Format(time.RFC3339) + `"}` }
	on, off := newH(true), newH(false)
	a := do(t, on, "POST", "/api/v1/consumers/1/keys", body(31*time.Minute), "Authorization", "Bearer "+token("key-admin"))
	if a.status != 422 || a.err.Code != apiv1.ErrorCodeValidationFailed || a.err.Details == nil || *(*a.err.Details)[0].Path != "expires_at" ||
		*(*a.err.Details)[0].Message != "expires_at: at most 30 minutes ahead in demo mode" {
		t.Errorf("31 minutes ahead in demo mode: %d %+v", a.status, a.err)
	}
	for name, tt := range map[string]struct {
		h    http.Handler
		body string
	}{
		"demo, 29 min":   {on, body(29 * time.Minute)},
		"demo, none":     {on, ""},
		"off, 31 min":    {off, body(31 * time.Minute)},
		"off, one month": {off, body(30 * 24 * time.Hour)},
	} {
		if a := do(t, tt.h, "POST", "/api/v1/consumers/1/keys", tt.body, "Authorization", "Bearer "+token("key-admin")); a.status != 599 {
			t.Errorf("%s: %d %+v, want it to reach the store", name, a.status, a.err)
		}
	}
	// Rotation asks the rule for the new key's expiry; on, it is 30
	// minutes from the fake now.
	if got, _ := (demo.Keys{On: true, Now: func() time.Time { return now }}).Expiry(nil); !got.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("a rotation's expiry %v", got)
	}
}

// TestDemoLimits: GetDemo's limits fill a GCRA burst from the rate.
func TestDemoLimits(t *testing.T) {
	if l := demoLimit(&configpb.Limit{Rate: 10, Period: "1s", Burst: 20}); *l != (apiv1.DemoLimit{Rate: 10, Burst: 20, Period: "1s"}) {
		t.Errorf("explicit burst: %+v", l)
	}
	if l := demoLimit(&configpb.Limit{Rate: 200, Period: "1s"}); *l != (apiv1.DemoLimit{Rate: 200, Burst: 200, Period: "1s"}) {
		t.Errorf("default burst: %+v", l)
	}
}

// TestDemoJanitorOff: with demo mode off the janitor returns at once and
// never touches the store (nil here).
func TestDemoJanitorOff(t *testing.T) {
	done := make(chan struct{})
	go func() {
		New(nil, nil, nil, fakeAuth{}, secret.Value{}, logger(io.Discard)).RunDemoJanitor(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the janitor runs with demo mode off")
	}
}
