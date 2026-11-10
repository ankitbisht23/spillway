package ratelimit

import (
	"crypto/sha256"
	"testing"
	"time"
)

func TestKey(t *testing.T) {
	k := Key("acme:orders")
	sum := sha256.Sum256([]byte("acme:orders"))
	if k != string(sum[:16]) {
		t.Fatalf("Key is not the first 16 bytes of SHA-256")
	}
	if Key("acme:users") == k {
		t.Fatalf("different templates gave the same key")
	}
}

func TestFakeClock(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	c := NewFakeClock(start)
	if !c.Now().Equal(start) {
		t.Fatalf("Now = %v, want %v", c.Now(), start)
	}
	c.Advance(1500 * time.Millisecond)
	if got := c.Now().Sub(start); got != 1500*time.Millisecond {
		t.Fatalf("after Advance: moved %v", got)
	}
}
