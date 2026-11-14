package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/telemetry"
)

// authFailure is the gateway's answer to a request whose credential is
// missing, invalid or cannot be checked (ADR-0039). It says nothing about
// the credential itself.
type authFailure struct {
	status    int
	msg       string
	challenge string // WWW-Authenticate; empty for none
	result    string // the authn span's spillway.authn.result
}

//nolint:gosec // G101: WWW-Authenticate challenges, not credentials
const (
	bearerChallenge  = `Bearer realm="spillway"`
	invalidChallenge = `Bearer realm="spillway", error="invalid_token"`
)

var (
	errKeyRequired        = &authFailure{http.StatusUnauthorized, "API key required", "", "missing"}
	errInvalidKey         = &authFailure{http.StatusUnauthorized, "invalid API key", "", "invalid"}
	errTokenRequired      = &authFailure{http.StatusUnauthorized, "bearer token required", bearerChallenge, "missing"}
	errCredentialRequired = &authFailure{http.StatusUnauthorized, "API key or bearer token required", bearerChallenge, "missing"}
	errInvalidToken       = &authFailure{http.StatusUnauthorized, "invalid token", invalidChallenge, "invalid"}
	errTokenKeys          = &authFailure{http.StatusServiceUnavailable, "token keys unavailable", "", "unavailable"}
)

// write answers with f: a gateway answer, reason none (ADR-0039).
func (f *authFailure) write(w http.ResponseWriter) {
	if f.challenge != "" {
		w.Header().Set("WWW-Authenticate", f.challenge)
	}
	if f.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	jsonError(w, f.status, f.msg)
}

// SetTokens makes the handler verify bearer tokens with v (GW-18, SEC-04).
// Without it, a route that needs a token answers 503; the gateway refuses
// such a config before it applies (ADR-0039). Call it before serving.
func (h *Handler) SetTokens(v *auth.Verifier) { h.tokens = v }

// authenticate resolves a request's credential as its route's auth mode
// says (GW-17 - GW-20, ADR-0039) and returns its consumer, nil for an
// anonymous request, and the API key that resolved it, empty for a token.
//
//   - An X-API-Key that is sent is resolved on every mode but jwt, and
//     decides: one value that resolves gives its consumer, anything else is
//     401. Its hash, never the key, goes in q for the access log.
//   - jwt, and either without a key, verify the one Authorization: Bearer
//     token and find the consumer by its subject.
//   - With neither, none is anonymous and the others are 401.
//
// The other credential's header is not read and goes upstream unchanged.
func (h *Handler) authenticate(ctx context.Context, mode string, header http.Header, q *telemetry.Request) (*config.Consumer, string, *authFailure) {
	if mode != config.AuthJWT {
		if keys := header.Values("X-API-Key"); len(keys) > 0 {
			q.KeyHash = config.KeyHash(keys[0])
			if len(keys) != 1 {
				return nil, "", errInvalidKey
			}
			c, ok := h.identity().Lookup(keys[0])
			if !ok {
				return nil, "", errInvalidKey
			}
			return c, keys[0], nil
		}
	}
	switch mode {
	case config.AuthAPIKey:
		return nil, "", errKeyRequired
	case config.AuthJWT, config.AuthEither:
	default:
		return nil, "", nil
	}
	values := header.Values("Authorization")
	if len(values) == 0 {
		if mode == config.AuthJWT {
			return nil, "", errTokenRequired
		}
		return nil, "", errCredentialRequired
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	token = strings.TrimSpace(token)
	if len(values) > 1 || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, "", errInvalidToken
	}
	if h.tokens == nil {
		return nil, "", errTokenKeys
	}
	claims, err := h.tokens.Verify(ctx, token)
	switch {
	case errors.Is(err, auth.ErrUnavailable):
		return nil, "", errTokenKeys
	case err != nil:
		return nil, "", errInvalidToken
	}
	c, ok := h.identity().Subject(claims.Subject)
	if !ok {
		return nil, "", errInvalidToken
	}
	return c, "", nil
}
