package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// TestLookupHeavyHittersRequests checks the lookup's answers before the
// store (ADR-0053): a bad body is 400, and no answer or log line echoes a
// subject value.
func TestLookupHeavyHittersRequests(t *testing.T) {
	var logs bytes.Buffer
	h := newHandler(t, &logs)
	const subject = "subject-dev-only-198.51.100.77"
	for _, tt := range []struct {
		name, body, want string
	}{
		{"no policy", `{"subject": {}}`, "policy_id: want a policy ID"},
		{"policy 0", `{"policy_id": 0, "subject": {}}`, "policy_id: want a policy ID"},
		{"not an IP", `{"policy_id": 1, "subject": {"ip": "` + subject + `"}}`, "subject.ip: want an IP address without a zone"},
		{"zone", `{"policy_id": 1, "subject": {"ip": "fe80::1%eth0"}}`, "subject.ip: want an IP address without a zone"},
		{"consumer 0", `{"policy_id": 1, "subject": {"consumer_id": 0}}`, "subject.consumer_id: want a consumer ID"},
		{"tenant -1", `{"policy_id": 1, "subject": {"tenant_id": -1}}`, "subject.tenant_id: want a tenant ID"},
		{"unknown field", `{"policy_id": 1, "subject": {"api_key": "` + subject + `"}}`, "unknown field"},
		{"over 7 days", `{"policy_id": 1, "subject": {}, "since": "2026-01-01T00:00:00Z", "until": "2026-01-09T00:00:00Z"}`, "at most 7 days"},
		{"empty range", `{"policy_id": 1, "subject": {}, "since": "2026-01-01T00:00:00Z", "until": "2026-01-01T00:00:00Z"}`, "until must be after since"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := do(t, h, http.MethodPost, BasePath+"/heavy-hitters/lookup", tt.body)
			if a.status != http.StatusBadRequest || a.err.Code != apiv1.ErrorCodeInvalidRequest || !strings.Contains(a.err.Error, tt.want) {
				t.Errorf("%d %s %q, want 400 %q", a.status, a.err.Code, a.err.Error, tt.want)
			}
			if strings.Contains(a.err.Error, subject) {
				t.Errorf("the answer echoes the subject: %q", a.err.Error)
			}
		})
	}
	if strings.Contains(logs.String(), subject) {
		t.Errorf("a log line shows the subject:\n%s", logs.String())
	}
}
