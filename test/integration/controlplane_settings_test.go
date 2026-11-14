//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// TestControlPlaneSettings checks the config settings, the working set's
// ratelimit section (ADR-0045): read by every role, replaced by editors and
// admins with If-Match, checked like a config file's, a config edit that
// bumps the working-set revision and is audited, shown in the preview's
// diff (not its changes list, whose entity enum is closed), published in
// every snapshot, exported, and rolled back with config.
func TestControlPlaneSettings(t *testing.T) {
	c, iss, _ := startControlPlane(t, io.Discard)
	r := newRoles(t, c, iss)

	var st apiv1.ConfigSettings
	a := r.viewer.must(200, "GET", "/config/settings", nil)
	a.decode(t, &st)
	if a.etag() != `"1"` || st.Ratelimit.Replicas != nil || st.Ratelimit.RedisBudget != nil {
		t.Fatalf("new settings: ETag %s, %+v", a.etag(), st)
	}
	three := map[string]any{"ratelimit": map[string]any{"replicas": 3, "redis_budget": "8ms"}}
	for _, cl := range []cpClient{r.viewer, r.publisher, r.keyAdmin} {
		cl.must(403, "PUT", "/config/settings", three, "If-Match", `"1"`)
	}
	r.editor.must(428, "PUT", "/config/settings", three)
	r.editor.must(412, "PUT", "/config/settings", three, "If-Match", `"7"`)
	if e := r.editor.must(422, "PUT", "/config/settings", map[string]any{"ratelimit": map[string]any{"redis_budget": "2s"}}, "If-Match", `"1"`).apiErr(t); !strings.Contains(e.Error, "ratelimit.redis_budget") {
		t.Errorf("a budget over 1s: %+v", e)
	}
	r.editor.must(400, "PUT", "/config/settings", map[string]any{"ratelimit": map[string]any{"replica": 3}}, "If-Match", `"1"`)

	a = r.editor.must(200, "PUT", "/config/settings", three, "If-Match", `"1"`)
	a.decode(t, &st)
	if a.etag() != `"2"` || *st.Version != 2 || *st.Ratelimit.Replicas != 3 || *st.Ratelimit.RedisBudget != "8ms" {
		t.Fatalf("PUT: ETag %s, %+v", a.etag(), st)
	}
	v, tag := validate(t, r.viewer)
	if tag != `"2"` || !v.Valid || len(v.Changes) != 0 || !strings.Contains(v.Diff, "+ratelimit:\n+  replicas: 3\n") {
		t.Fatalf("validate after the PUT: %s %+v", tag, v)
	}
	r.publisher.must(200, "POST", "/config/publish", note("three replicas"), "If-Match", tag)
	if e, _ := exportYAML(t, r.viewer, "?version=1"); e != "version: 1\nratelimit:\n  replicas: 3\n  redis_budget: 8ms\n" {
		t.Errorf("version 1:\n%s", e)
	}

	// A second change, published, then a rollback to version 1 restores
	// the settings with the rest of config.
	r.admin.must(200, "PUT", "/config/settings", map[string]any{"ratelimit": map[string]any{}}, "If-Match", `"2"`)
	_, tag = validate(t, r.viewer)
	r.publisher.must(200, "POST", "/config/publish", note("defaults"), "If-Match", tag)
	if e, _ := exportYAML(t, r.viewer, ""); e != "{}\n" {
		t.Errorf("working set after the second publish:\n%s", e)
	}
	r.publisher.must(200, "POST", "/config/versions/1/rollback", note("back to three"), "If-Match", tag)
	a = r.viewer.must(200, "GET", "/config/settings", nil)
	a.decode(t, &st)
	if st.Ratelimit.Replicas == nil || *st.Ratelimit.Replicas != 3 || a.etag() != `"4"` {
		t.Errorf("after the rollback: ETag %s, %+v", a.etag(), st)
	}

	// One record per PUT, with the settings before and after.
	recs := auditPage(t, r.admin, "?entity=settings").Items
	if len(recs) != 2 || recs[1].Action != "settings.update" || recs[1].Actor.Sub != "sub-editor" || *recs[1].EntityId != "1" {
		t.Fatalf("settings records: %+v", recs)
	}
	diff, _ := json.Marshal(recs[1].Diff)
	if !strings.Contains(string(diff), `"after":{"ratelimit":{"redis_budget":"8ms","replicas":3}`) || !strings.Contains(string(diff), `"before":{"ratelimit":{}`) {
		t.Errorf("settings record diff %s", diff)
	}
}
