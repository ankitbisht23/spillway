//go:build webui

package webui

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestDist checks the embedded Vite build: index.html loads only hashed
// same-origin assets with no inline script or style, which the CSP would
// block, and each of them is served immutable.
func TestDist(t *testing.T) {
	h, err := New(Files(), Config{Issuer: issuer, ClientID: "admin-ui"}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	index := serve(h, "GET", "/").Body.String()
	if strings.Contains(index, "style=") || strings.Contains(index, "<style") || regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`).MatchString(index) {
		t.Errorf("index.html has inline script or style:\n%s", index)
	}
	refs := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(index, -1)
	if len(refs) < 2 {
		t.Fatalf("index.html loads %d assets, want a script and a stylesheet:\n%s", len(refs), index)
	}
	for _, m := range refs {
		rec := serve(h, "GET", m[1])
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != immutable {
			t.Errorf("%s: %d, Cache-Control %q", m[1], rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
}
