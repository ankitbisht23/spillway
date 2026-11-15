package api

import (
	"context"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// GetMe answers the caller as the guard resolved it from its token (UI-04,
// ADR-0058): sub, display name and Spillway roles, nothing else of the token.
// A read, so no audit record.
func (s *Server) GetMe(ctx context.Context, _ apiv1.GetMeRequestObject) (apiv1.GetMeResponseObject, error) {
	a := actorOf(ctx)
	roles := make([]apiv1.MeRoles, len(a.Roles))
	for i, r := range a.Roles {
		roles[i] = apiv1.MeRoles(r)
	}
	return apiv1.GetMe200JSONResponse{Subject: a.Sub, Name: a.Name, Roles: roles}, nil
}
