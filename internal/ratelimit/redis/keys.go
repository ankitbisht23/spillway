package redis

import (
	"encoding/base64"
	"strconv"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// Limiter keys (docs/design.md "Redis keys", ADR-0021). The hash tag puts a
// tenant's limits in one slot, spreads global policies across slots, and
// gives each no-tenant digest its own (RL-13). v1 names the key layout and
// limiter_v1.lua together (RL-29).

// Digest returns the key digest of a filled key template: base64url,
// unpadded, of ratelimit.Key, 22 characters. Raw API keys and header values
// never appear in key names.
func Digest(filled string) string {
	return digestOf(ratelimit.Key(filled))
}

// digestOf encodes a ratelimit.Key.
func digestOf(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

// TenantKey is a tenant-, consumer- or route-level key of a request with a
// tenant: rl:v1:{t:<tenant>}:<policy>:<period_ms>:<digest>.
func TenantKey(tenant, policy string, period time.Duration, digest string) string {
	return "rl:v1:{t:" + tenant + "}:" + policy + ":" + periodMS(period) + ":" + digest
}

// GlobalKey is a global-level key: rl:v1:{g:<policy>}:<period_ms>:<digest>.
func GlobalKey(policy string, period time.Duration, digest string) string {
	return "rl:v1:{g:" + policy + "}:" + periodMS(period) + ":" + digest
}

// NoTenantKey is a tenant-, consumer- or route-level key of a request without
// a tenant: rl:v1:{k:<digest>}:<policy>:<period_ms>.
func NoTenantKey(digest, policy string, period time.Duration) string {
	return "rl:v1:{k:" + digest + "}:" + policy + ":" + periodMS(period)
}

func periodMS(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}
