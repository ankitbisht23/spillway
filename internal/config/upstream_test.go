package config

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHealthAndOutlierDefaults: an upstream without the blocks has no active
// checks and the default outlier ejection; set fields replace the defaults
// one by one (GW-08, GW-09, ADR-0030).
func TestHealthAndOutlierDefaults(t *testing.T) {
	tests := []struct {
		name, fields string
		health       *HealthCheck
		outlier      OutlierDetection
	}{
		{"unset", "", nil, OutlierDetection{5, 30 * time.Second, 300 * time.Second, 50}},
		{
			"path only", "health_check: {path: /healthz}",
			&HealthCheck{"/healthz", 5 * time.Second, time.Second, 3, 2}, OutlierDetection{5, 30 * time.Second, 300 * time.Second, 50},
		},
		{
			"short interval shortens the default timeout", "health_check: {path: '/status/200?x=1', interval: 500ms}",
			&HealthCheck{"/status/200?x=1", 500 * time.Millisecond, 500 * time.Millisecond, 3, 2}, OutlierDetection{5, 30 * time.Second, 300 * time.Second, 50},
		},
		{
			"everything set",
			"health_check: {path: /h, interval: 2s, timeout: 2s, unhealthy: 1, healthy: 100}, " +
				"outlier_detection: {consecutive_failures: 1000, base_ejection: 1s, max_ejection: 1s, max_ejected_percent: 100}",
			&HealthCheck{"/h", 2 * time.Second, 2 * time.Second, 1, 100}, OutlierDetection{1000, time.Second, time.Second, 100},
		},
		{
			"a long base ejection raises the default max", "outlier_detection: {base_ejection: 10m}",
			nil, OutlierDetection{5, 10 * time.Minute, 10 * time.Minute, 50},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := parse([]byte(withUpstream(tt.fields)))
			if err != nil {
				t.Fatal(err)
			}
			u := s.Upstreams["u"]
			if (u.HealthCheck == nil) != (tt.health == nil) || u.HealthCheck != nil && *u.HealthCheck != *tt.health {
				t.Errorf("health check = %+v, want %+v", u.HealthCheck, tt.health)
			}
			if u.Outlier != tt.outlier {
				t.Errorf("outlier detection = %+v, want %+v", u.Outlier, tt.outlier)
			}
		})
	}
}

func TestHealthAndOutlierRejects(t *testing.T) {
	tests := []struct{ fields, want string }{
		{"health_check: {}", `upstream "u": health_check.path "": want a path such as /healthz`},
		{"health_check: {path: healthz}", `health_check.path "healthz": want a path`},
		{"health_check: {path: '/a b'}", "want a path"},
		{"health_check: {path: '/a#b'}", "want a path"},
		{"health_check: {path: \"/a\\tb\"}", "want a path"},
		{"health_check: {path: /h, interval: 99ms}", `health_check.interval: "99ms" is not a duration from 100ms to 1h`},
		{"health_check: {path: /h, interval: 61m}", "is not a duration from 100ms to 1h"},
		{"health_check: {path: /h, interval: '5'}", "health_check.interval"},
		{"health_check: {path: /h, timeout: 0s}", "health_check.timeout: want more than 0 and at most interval"},
		{"health_check: {path: /h, interval: 1s, timeout: 2s}", "health_check.timeout: want more than 0 and at most interval"},
		{"health_check: {path: /h, timeout: soon}", "health_check.timeout"},
		{"health_check: {path: /h, unhealthy: -1}", "health_check.unhealthy: want 1 to 100"},
		{"health_check: {path: /h, healthy: 101}", "health_check.healthy: want 1 to 100"},
		{"outlier_detection: {consecutive_failures: -1}", `upstream "u": outlier_detection.consecutive_failures: want 1 to 1000`},
		{"outlier_detection: {consecutive_failures: 1001}", "outlier_detection.consecutive_failures: want 1 to 1000"},
		{"outlier_detection: {base_ejection: 999ms}", `outlier_detection.base_ejection: "999ms" is not a duration from 1s to 1h`},
		{"outlier_detection: {base_ejection: 2h}", "outlier_detection.base_ejection"},
		{"outlier_detection: {base_ejection: 1m, max_ejection: 30s}", "outlier_detection.max_ejection: want at least base_ejection"},
		{"outlier_detection: {max_ejection: 25h}", "outlier_detection.max_ejection"},
		{"outlier_detection: {max_ejected_percent: -5}", "outlier_detection.max_ejected_percent: want 1 to 100"},
		{"outlier_detection: {max_ejected_percent: 101}", "outlier_detection.max_ejected_percent: want 1 to 100"},
		{"health_check: {path: /h, every: 1s}", `unknown field "every"`},
	}
	for _, tt := range tests {
		t.Run(tt.fields, func(t *testing.T) {
			_, err := parse([]byte(withUpstream(tt.fields)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

// withUpstream is a config whose one upstream u has extra fields.
func withUpstream(fields string) string {
	if fields != "" {
		fields = ", " + fields
	}
	return "upstreams: [{name: u, endpoints: [{address: 'u:80'}]" + fields + "}]\n"
}

// TestLB: an upstream without lb uses p2c; consistent_hash takes its key
// template from hash_on, which the other algorithms reject (GW-07, ADR-0030).
func TestLB(t *testing.T) {
	tests := []struct{ fields, lb, key string }{
		{"", LBP2C, ""},
		{"lb: p2c", LBP2C, ""},
		{"lb: round_robin", LBRoundRobin, ""},
		{"lb: consistent_hash, hash_on: '{header:x-cart-id}'", LBConsistentHash, "cart-7"},
		{"lb: consistent_hash, hash_on: 'cart:{header:X-Cart-Id}:{route}'", LBConsistentHash, "cart:cart-7:r"},
	}
	for _, tt := range tests {
		t.Run(tt.fields, func(t *testing.T) {
			s, err := parse([]byte(withUpstream(tt.fields)))
			if err != nil {
				t.Fatal(err)
			}
			u := s.Upstreams["u"]
			if u.LB != tt.lb {
				t.Errorf("lb = %q, want %q", u.LB, tt.lb)
			}
			if (u.HashOn != nil) != (tt.key != "") {
				t.Fatalf("hash_on = %v, want one only for consistent_hash", u.HashOn)
			}
			if u.HashOn != nil {
				if key, _ := u.HashOn.Fill(&KeyValues{Route: "r", Header: http.Header{"X-Cart-Id": {"cart-7"}}}); key != tt.key {
					t.Errorf("hash key = %q, want %q", key, tt.key)
				}
			}
		})
	}
}

func TestLBRejects(t *testing.T) {
	tests := []struct{ fields, want string }{
		{"lb: random", `upstream "u": lb "random": want round_robin, p2c or consistent_hash`},
		{"lb: P2C", `lb "P2C": want round_robin, p2c or consistent_hash`},
		{"lb: consistent_hash", `upstream "u": hash_on is required with lb consistent_hash`},
		{"hash_on: '{ip}'", "hash_on is only for lb consistent_hash"},
		{"lb: round_robin, hash_on: '{ip}'", "hash_on is only for lb consistent_hash"},
		{"lb: consistent_hash, hash_on: '{cart}'", `hash_on: key_template "{cart}": unknown placeholder {cart}`},
		{"lb: consistent_hash, hash_on: '{header:}'", `hash_on: key_template "{header:}": header name "" is not valid`},
	}
	for _, tt := range tests {
		t.Run(tt.fields, func(t *testing.T) {
			_, err := parse([]byte(withUpstream(tt.fields)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
