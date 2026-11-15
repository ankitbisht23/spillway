// Package webui serves the admin UI and the REST API on the control plane's
// one HTTP listener (UI-09, ADR-0057): /api/v1 is the API, /api/ui-config
// the UI's runtime config, any other /api path a JSON 404, and everything
// else the UI's files, with index.html for any path that is not a file so
// the UI's router can show it.
//
// The files come from Files: the Vite build when the binary is built with
// the webui tag (images always are), else a committed placeholder page that
// says how to build the UI, so go build and go test need no Node.
package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Config is what the UI needs before it has a token: public values only.
type Config struct {
	// Issuer is the browser-facing OIDC issuer (SPILLWAY_CP_OIDC_ISSUER,
	// ADR-0040); its origin goes into the CSP.
	Issuer string
	// ClientID is the UI's public Keycloak client (ADR-0058).
	ClientID string
}

// file is one UI file, read once at start.
type file struct {
	data  []byte
	etag  string
	ctype string
}

type handler struct {
	files    map[string]file // by path without the leading slash
	api      http.Handler
	csp      string
	uiConfig []byte
}

// New returns the listener's handler: api serves /api/v1 and answers the
// other /api paths (its own JSON 404), files are the UI's (index.html at
// their root), and cfg is served at /api/ui-config.
func New(files fs.FS, cfg Config, api http.Handler) (http.Handler, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("issuer %q: want an absolute URL", cfg.Issuer)
	}
	h := &handler{files: map[string]file{}, api: api, csp: csp(u.Scheme + "://" + u.Host)}
	if h.uiConfig, err = json.Marshal(map[string]string{"issuer": cfg.Issuer, "client_id": cfg.ClientID}); err != nil {
		return nil, err
	}
	err = fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		ctype := mime.TypeByExtension(path.Ext(name))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		h.files[name] = file{data: data, etag: `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`, ctype: ctype}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("UI files: %w", err)
	}
	if _, ok := h.files["index.html"]; !ok {
		return nil, errors.New("UI files: no index.html")
	}
	return h, nil
}

// csp is the Content-Security-Policy of every UI answer (ADR-0057):
// nothing inline, nothing from elsewhere but the issuer, which the browser
// calls for discovery, tokens and refresh, and posts the logout form to.
func csp(issuerOrigin string) string {
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
		"connect-src 'self' " + issuerOrigin + "; form-action 'self' " + issuerOrigin + "; " +
		"frame-src 'none'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; worker-src 'none'; manifest-src 'self'"
}

// Cache-Control values (ADR-0057).
const (
	immutable = "public, max-age=31536000, immutable" // content-hashed assets
	noCache   = "no-cache"                            // revalidated with the ETag
	noStore   = "no-store"                            // API answers and the runtime config
)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		if p == "/api/ui-config" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			h.securityHeaders(w)
			w.Header().Set("Cache-Control", noStore)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(h.uiConfig)
			return
		}
		// No API answer belongs in a browser's cache; an answer that sets
		// its own Cache-Control replaces this one.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", noStore)
		h.api.ServeHTTP(w, r)
		return
	}
	h.securityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(p, "/")
	f, ok := h.files[name]
	switch {
	case strings.HasPrefix(p, "/assets/"):
		// A missing script must never come back as HTML.
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", immutable)
	case ok:
		w.Header().Set("Cache-Control", noCache)
	default:
		// Any other path is a route of the UI's router, which shows Not
		// found itself for one it does not know.
		f = h.files["index.html"]
		name = "index.html"
		w.Header().Set("Cache-Control", noCache)
	}
	w.Header().Set("Content-Type", f.ctype)
	w.Header().Set("ETag", f.etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
}

// securityHeaders are set on every answer that is not the API's.
func (h *handler) securityHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", h.csp)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	hd.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()")
}
