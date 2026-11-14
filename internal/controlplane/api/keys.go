package api

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/ankitbisht23/spillway/internal/apikey"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// Managed API keys (CP-15 - CP-17, ADR-0039): issue, rotate and revoke,
// each one identity change (CP-20). The key is in the issue or rotate
// answer only, with Cache-Control: no-store; listings, audit records and
// logs name a key by its ID and prefix.

// showKey is a key's REST form, for audit records: never the key or its
// hash (ADR-0040).
var showKey = showAs(func(k store.Key) (apiv1.ApiKey, error) { return keyOut(k), nil })

// noStore keeps caches from holding an answer with a key.
var noStore = "no-store"

// defaultGrace is a rotation's grace period when none is given (D2).
const defaultGrace = 24 * time.Hour

func keyOut(k store.Key) apiv1.ApiKey {
	out := apiv1.ApiKey{
		Id: k.ID, ConsumerId: k.ConsumerID, Prefix: k.Prefix, RotatedFrom: k.RotatedFrom, Version: k.Version,
		CreatedAt: k.CreatedAt.UTC(), UpdatedAt: k.UpdatedAt.UTC(),
	}
	if k.ExpiresAt != nil {
		t := k.ExpiresAt.UTC()
		out.ExpiresAt = &t
	}
	if k.RevokedAt != nil {
		t := k.RevokedAt.UTC()
		out.RevokedAt = &t
	}
	return out
}

func issuedOut(k store.Key, key apikey.Key) apiv1.IssuedApiKey {
	l := keyOut(k)
	return apiv1.IssuedApiKey{
		Id: l.Id, ConsumerId: l.ConsumerId, Prefix: l.Prefix, ExpiresAt: l.ExpiresAt, RevokedAt: l.RevokedAt,
		RotatedFrom: l.RotatedFrom, Version: l.Version, CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
		Key: key.Secret.Reveal(),
	}
}

// keyNote checks the optional note of a rotation or revocation.
func keyNote(note *string) error {
	if note != nil {
		if n := utf8.RuneCountInString(*note); n < 1 || n > maxNote {
			return validation("key", "note", fmt.Sprintf("note: 1 to %d characters when given", maxNote))
		}
	}
	return nil
}

func (s *Server) IssueKey(ctx context.Context, req apiv1.IssueKeyRequestObject) (apiv1.IssueKeyResponseObject, error) {
	var expires *time.Time
	if req.Body != nil && req.Body.ExpiresAt != nil {
		if !req.Body.ExpiresAt.After(time.Now()) {
			return nil, validation("key", "expires_at", "expires_at: want a time in the future")
		}
		expires = req.Body.ExpiresAt
	}
	k, key, err := s.store.IssueKey(ctx, audit(ctx, showKey), s.newKey, req.Id, expires)
	if err != nil {
		return nil, err
	}
	return apiv1.IssueKey201JSONResponse{Body: issuedOut(k, key), Headers: apiv1.IssueKey201ResponseHeaders{CacheControl: &noStore}}, nil
}

func (s *Server) RotateKey(ctx context.Context, req apiv1.RotateKeyRequestObject) (apiv1.RotateKeyResponseObject, error) {
	grace, note := defaultGrace, (*string)(nil)
	if req.Body != nil {
		if g := req.Body.Grace; g != nil {
			d, err := time.ParseDuration(*g)
			if err != nil || d < 0 || d > store.MaxGrace {
				return nil, validation("key", "grace", fmt.Sprintf("grace %q: want a duration from 0s to 168h", *g))
			}
			grace = d
		}
		if err := keyNote(req.Body.Note); err != nil {
			return nil, err
		}
		note = req.Body.Note
	}
	old, rotated, key, err := s.store.RotateKey(ctx, audit(ctx, showKey), s.newKey, req.Id, grace, note)
	if err != nil {
		return nil, err
	}
	return apiv1.RotateKey200JSONResponse{
		Body:    apiv1.KeyRotation{Old: keyOut(old), New: issuedOut(rotated, key)},
		Headers: apiv1.RotateKey200ResponseHeaders{CacheControl: &noStore},
	}, nil
}

func (s *Server) RevokeKey(ctx context.Context, req apiv1.RevokeKeyRequestObject) (apiv1.RevokeKeyResponseObject, error) {
	var note *string
	if req.Body != nil {
		if err := keyNote(req.Body.Note); err != nil {
			return nil, err
		}
		note = req.Body.Note
	}
	k, err := s.store.RevokeKey(ctx, audit(ctx, showKey), req.Id, note)
	if err != nil {
		return nil, err
	}
	return apiv1.RevokeKey200JSONResponse(keyOut(k)), nil
}
