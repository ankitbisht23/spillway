package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/proxy"
)

func spec(t *testing.T) *openapi3.T {
	t.Helper()
	s, err := apiv1.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeResolver resolves host names from a map; others fail.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if ips, ok := f[host]; ok {
		return ips, nil
	}
	return nil, errors.New("no such host")
}

// newHandler is the API without a store: a request that reaches the store
// panics, which handler turns into a 599 answer, so these tests see every
// answer the API gives before it touches Postgres.
func newHandler(t *testing.T, log io.Writer) http.Handler {
	t.Helper()
	allowed, err := proxy.ParseAllowlist("172.29.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	res := fakeResolver{
		"upstream-1": {netip.MustParseAddr("172.29.0.10")},
		"metadata":   {netip.MustParseAddr("172.29.0.11"), netip.MustParseAddr("169.254.169.254")},
		"outside":    {netip.MustParseAddr("10.1.2.3")},
	}
	h := New(nil, allowed, res, slog.New(slog.NewJSONHandler(log, nil))).Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				w.WriteHeader(599)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

type answer struct {
	status int
	header http.Header
	err    apiv1.Error
}

func do(t *testing.T, h http.Handler, method, path, body string, header ...string) answer {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	a := answer{status: rec.Code, header: rec.Header()}
	if rec.Code >= 400 && rec.Code < 599 {
		if err := json.Unmarshal(rec.Body.Bytes(), &a.err); err != nil {
			t.Fatalf("%s %s: %d with a body that is not the error JSON: %q", method, path, rec.Code, rec.Body.String())
		}
	}
	return a
}

// TestEveryOperationIsServed sends a request to every operation of
// api/openapi.yaml and checks that the server routes it (CP-10): the strict
// server interface makes the compiler check that each has a handler, and
// this checks that each path and method reach it.
func TestEveryOperationIsServed(t *testing.T) {
	h := newHandler(t, io.Discard)
	n := 0
	for path, item := range spec(t).Paths.Map() {
		for method, op := range item.Operations() {
			n++
			url := BasePath + strings.NewReplacer("{id}", "1", "{eid}", "2").Replace(path)
			body := ""
			if op.RequestBody != nil {
				body = "{}"
			}
			a := do(t, h, method, url, body, "If-Match", `"1"`)
			if a.status == http.StatusMethodNotAllowed || a.status == http.StatusNotFound && a.err.Error == "no such path" {
				t.Errorf("%s %s (%s): %d %q, not served", method, url, op.OperationID, a.status, a.err.Error)
			}
		}
	}
	if n < 42 {
		t.Errorf("the spec has %d operations; M5's CRUD paths, key listing and drain are 42", n)
	}
}

// TestEveryOperationHasRoles checks x-spillway-roles, which RBAC reads
// (ADR-0040): every operation lists known roles, admin among them.
func TestEveryOperationHasRoles(t *testing.T) {
	known := []string{"viewer", "editor", "publisher", "key-admin", "admin"}
	for path, item := range spec(t).Paths.Map() {
		for method, op := range item.Operations() {
			raw, ok := op.Extensions["x-spillway-roles"].([]any)
			if !ok || len(raw) == 0 {
				t.Errorf("%s %s: no x-spillway-roles", method, path)
				continue
			}
			var roles []string
			for _, r := range raw {
				s, _ := r.(string)
				if !slices.Contains(known, s) {
					t.Errorf("%s %s: unknown role %v", method, path, r)
				}
				roles = append(roles, s)
			}
			if !slices.Contains(roles, "admin") {
				t.Errorf("%s %s: roles %v leave out admin", method, path, roles)
			}
			if op.OperationID == "" {
				t.Errorf("%s %s: no operationId", method, path)
			}
		}
	}
}

// TestProtoParity checks that each config entity schema has exactly its
// config.proto message's fields, with matching types, besides the REST-only
// version, created_at and updated_at (CP-21, ADR-0036).
func TestProtoParity(t *testing.T) {
	schemas := spec(t).Components.Schemas
	for name, m := range map[string]proto.Message{
		"Upstream": &configpb.Upstream{}, "Route": &configpb.Route{}, "Policy": &configpb.Policy{}, "Plan": &configpb.Plan{},
	} {
		parity(t, name, schemas[name], m.ProtoReflect().Descriptor())
	}
}

func parity(t *testing.T, name string, ref *openapi3.SchemaRef, md protoreflect.MessageDescriptor) {
	t.Helper()
	if ref == nil || ref.Value == nil {
		t.Errorf("%s: no schema for %s", name, md.FullName())
		return
	}
	s := ref.Value
	if s.AdditionalProperties.Has == nil || *s.AdditionalProperties.Has {
		t.Errorf("%s: additionalProperties must be false (unknown fields are 400)", name)
	}
	fields := md.Fields()
	for prop, p := range s.Properties {
		if prop == "version" || prop == "created_at" || prop == "updated_at" {
			continue
		}
		fd := fields.ByName(protoreflect.Name(prop))
		if fd == nil {
			t.Errorf("%s.%s: not a field of %s", name, prop, md.FullName())
			continue
		}
		v := p.Value
		if fd.IsList() {
			if !v.Type.Is("array") {
				t.Errorf("%s.%s: repeated in the proto, %v in the spec", name, prop, v.Type)
				continue
			}
			p, v = v.Items, v.Items.Value
		}
		want := map[protoreflect.Kind][2]string{
			protoreflect.StringKind: {"string", ""}, protoreflect.BoolKind: {"boolean", ""},
			protoreflect.Int32Kind: {"integer", "int32"}, protoreflect.Int64Kind: {"integer", "int64"},
			protoreflect.DoubleKind: {"number", "double"}, protoreflect.MessageKind: {"object", ""},
		}[fd.Kind()]
		if !v.Type.Is(want[0]) || v.Format != want[1] {
			t.Errorf("%s.%s: %v %q in the spec, want %s %q for %v", name, prop, v.Type, v.Format, want[0], want[1], fd.Kind())
		}
		if fd.Kind() == protoreflect.MessageKind {
			parity(t, name+"."+prop, p, fd.Message())
		}
	}
	for i := range fields.Len() {
		if _, ok := s.Properties[string(fields.Get(i).Name())]; !ok {
			t.Errorf("%s: field %s of %s is not in the spec", name, fields.Get(i).Name(), md.FullName())
		}
	}
}

// TestRequestErrors checks the answers the API gives before it reaches the
// store (CP-14, ADR-0036): the error body, request IDs, strict bodies, the
// body cap, If-Match, pages and entity rules.
func TestRequestErrors(t *testing.T) {
	h := newHandler(t, io.Discard)
	const route = `{"name": "r", "upstream": "u", "match": {"path": "/a"}}`
	tests := []struct {
		name, method, path, body string
		header                   []string
		status                   int
		code                     apiv1.ErrorCode
		msg                      string
	}{
		{"unknown path", "GET", "/api/v1/nope", "", nil, 404, apiv1.ErrorCodeNotFound, "no such path"},
		{"outside the API", "GET", "/", "", nil, 404, apiv1.ErrorCodeNotFound, "no such path"},
		{"id not a number", "GET", "/api/v1/routes/abc", "", nil, 400, apiv1.ErrorCodeInvalidRequest, "id"},
		{"malformed JSON", "POST", "/api/v1/routes", `{"name": `, nil, 400, apiv1.ErrorCodeInvalidRequest, "body: unexpected EOF"},
		{"unknown field", "POST", "/api/v1/routes", `{"name": "r", "upstrem": "u"}`, nil, 400, apiv1.ErrorCodeInvalidRequest, `unknown field "upstrem"`},
		{"unknown nested field", "POST", "/api/v1/routes", `{"name": "r", "match": {"path": "/", "timeout": "1s"}}`, nil, 400, apiv1.ErrorCodeInvalidRequest, `unknown field "timeout"`},
		{"wrong type", "POST", "/api/v1/routes", `{"name": "r", "priority": "high"}`, nil, 400, apiv1.ErrorCodeInvalidRequest, "priority: wrong type, want an integer"},
		{"data after the body", "POST", "/api/v1/routes", route + ` {}`, nil, 400, apiv1.ErrorCodeInvalidRequest, "data after the JSON value"},
		{"empty body", "POST", "/api/v1/plans", "", []string{"Content-Type", "application/json"}, 400, apiv1.ErrorCodeInvalidRequest, "body: required"},
		{"not JSON", "POST", "/api/v1/routes", route, []string{"Content-Type", "text/plain"}, 415, apiv1.ErrorCodeUnsupportedMediaType, "application/json"},
		{"too large", "POST", "/api/v1/routes", `{"name": "` + strings.Repeat("x", MaxBody) + `"}`, nil, 413, apiv1.ErrorCodePayloadTooLarge, "over 1048576 bytes"},
		{"PUT without If-Match", "PUT", "/api/v1/routes/1", route, nil, 428, apiv1.ErrorCodePreconditionRequired, "If-Match is required"},
		{"DELETE without If-Match", "DELETE", "/api/v1/tenants/1", "", nil, 428, apiv1.ErrorCodePreconditionRequired, "If-Match is required"},
		{"drain without If-Match", "POST", "/api/v1/upstreams/1/endpoints/2/drain", "", nil, 428, apiv1.ErrorCodePreconditionRequired, "If-Match is required"},
		{"limit 0", "GET", "/api/v1/routes?limit=0", "", nil, 400, apiv1.ErrorCodeInvalidRequest, "limit: want 1 to 500"},
		{"limit 501", "GET", "/api/v1/consumers?limit=501", "", nil, 400, apiv1.ErrorCodeInvalidRequest, "limit: want 1 to 500"},
		{"bad cursor", "GET", "/api/v1/routes?cursor=!!", "", nil, 400, apiv1.ErrorCodeInvalidRequest, "cursor"},
		{"cursor not an ID", "GET", "/api/v1/routes?cursor=YWJj", "", nil, 400, apiv1.ErrorCodeInvalidRequest, "cursor"},
		{"unknown action filter", "GET", "/api/v1/access-lists?action=block", "", nil, 400, apiv1.ErrorCodeInvalidRequest, "action"},
		{"bad name", "POST", "/api/v1/routes", `{"name": "a b", "upstream": "u", "match": {"path": "/"}}`, nil, 422, apiv1.ErrorCodeValidationFailed, "name must be 1-64 characters"},
		{"route rule", "POST", "/api/v1/routes", `{"name": "r", "upstream": "u", "match": {"path": "/a", "path_prefix": "/a"}}`, nil, 422, apiv1.ErrorCodeValidationFailed, "exactly one of match.path"},
		{"route before its task", "POST", "/api/v1/routes", `{"name": "r", "upstream": "u", "match": {"path": "/a"}, "auth_mode": "jwt"}`, nil, 422, apiv1.ErrorCodeValidationFailed, "not supported until"},
		{"upstream rule", "POST", "/api/v1/upstreams", `{"name": "u", "endpoints": [{"address": "upstream-1"}]}`, nil, 422, apiv1.ErrorCodeValidationFailed, "want host:port"},
		{"upstream IP outside the allow-list", "POST", "/api/v1/upstreams", `{"name": "u", "endpoints": [{"address": "10.0.0.1:80"}]}`, nil, 422, apiv1.ErrorCodeValidationFailed, "not in the allowed upstream networks"},
		{"upstream at the metadata address", "PUT", "/api/v1/upstreams/1", `{"name": "u", "endpoints": [{"address": "169.254.169.254:80"}]}`, []string{"If-Match", `"1"`}, 422, apiv1.ErrorCodeValidationFailed, "not in the allowed upstream networks"},
		{"upstream name resolving outside", "POST", "/api/v1/upstreams", `{"name": "u", "endpoints": [{"address": "outside:80"}]}`, nil, 422, apiv1.ErrorCodeValidationFailed, "resolves to 10.1.2.3"},
		{"upstream name resolving to metadata too", "POST", "/api/v1/upstreams", `{"name": "u", "endpoints": [{"address": "metadata:80"}]}`, nil, 422, apiv1.ErrorCodeValidationFailed, "resolves to 169.254.169.254"},
		{"policy rule", "POST", "/api/v1/policies", `{"name": "p", "level": "user"}`, nil, 422, apiv1.ErrorCodeValidationFailed, `level "user"`},
		{"plan before its task", "POST", "/api/v1/plans", `{"name": "free", "default_limits": [{"policy": "p", "limits": [{"rate": 1, "period": "1s"}]}]}`, nil, 422, apiv1.ErrorCodeValidationFailed, "not supported until"},
		{"tenant without a plan", "POST", "/api/v1/tenants", `{"name": "acme"}`, nil, 422, apiv1.ErrorCodeValidationFailed, "plan_id is required"},
		{"tenant status", "POST", "/api/v1/tenants", `{"name": "acme", "plan_id": 1, "status": "gone"}`, nil, 422, apiv1.ErrorCodeValidationFailed, `status "gone"`},
		{"consumer without a tenant", "POST", "/api/v1/consumers", `{"name": "bot"}`, nil, 422, apiv1.ErrorCodeValidationFailed, "tenant_id is required"},
		{"consumer with an empty jwt_subject", "POST", "/api/v1/consumers", `{"name": "bot", "tenant_id": 1, "jwt_subject": ""}`, nil, 422, apiv1.ErrorCodeValidationFailed, "jwt_subject"},
		{"access list subject missing", "POST", "/api/v1/access-lists", `{"action": "deny", "subject_type": "cidr"}`, nil, 422, apiv1.ErrorCodeValidationFailed, "set cidr and none of the other subjects"},
		{"access list two subjects", "POST", "/api/v1/access-lists", `{"action": "deny", "subject_type": "key", "api_key_id": 1, "consumer_id": 2}`, nil, 422, apiv1.ErrorCodeValidationFailed, "set api_key_id and none"},
		{"access list host bits", "POST", "/api/v1/access-lists", `{"action": "deny", "subject_type": "cidr", "cidr": "203.0.113.7/24"}`, nil, 422, apiv1.ErrorCodeValidationFailed, "want 203.0.113.0/24"},
		{"access list bad CIDR", "POST", "/api/v1/access-lists", `{"action": "allow", "subject_type": "cidr", "cidr": "nope"}`, nil, 422, apiv1.ErrorCodeValidationFailed, "want a CIDR"},
		{"access list action", "POST", "/api/v1/access-lists", `{"action": "block", "subject_type": "cidr", "cidr": "10.0.0.0/8"}`, nil, 422, apiv1.ErrorCodeValidationFailed, `action "block"`},
		{"override without limits", "POST", "/api/v1/overrides", `{"consumer_id": 1, "policy_id": 2, "limits": []}`, nil, 422, apiv1.ErrorCodeValidationFailed, "at least one"},
		{"override ends before it starts", "POST", "/api/v1/overrides", `{"consumer_id": 1, "policy_id": 2, "limits": [{"rate": 5, "period": "1s"}], "starts_at": "2026-11-14T10:00:00Z", "ends_at": "2026-11-14T09:00:00Z"}`, nil, 422, apiv1.ErrorCodeValidationFailed, "ends_at must be after starts_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := do(t, h, tt.method, tt.path, tt.body, append([]string{"X-Request-Id", "req-1"}, tt.header...)...)
			if a.status != tt.status || a.err.Code != tt.code || !strings.Contains(a.err.Error, tt.msg) {
				t.Errorf("%d %s %q, want %d %s with %q", a.status, a.err.Code, a.err.Error, tt.status, tt.code, tt.msg)
			}
			if a.err.RequestId != "req-1" || a.header.Get("X-Request-Id") != "req-1" {
				t.Errorf("request ID %q, header %q, want req-1 in both", a.err.RequestId, a.header.Get("X-Request-Id"))
			}
			if a.header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type %q", a.header.Get("Content-Type"))
			}
		})
	}
}

func TestValidationDetails(t *testing.T) {
	a := do(t, newHandler(t, io.Discard), "POST", "/api/v1/upstreams", `{"name": "u", "endpoints": [{"address": "10.0.0.1:80"}]}`)
	if a.err.Details == nil || len(*a.err.Details) != 1 {
		t.Fatalf("details = %v", a.err.Details)
	}
	d := (*a.err.Details)[0]
	if *d.Entity != "upstream" || *d.Path != "endpoints" || !strings.Contains(*d.Message, "10.0.0.1:80") {
		t.Errorf("detail = entity %q, path %q, message %q", *d.Entity, *d.Path, *d.Message)
	}
}

func TestRequestIDGenerated(t *testing.T) {
	a := do(t, newHandler(t, io.Discard), "GET", "/api/v1/nope", "", "X-Request-Id", "bad id with spaces")
	if id := a.header.Get("X-Request-Id"); id == "" || id == "bad id with spaces" || a.err.RequestId != id {
		t.Errorf("request ID %q in the header, %q in the body: want a new one in both", id, a.err.RequestId)
	}
}

func TestIfMatch(t *testing.T) {
	for h, want := range map[string]int64{`"7"`: 7, `"0"`: 0, `W/"7"`: -1, `*`: -1, `7`: -1, `"-1"`: -1, `"7", "8"`: -1, `""`: -1} {
		if got := ifMatch(h); got != want {
			t.Errorf("ifMatch(%s) = %d, want %d", h, got, want)
		}
	}
}

func TestPageCursorRoundTrip(t *testing.T) {
	rows := []int64{3, 5, 9}
	p, err := page(new(int32(2)), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, next := pageOf(rows, p, func(id int64) int64 { return id })
	if len(got) != 2 || next == nil {
		t.Fatalf("page = %v, next %v", got, next)
	}
	p, err = page(nil, next)
	if err != nil || p.After != 5 || p.Limit != defaultLimit {
		t.Errorf("next page = %+v, %v; want after 5, limit %d", p, err, defaultLimit)
	}
	if _, next := pageOf(rows[:2], store.Page{Limit: 2}, func(id int64) int64 { return id }); next != nil {
		t.Errorf("a full last page has next %q", *next)
	}
}
