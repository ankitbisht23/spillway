package live

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// injections are values that would change a query if they reached PromQL
// unescaped, or a label matcher's meaning.
var injections = []string{
	`"}or vector(1)`,
	`r1"} or vector(1) #`,
	`r1\"}`,
	"r1\n",
	`.*`,
	`r1|orders`,
	`r1",tenant=~".*`,
	`{}`,
	`r1 `,
	strings.Repeat("a", 65),
	"",
	"ünïcode",
}

// TestValidation: every unknown name, unknown, repeated or unused
// parameter and bad value is refused before the stream starts, with a
// message that names the parameter and never echoes a value (D6, ADR-0059).
func TestValidation(t *testing.T) {
	type tc struct {
		name, query, msg string
	}
	cases := []tc{
		{"no sub", "", "sub: give 1 to 8"},
		{"empty sub", "sub=", "sub: not a named query"},
		{"unknown sub", "sub=overview.nope", "sub: not a named query"},
		{"PromQL as sub", "sub=" + url.QueryEscape(`sum(rate(gw_requests_total[1m]))`), "sub: not a named query"},
		{"sub case", "sub=Replicas", "sub: not a named query"},
		{"routes.* takes no parameter", "sub=routes.endpoints&sub=routes.circuits&range=1h", "range: none of the requested queries takes it"},
		{"repeated sub", "sub=replicas&sub=replicas", "sub: each named query at most once"},
		{"nine subs", "sub=replicas" + strings.Repeat("&sub=replicas", 8), "sub: give 1 to 8"},
		{"unknown parameter", "sub=replicas&query=up", "unknown query parameter"},
		{"token in URL", "sub=replicas&access_token=x", "unknown query parameter"},
		{"repeated route", "sub=overview.requests&route=r1&route=orders", "route: give it at most once"},
		{"unused route", "sub=replicas&route=r1", "route: none of the requested queries takes it"},
		{"route for overview.routes", "sub=overview.routes&route=r1", "route: none of the requested queries takes it"},
		{"tenant for overview.tenants", "sub=overview.tenants&tenant=acme", "tenant: none of the requested queries takes it"},
		{"unused policy_id", "sub=overview.requests&policy_id=1", "policy_id: none of the requested queries takes it"},
		{"unused range", "sub=replicas&range=1h", "range: none of the requested queries takes it"},
		{"bad range", "sub=overview.requests&range=2h", "range: want 15m, 1h, 6h, 24h or 7d"},
		{"range as PromQL", "sub=overview.requests&range=" + url.QueryEscape("1m])or vector(1)#"), "range: want"},
		{"policy_id 0", "sub=throttling.heavy_hitters&policy_id=0", "policy_id: want a policy ID"},
		{"policy_id negative", "sub=throttling.heavy_hitters&policy_id=-1", "policy_id: want a policy ID"},
		{"policy_id signed", "sub=throttling.heavy_hitters&policy_id=%2B1", "policy_id: want a policy ID"},
		{"policy_id leading zero", "sub=throttling.heavy_hitters&policy_id=01", "policy_id: want a policy ID"},
		{"policy_id huge", "sub=throttling.heavy_hitters&policy_id=99999999999999999999", "policy_id: want a policy ID"},
		{"policy_id text", "sub=throttling.heavy_hitters&policy_id=1%20or%201", "policy_id: want a policy ID"},
		{"unknown route", "sub=overview.requests&route=nope", "route: no route of that name"},
		{"unknown tenant", "sub=overview.requests&tenant=nope", "tenant: no tenant of that name"},
	}
	for _, v := range injections {
		cases = append(cases,
			tc{"route " + v, "sub=overview.requests&route=" + url.QueryEscape(v), "route: want * or a route name"},
			tc{"tenant " + v, "sub=overview.requests&tenant=" + url.QueryEscape(v), "tenant: want *, none, other or a tenant name"})
	}
	pg := newFakePG()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			r, err := parse(q)
			if err == nil {
				err = r.check(context.Background(), pg)
			}
			inv, ok := errors.AsType[*InvalidError](err)
			if !ok || !strings.HasPrefix(inv.Msg, tt.msg) {
				t.Fatalf("error %v, want %q", err, tt.msg)
			}
			for _, values := range q {
				for _, v := range values {
					if len(v) > 2 && strings.Contains(inv.Msg, v) {
						t.Errorf("the message echoes the value %q: %s", v, inv.Msg)
					}
				}
			}
		})
	}
}

// TestValid: accepted requests, with defaults and the fixed tenant values.
func TestValid(t *testing.T) {
	pg := newFakePG()
	for query, want := range map[string]string{
		"sub=overview.requests":                                   "overview.requests&route=%2A&tenant=%2A&range=15m",
		"sub=overview.requests&route=r1&tenant=acme&range=7d":     "overview.requests&route=r1&tenant=acme&range=7d",
		"sub=overview.requests&route=*&tenant=none":               "overview.requests&route=%2A&tenant=none&range=15m",
		"sub=overview.requests&tenant=other":                      "overview.requests&route=%2A&tenant=other&range=15m",
		"sub=throttling.heavy_hitters&policy_id=12&range=24h":     "throttling.heavy_hitters&range=24h&policy_id=12",
		"sub=throttling.heavy_hitters":                            "throttling.heavy_hitters&range=15m&policy_id=",
		"sub=replicas&sub=overview.tenants&route=orders&range=6h": "replicas",
		"sub=routes.endpoints&sub=routes.circuits":                "routes.endpoints",
	} {
		q, _ := url.ParseQuery(query)
		r, err := parse(q)
		if err == nil {
			err = r.check(context.Background(), pg)
		}
		if err != nil {
			t.Errorf("%s: %v", query, err)
			continue
		}
		if got := r.subs[0].key(r.params); got != want {
			t.Errorf("%s: key %q, want %q", query, got, want)
		}
	}
	// The fixed tenant values are not looked up.
	if n := pg.count("TenantExists"); n != 1 {
		t.Errorf("TenantExists called %d times, want 1 (acme only)", n)
	}
}

// TestCheckStoreError: a store error during validation is passed on (the
// API answers it, 503 when Postgres is down), not taken for invalid.
func TestCheckStoreError(t *testing.T) {
	pg := newFakePG()
	pg.setDown(true)
	q, _ := url.ParseQuery("sub=overview.requests&route=r1")
	r, err := parse(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.check(context.Background(), pg); !errors.Is(err, errPGDown) {
		t.Errorf("error %v, want the store's", err)
	}
}

// TestPromString: the one way values reach PromQL quotes and escapes
// them, whatever they hold.
func TestPromString(t *testing.T) {
	for in, want := range map[string]string{
		"r1":             `"r1"`,
		`"}or vector(1)`: `"\"}or vector(1)"`,
		`a\"}`:           `"a\\\"}"`,
		"a\nb\rc":        `"a\nb\rc"`,
		"":               `""`,
	} {
		if got := promString(in); got != want {
			t.Errorf("promString(%q) = %s, want %s", in, got, want)
		}
	}
	p := Params{Route: `"}or vector(1)`, Tenant: "*"}
	if got := matchers(p, []string{paramRoute, paramTenant}, `reason="ratelimited"`); got != `route="\"}or vector(1)",reason="ratelimited"` {
		t.Errorf("matchers = %s", got)
	}
}

func TestParsePrometheusURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://prometheus:9090": true, "https://prom.example/prefix": true,
		"": false, "prometheus:9090": false, "ftp://p": false, "http://": false,
		"http://u:p@prometheus:9090": false, "http://p/?q=1": false, "http://p/#f": false, "http://p q": false,
	} {
		if _, err := ParsePrometheusURL(raw); (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
	}
}
