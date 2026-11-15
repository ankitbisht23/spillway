//go:build !webui

package webui

import (
	"net/http"
	"strings"
	"testing"
)

// TestPlaceholder checks the page an untagged build serves at every UI
// path: it has no script and no inline style, which the CSP would block.
func TestPlaceholder(t *testing.T) {
	h, err := New(Files(), Config{Issuer: issuer, ClientID: "admin-ui"}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	rec := serve(h, "GET", "/overview")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "built without the admin UI") || !strings.Contains(body, "make web-build") {
		t.Errorf("%d %q", rec.Code, body)
	}
	for _, bad := range []string{"<script", "<style", "style="} {
		if strings.Contains(body, bad) {
			t.Errorf("the placeholder has %s", bad)
		}
	}
	checkSecurity(t, rec.Header())
}
