package webui

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ankitbisht23/spillway/internal/controlplane/api"
	"github.com/ankitbisht23/spillway/internal/secret"
)

const issuer = "http://localhost:8180/auth/realms/spillway"

// wantCSP is ADR-0057's policy, written out, with compose's issuer origin.
const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
	"connect-src 'self' http://localhost:8180; form-action 'self' http://localhost:8180; " +
	"frame-src 'none'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; worker-src 'none'; manifest-src 'self'"

var security = map[string]string{
	"Content-Security-Policy":      wantCSP,
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"X-Frame-Options":              "DENY",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Permissions-Policy":           "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()",
}

const indexHTML = `<!doctype html><div id="root"></div><script type="module" src="/assets/index-AbC123.js"></script>`

var build = fstest.MapFS{
	"index.html":             {Data: []byte(indexHTML)},
	"favicon.svg":            {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	"assets/index-AbC123.js": {Data: []byte(`console.log(1)`)},
	"assets/index-Xy9.css":   {Data: []byte(`body{}`)},
}

// newHandler serves build in front of the real API handler (without a
// store: these requests never reach it); the stub at /api/v1/stub stands
// for an operation that sets its own Cache-Control.
func newHandler(t *testing.T) http.Handler {
	t.Helper()
	apiH := api.New(nil, nil, nil, nil, secret.New("dev-only-not-secret"), slog.New(slog.NewJSONHandler(io.Discard, nil))).Handler()
	mux := http.NewServeMux()
	mux.Handle("/", apiH)
	mux.HandleFunc("/api/v1/stub", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = io.WriteString(w, "{}")
	})
	h, err := New(build, Config{Issuer: issuer, ClientID: "admin-ui"}, mux)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func serve(h http.Handler, method, path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestServing walks ADR-0057's serving table.
func TestServing(t *testing.T) {
	h := newHandler(t)
	for _, tt := range []struct {
		name, method, path string
		status             int
		body, ctype, cache string
	}{
		{"root", "GET", "/", 200, indexHTML, "text/html; charset=utf-8", "no-cache"},
		{"a route", "GET", "/overview", 200, indexHTML, "text/html; charset=utf-8", "no-cache"},
		{"a nested route", "GET", "/policies/12/shadow", 200, indexHTML, "text/html; charset=utf-8", "no-cache"},
		{"an unknown route", "GET", "/no/such/screen", 200, indexHTML, "text/html; charset=utf-8", "no-cache"},
		{"a dotted unknown path", "GET", "/missing.js", 200, indexHTML, "text/html; charset=utf-8", "no-cache"},
		{"index.html", "GET", "/index.html", 200, indexHTML, "text/html; charset=utf-8", "no-cache"},
		{"a root file", "GET", "/favicon.svg", 200, `<svg xmlns="http://www.w3.org/2000/svg"/>`, "image/svg+xml", "no-cache"},
		{"an asset", "GET", "/assets/index-AbC123.js", 200, "console.log(1)", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"a stylesheet", "GET", "/assets/index-Xy9.css", 200, "body{}", "text/css; charset=utf-8", "public, max-age=31536000, immutable"},
		{"an asset, HEAD", "HEAD", "/assets/index-AbC123.js", 200, "", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable"},
		{"a route, HEAD", "HEAD", "/routes", 200, "", "text/html; charset=utf-8", "no-cache"},
		{"a missing asset", "GET", "/assets/index-old.js", 404, "not found\n", "text/plain; charset=utf-8", ""},
		{"the assets directory", "GET", "/assets/", 404, "not found\n", "text/plain; charset=utf-8", ""},
		{"a climbing asset path", "GET", "/assets/../index.html", 404, "not found\n", "text/plain; charset=utf-8", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(h, tt.method, tt.path)
			if rec.Code != tt.status || rec.Body.String() != tt.body {
				t.Errorf("%d %q, want %d %q", rec.Code, rec.Body.String(), tt.status, tt.body)
			}
			if got := rec.Header().Get("Content-Type"); got != tt.ctype {
				t.Errorf("Content-Type %q, want %q", got, tt.ctype)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.cache {
				t.Errorf("Cache-Control %q, want %q", got, tt.cache)
			}
			checkSecurity(t, rec.Header())
		})
	}
}

func checkSecurity(t *testing.T, h http.Header) {
	t.Helper()
	for k, want := range security {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHandler(t)
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		rec := serve(h, m, "/overview")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d, Allow %q", m, rec.Code, rec.Header().Get("Allow"))
		}
		checkSecurity(t, rec.Header())
	}
}

// TestRevalidation checks that no-cache files revalidate to 304 with their
// strong ETag, and that a changed file has another.
func TestRevalidation(t *testing.T) {
	h := newHandler(t)
	first := serve(h, "GET", "/overview")
	tag := first.Header().Get("ETag")
	if !strings.HasPrefix(tag, `"`) || strings.HasPrefix(tag, `W/`) || len(tag) < 10 {
		t.Fatalf("ETag %q: want a strong ETag", tag)
	}
	if rec := serve(h, "GET", "/", "If-None-Match", tag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d %q, want 304", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "GET", "/", "If-None-Match", `"other"`); rec.Code != http.StatusOK {
		t.Errorf("another ETag: %d, want 200", rec.Code)
	}
	if other := serve(h, "GET", "/favicon.svg").Header().Get("ETag"); other == tag {
		t.Errorf("two files share the ETag %s", tag)
	}
}

func TestUIConfig(t *testing.T) {
	h := newHandler(t)
	for _, m := range []string{"GET", "HEAD"} {
		rec := serve(h, m, "/api/ui-config")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d, Cache-Control %q, Content-Type %q", m, rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("Content-Type"))
		}
		checkSecurity(t, rec.Header())
		if m == "HEAD" {
			continue
		}
		var got map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 2 || got["issuer"] != issuer || got["client_id"] != "admin-ui" {
			t.Errorf("body %s: want exactly the issuer and client_id", rec.Body.String())
		}
	}
}

// TestAPIPaths checks that /api/v1 reaches the API, every other /api path
// gets the API's JSON 404 (never index.html), and every API answer is
// nosniff and not stored, unless it sets its own Cache-Control.
func TestAPIPaths(t *testing.T) {
	h := newHandler(t)
	for _, tt := range []struct{ method, path string }{
		{"GET", "/api"}, {"GET", "/api/"}, {"GET", "/api/nope"}, {"GET", "/api/v2/routes"},
		{"POST", "/api/ui-config"}, {"GET", "/api/ui-config/x"}, {"GET", "/api/v1/nope"}, {"GET", "/api/v1"},
	} {
		rec := serve(h, tt.method, tt.path)
		var body struct{ Error, Code, RequestID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 404 || body.Code != "not_found" || body.Error != "no such path" || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: %d %q, want the API's JSON 404", tt.method, tt.path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: X-Content-Type-Options %q, Cache-Control %q", tt.method, tt.path, rec.Header().Get("X-Content-Type-Options"), rec.Header().Get("Cache-Control"))
		}
	}
	rec := serve(h, "GET", "/api/v1/routes")
	if rec.Code != 401 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("GET /api/v1/routes without a token: %d, headers %v", rec.Code, rec.Header())
	}
	if rec := serve(h, "GET", "/api/v1/stub"); rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("an answer's own Cache-Control was replaced: %q", rec.Header().Get("Cache-Control"))
	}
}

func TestNewErrors(t *testing.T) {
	ok := Config{Issuer: issuer, ClientID: "admin-ui"}
	if _, err := New(fstest.MapFS{"favicon.svg": {Data: []byte("x")}}, ok, http.NotFoundHandler()); err == nil {
		t.Error("files without index.html: accepted")
	}
	for _, iss := range []string{"", "localhost:8180", "/auth/realms/spillway"} {
		if _, err := New(build, Config{Issuer: iss, ClientID: "admin-ui"}, http.NotFoundHandler()); err == nil {
			t.Errorf("issuer %q: accepted", iss)
		}
	}
	h, err := New(build, Config{Issuer: "https://id.example.com:8443/realms/x", ClientID: "admin-ui"}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if got := serve(h, "GET", "/").Header().Get("Content-Security-Policy"); !strings.Contains(got, "connect-src 'self' https://id.example.com:8443; form-action 'self' https://id.example.com:8443;") {
		t.Errorf("CSP %q: want the issuer's origin, port included", got)
	}
}
