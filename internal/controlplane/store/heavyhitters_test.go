package store

import (
	"testing"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// TestHeavyHitterRows checks what a heartbeat's top_keys add (ADR-0041):
// one row per (policy, digest) in (policy, digest) order, duplicates summed
// with the later non-zero IDs, and keys no gateway sends dropped.
func TestHeavyHitterRows(t *testing.T) {
	const d1, d2 = "AAAAAAAAAAAAAAAAAAAAAA", "_-_-_-_-_-_-_-_-_-_-_w"
	p := heavyHitters([]*configpb.TopKey{
		{PolicyId: 1, KeyDigest: d1, ConsumerId: 3, ApiKeyId: 30, Denied: 2},
		{PolicyId: 2, KeyDigest: d1, ShadowDenied: 1},
		{PolicyId: 1, KeyDigest: d2, Denied: 1, ShadowDenied: 4},
		{PolicyId: 1, KeyDigest: d1, ShadowDenied: 5},                        // a duplicate, no IDs
		{PolicyId: 1, KeyDigest: d1, ConsumerId: 4, ApiKeyId: 40, Denied: 1}, // a duplicate, newer IDs
		{PolicyId: 0, KeyDigest: d1, Denied: 1},                              // no policy
		{PolicyId: 1, KeyDigest: "client-1", Denied: 1},                      // not a digest
		{PolicyId: 1, KeyDigest: d1 + "AA", Denied: 1},                       // too long
		{PolicyId: 1, KeyDigest: "AAAAAAAAAAAAAAAAAAAAAB", Denied: 1},        // not canonical
		{PolicyId: 1, KeyDigest: d2, Denied: -1, ShadowDenied: 3},            // negative
		{PolicyId: 3, KeyDigest: d2},                                         // nothing counted
		{PolicyId: 3, KeyDigest: d2, ConsumerId: -1, Denied: 1},              // a bad ID
	})
	if len(p.PolicyIds) != 3 {
		t.Fatalf("%d rows: %+v", len(p.PolicyIds), p)
	}
	type row struct {
		policy           int64
		digest           string
		consumer, key    int64
		denied, shadowed int64
	}
	want := []row{{1, d1, 4, 40, 3, 5}, {1, d2, 0, 0, 1, 4}, {2, d1, 0, 0, 0, 1}}
	for i, w := range want {
		if g := (row{p.PolicyIds[i], p.KeyDigests[i], p.ConsumerIds[i], p.ApiKeyIds[i], p.Denied[i], p.ShadowDenied[i]}); g != w {
			t.Errorf("row %d: %+v, want %+v", i, g, w)
		}
	}
}
