package config

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

func TestDrainingEndpoints(t *testing.T) {
	s, err := parse([]byte("upstreams: [{name: u, endpoints: [{address: 'a:80', draining: true}, {address: 'b:80'}]}, {name: d, endpoints: [{address: 'c:80', draining: true}]}]"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Upstreams["u"].Endpoints; len(got) != 1 || got[0] != "b:80" {
		t.Errorf("u endpoints = %v, want [b:80]: a draining endpoint gets no new requests", got)
	}
	if got := s.Upstreams["d"].Endpoints; len(got) != 0 {
		t.Errorf("d endpoints = %v, want none", got)
	}
	if _, err := parse([]byte("upstreams: [{name: u, endpoints: [{address: 'a', draining: true}]}]")); err == nil || !strings.Contains(err.Error(), "want host:port") {
		t.Errorf("a draining endpoint is still checked: error = %v", err)
	}
}

// TestCheck checks the per-entity rules the control plane applies on every
// write (ADR-0037): each entity's own rules, without the rules that span
// entities.
func TestCheck(t *testing.T) {
	tests := []struct {
		name  string
		check func() error
		want  string // "" means valid
	}{
		{"upstream", checkJSON(CheckUpstream, &configpb.Upstream{}, `{"name": "u", "endpoints": [{"address": "u:80"}]}`), ""},
		{"upstream without endpoints", checkJSON(CheckUpstream, &configpb.Upstream{}, `{"name": "u"}`), "at least one endpoint"},
		{"upstream with a bad health check", checkJSON(CheckUpstream, &configpb.Upstream{}, `{"name": "u", "endpoints": [{"address": "u:80"}], "health_check": {"path": "/", "interval": "1"}}`), "health_check.interval"},
		{"route to an upstream that does not exist", checkJSON(CheckRoute, &configpb.Route{}, `{"name": "r", "upstream": "nope", "match": {"path": "/a"}}`), ""},
		{"route without a name", checkJSON(CheckRoute, &configpb.Route{}, `{"match": {"path": "/a"}}`), "name is required"},
		{"route named none", checkJSON(CheckRoute, &configpb.Route{}, `{"name": "none", "match": {"path": "/a"}}`), "reserved"},
		{"route with an unclean path", checkJSON(CheckRoute, &configpb.Route{}, `{"name": "r", "match": {"path": "/a//b"}}`), "not clean"},
		{"route with a bad regex", checkJSON(CheckRoute, &configpb.Route{}, `{"name": "r", "match": {"path": "/", "headers": [{"name": "x", "regex": "("}]}}`), `header "x"`},
		{"route with a bad strip_prefix", checkJSON(CheckRoute, &configpb.Route{}, `{"name": "r", "match": {"path_prefix": "/v1"}, "strip_prefix": "/v2"}`), "strip_prefix"},
		{"route with auth_mode jwt", checkJSON(CheckRoute, &configpb.Route{}, `{"name": "r", "match": {"path": "/"}, "auth_mode": "jwt"}`), ""},
		{"route with an unknown auth_mode", checkJSON(CheckRoute, &configpb.Route{}, `{"name": "r", "match": {"path": "/"}, "auth_mode": "basic"}`), "want none, api_key, jwt or either"},
		{"policy on routes that do not exist", checkJSON(CheckPolicy, &configpb.Policy{}, `{"name": "p", "level": "route", "match": {"routes": ["a", "b"]}, "key_template": "{route}", "algorithm": "gcra", "limits": [{"rate": 1, "period": "1s"}]}`), ""},
		{"policy listing a route twice", checkJSON(CheckPolicy, &configpb.Policy{}, `{"name": "p", "level": "route", "match": {"routes": ["a", "a"]}, "key_template": "{route}", "algorithm": "gcra", "limits": [{"rate": 1, "period": "1s"}]}`), "listed twice"},
		{"policy with a bad limit", checkJSON(CheckPolicy, &configpb.Policy{}, `{"name": "p", "level": "global", "match": {"routes": ["*"]}, "key_template": "x", "algorithm": "gcra", "limits": [{"rate": 1, "period": "2m"}]}`), "burst: required"},
		{"policy with a bad template", checkJSON(CheckPolicy, &configpb.Policy{}, `{"name": "p", "level": "global", "match": {"routes": ["*"]}, "key_template": "{nope}", "algorithm": "gcra", "limits": [{"rate": 1, "period": "1s"}]}`), "nope"},
		{"policy from_plan", checkJSON(CheckPolicy, &configpb.Policy{}, `{"name": "p", "level": "consumer", "match": {"routes": ["*"]}, "key_template": "{consumer}", "algorithm": "gcra", "from_plan": true}`), "not supported until"},
		{"plan", checkJSON(CheckPlan, &configpb.Plan{}, `{"name": "free"}`), ""},
		{"plan with limits", checkJSON(CheckPlan, &configpb.Plan{}, `{"name": "free", "default_limits": [{"policy": "p", "limits": [{"rate": 1, "period": "1s"}]}]}`), "not supported until"},
		{"plan with a bad name", checkJSON(CheckPlan, &configpb.Plan{}, `{"name": "a b"}`), "name must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.check()
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func checkJSON[M proto.Message](check func(M) error, m M, js string) func() error {
	return func() error {
		if err := protojson.Unmarshal([]byte(js), m); err != nil {
			return err
		}
		return check(m)
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{"a": true, "A-z_0.9": true, strings.Repeat("n", 64): true, "": false, "a b": false, strings.Repeat("n", 65): false, "é": false} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v", name, got)
		}
	}
}
