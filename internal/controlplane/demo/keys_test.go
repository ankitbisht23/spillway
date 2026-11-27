package demo

import (
	"errors"
	"testing"
	"time"
)

// TestKeyExpiry checks the 30-minute rule on a fake clock (SC-08, ADR-0095
// §3): off changes nothing; on, no expiry is exactly 30 minutes from now,
// up to 30 minutes is kept, and one nanosecond more is refused.
func TestKeyExpiry(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	for _, tt := range []struct {
		name      string
		on        bool
		requested *time.Time
		want      *time.Time
		err       error
	}{
		{"off, none", false, nil, nil, nil},
		{"off, a year", false, at(365 * 24 * time.Hour), at(365 * 24 * time.Hour), nil},
		{"on, none", true, nil, at(30 * time.Minute), nil},
		{"on, 15 s", true, at(15 * time.Second), at(15 * time.Second), nil},
		{"on, exactly 30 min", true, at(30 * time.Minute), at(30 * time.Minute), nil},
		{"on, 30 min and 1 ns", true, at(30*time.Minute + 1), nil, ErrExpiryTooLate},
		{"on, 31 min", true, at(31 * time.Minute), nil, ErrExpiryTooLate},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Keys{On: tt.on, Now: clock}.Expiry(tt.requested)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error %v, want %v", err, tt.err)
			}
			if (got == nil) != (tt.want == nil) || got != nil && !got.Equal(*tt.want) {
				t.Errorf("expiry %v, want %v", got, tt.want)
			}
		})
	}
	// The clock moves: each call measures from its own now.
	k := Keys{On: true, Now: clock}
	first, _ := k.Expiry(nil)
	now = now.Add(10 * time.Minute)
	if second, _ := k.Expiry(nil); second.Sub(*first) != 10*time.Minute {
		t.Errorf("10 minutes later the expiry moved by %v", second.Sub(*first))
	}
	if got, err := (Keys{On: true}).Expiry(nil); err != nil || time.Until(*got) > KeyLifetime || time.Until(*got) < KeyLifetime-time.Minute {
		t.Errorf("the real clock: %v, %v", got, err)
	}
}
