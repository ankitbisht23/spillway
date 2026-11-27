// Package demo holds the control plane's demo-mode rules (SC-08,
// ADR-0095), which apply only while the control plane runs with demo mode
// on.
package demo

import (
	"errors"
	"time"
)

// KeyLifetime is how long any key the control plane issues or rotates in
// demo mode works at most (SC-08, ADR-0095 §3).
const KeyLifetime = 30 * time.Minute

// ErrExpiryTooLate is a requested expiry beyond KeyLifetime in demo mode.
var ErrExpiryTooLate = errors.New("expires_at: at most 30 minutes ahead in demo mode")

// Keys is the 30-minute rule (ADR-0095 §3): the one function every issuer
// of keys calls (issue and rotate in the REST API, and the Playground's
// key generator), so each meets it. The zero value is demo mode off, which
// changes nothing.
type Keys struct {
	On  bool
	Now func() time.Time // the control plane's clock; time.Now when nil
}

// Expiry returns the expiry of a key issued or rotated now when requested
// is asked for (nil: none). With demo mode off it is requested. On, it is
// now + KeyLifetime when requested is nil, requested when that is no later,
// and ErrExpiryTooLate otherwise. A rotation passes nil for its new key and
// ends the old key's grace at the latest at the same instant.
func (k Keys) Expiry(requested *time.Time) (*time.Time, error) {
	if !k.On {
		return requested, nil
	}
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}
	latest := now().Add(KeyLifetime)
	switch {
	case requested == nil:
		return &latest, nil
	case requested.After(latest):
		return nil, ErrExpiryTooLate
	}
	return requested, nil
}
