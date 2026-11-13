package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/ankitbisht23/spillway/internal/auth"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// Authentication and RBAC (CP-11, CP-12, SEC-08, ADR-0040). Every
// operation of api/openapi.yaml lists its roles in x-spillway-roles; one
// handler, routed by the same paths, checks the bearer token and the roles
// before the request is read or bound, so a caller without a token or a
// role learns nothing from validation answers. An operation without roles,
// with an empty list or with only unknown role names is denied to everyone.

// Roles are Spillway's five global roles (D4, ADR-0040).
var Roles = []string{"viewer", "editor", "publisher", "key-admin", "admin"}

// Authenticator verifies a bearer token and returns who sent it. An error
// wrapping auth.ErrUnavailable is answered 503; any other error 401.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (store.Actor, error)
}

// OIDC authenticates Keycloak access tokens (CP-11): verified by v, the
// actor is the token's sub, its preferred_username as the display name
// (service-account-<client> for a confidential client), and the Spillway
// roles among realm_access.roles; other roles are ignored.
type OIDC struct{ Verifier *auth.Verifier }

// Authenticate implements Authenticator.
func (o OIDC) Authenticate(ctx context.Context, token string) (store.Actor, error) {
	c, err := o.Verifier.Verify(ctx, token)
	if err != nil {
		return store.Actor{}, err
	}
	var extra struct {
		Username    string `json:"preferred_username"`
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if err := c.Decode(&extra); err != nil || c.Subject == "" {
		return store.Actor{}, auth.ErrInvalid
	}
	return store.Actor{Sub: c.Subject, Name: cmp.Or(extra.Username, c.Subject), Roles: spillwayRoles(extra.RealmAccess.Roles)}, nil
}

// spillwayRoles is the Spillway roles among roles, in Roles' order.
func spillwayRoles(roles []string) []string {
	out := []string{}
	for _, r := range Roles {
		if slices.Contains(roles, r) {
			out = append(out, r)
		}
	}
	return out
}

// operation is one operation of the spec: its ID and the roles it allows.
type operation struct {
	id    string
	roles []string
}

// operations maps each operation's ServeMux pattern, "METHOD /api/v1/path",
// to it, from the embedded spec.
var operations = sync.OnceValue(func() map[string]operation {
	spec, err := apiv1.GetSpec()
	if err != nil {
		panic("the embedded OpenAPI spec: " + err.Error()) // compiled in; TestEveryOperationHasRoles covers it
	}
	return operationsOf(spec)
})

func operationsOf(spec *openapi3.T) map[string]operation {
	ops := map[string]operation{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			ops[method+" "+BasePath+path] = operation{id: operationID(op), roles: rolesOf(op)}
		}
	}
	return ops
}

// operationID is op's ID as api/openapi.yaml spells it: the embedded spec
// has it with the first letter upper-cased, as a Go name.
func operationID(op *openapi3.Operation) string {
	if op.OperationID == "" {
		return ""
	}
	return strings.ToLower(op.OperationID[:1]) + op.OperationID[1:]
}

// rolesOf is an operation's x-spillway-roles that are Spillway roles.
func rolesOf(op *openapi3.Operation) []string {
	raw, _ := op.Extensions["x-spillway-roles"].([]any)
	var roles []string
	for _, r := range raw {
		if s, ok := r.(string); ok && slices.Contains(Roles, s) {
			roles = append(roles, s)
		}
	}
	return roles
}

func (op operation) allows(roles []string) bool {
	for _, r := range roles {
		if slices.Contains(op.roles, r) {
			return true
		}
	}
	return false
}

type actorKey struct{}

// actorOf is the authenticated caller of a request.
func actorOf(ctx context.Context) store.Actor {
	a, _ := ctx.Value(actorKey{}).(store.Actor)
	return a
}

// errNoToken is a request without a bearer token.
var errNoToken = errors.New("no bearer token")

// guard serves op: it authenticates the request, checks its roles and only
// then hands it to next. Denials are logged with the operation, the
// caller's sub and the request ID, never the token; they are not audit
// records (ADR-0040).
func (s *Server) guard(op operation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := s.authenticate(r)
		if err != nil {
			s.unauthenticated(w, r, op, err)
			return
		}
		if !op.allows(actor.Roles) {
			s.log.Warn("request denied", "operation", op.id, "status", http.StatusForbidden, "sub", actor.Sub, "request_id", requestIDOf(r.Context()))
			msg := "forbidden: this operation is not allowed to any role"
			if len(op.roles) > 0 {
				msg = "forbidden: this operation needs one of the roles " + strings.Join(op.roles, ", ")
			}
			s.writeError(w, r, &apiError{http.StatusForbidden, apiv1.ErrorCodeForbidden, msg, nil})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
	})
}

// authenticate reads the one Authorization header, scheme Bearer in any
// case, and verifies its token.
func (s *Server) authenticate(r *http.Request) (store.Actor, error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return store.Actor{}, errNoToken
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	token = strings.TrimSpace(token)
	if len(values) > 1 || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return store.Actor{}, auth.ErrInvalid
	}
	return s.auth.Authenticate(r.Context(), token)
}

// unauthenticated answers a request whose token is missing (401), invalid
// (401 with error="invalid_token") or cannot be checked (503).
func (s *Server) unauthenticated(w http.ResponseWriter, r *http.Request, op operation, err error) {
	if errors.Is(err, auth.ErrUnavailable) {
		w.Header().Set("Retry-After", "1")
		s.writeError(w, r, &apiError{http.StatusServiceUnavailable, apiv1.ErrorCodeUnavailable, "token keys unavailable: try again", nil})
		return
	}
	s.log.Warn("request denied", "operation", op.id, "status", http.StatusUnauthorized, "request_id", requestIDOf(r.Context()))
	if errors.Is(err, errNoToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="spillway"`)
		s.writeError(w, r, &apiError{http.StatusUnauthorized, apiv1.ErrorCodeUnauthenticated, "a bearer token is required", nil})
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="spillway", error="invalid_token"`)
	s.writeError(w, r, &apiError{http.StatusUnauthorized, apiv1.ErrorCodeUnauthenticated, "invalid token", nil})
}

// audit is what the store needs for a change's audit record: the caller,
// the request ID, and show, the REST form of the rows the change touches.
func audit(ctx context.Context, show func(row any) (any, error)) store.Audit {
	return store.Audit{Actor: actorOf(ctx), RequestID: requestIDOf(ctx), Show: show}
}

// showAs returns a Show that converts rows of type R with out.
func showAs[R, T any](out func(R) (T, error)) func(any) (any, error) {
	return func(row any) (any, error) {
		r, ok := row.(R)
		if !ok {
			return nil, fmt.Errorf("audit: a %T is not a %T", row, r)
		}
		return out(r)
	}
}
