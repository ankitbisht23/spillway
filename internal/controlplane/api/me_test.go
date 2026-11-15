package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// TestGetMe checks GET /me (UI-04, ADR-0058): the caller's sub, display
// name and Spillway roles in a fixed order, exactly those three fields, and
// no token in the answer or the logs. A token without a Spillway role is
// 403, as for every operation.
func TestGetMe(t *testing.T) {
	var logs bytes.Buffer
	h := newHandler(t, &logs)
	for _, tt := range []struct {
		roles string
		want  []apiv1.MeRoles
	}{
		{"viewer", []apiv1.MeRoles{"viewer"}},
		{"admin,offline_access,viewer", []apiv1.MeRoles{"viewer", "admin"}},
		{"key-admin", []apiv1.MeRoles{"key-admin"}},
	} {
		req := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+token(tt.roles))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tt.roles, rec.Code, rec.Body)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil || len(fields) != 3 {
			t.Errorf("%s: answer %s, want exactly subject, name and roles", tt.roles, rec.Body)
		}
		var me apiv1.Me
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		if me.Subject != "user-"+tt.roles || me.Name != "name-"+tt.roles || !slices.Equal(me.Roles, tt.want) {
			t.Errorf("%s: %+v, want roles %v", tt.roles, me, tt.want)
		}
		if strings.Contains(rec.Body.String(), tokenSecret) {
			t.Errorf("%s: the answer carries the token: %s", tt.roles, rec.Body)
		}
	}
	if a := do(t, h, "GET", "/api/v1/me", "", "Authorization", "Bearer "+token("operator")); a.status != http.StatusForbidden || a.err.Code != "forbidden" {
		t.Errorf("no Spillway role: %d %s, want 403 forbidden", a.status, a.err.Code)
	}
	if a := do(t, h, "GET", "/api/v1/me", "", "Authorization", ""); a.status != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", a.status)
	}
	if strings.Contains(logs.String(), tokenSecret) {
		t.Errorf("a token appears in the logs:\n%s", logs.String())
	}
}
