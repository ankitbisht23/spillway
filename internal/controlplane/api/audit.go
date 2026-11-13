package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// ListAudit lists audit records, newest first (CP-13, ADR-0040); the spec
// allows it to admin only. The cursor is the last record's ID: the next
// page has older records.
func (s *Server) ListAudit(ctx context.Context, req apiv1.ListAuditRequestObject) (apiv1.ListAuditResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	f := store.AuditFilter{Entity: req.Params.Entity, EntityID: req.Params.EntityId, ActorSub: req.Params.ActorSub, Since: req.Params.Since, Until: req.Params.Until}
	if f.EntityID != nil && f.Entity == nil {
		return nil, invalid("entity_id: give entity too")
	}
	if f.Since != nil && f.Until != nil && !f.Until.After(*f.Since) {
		return nil, invalid("until must be after since")
	}
	rows, err := s.store.ListAudit(ctx, p.After, p.Limit, f)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(a db.AuditLog) int64 { return a.ID })
	items := make([]apiv1.AuditRecord, len(rows))
	for i, a := range rows {
		items[i] = apiv1.AuditRecord{
			Id: a.ID, At: a.At.UTC(), Actor: apiv1.AuditActor{Sub: a.ActorSub, Name: a.ActorName, Roles: a.ActorRoles},
			Action: a.Action, Entity: a.Entity, EntityId: a.EntityID, Note: a.Note, RequestId: a.RequestID,
			ConfigVersion: a.ConfigVersion, IdentityVersion: a.IdentityVersion,
		}
		if err := json.Unmarshal(a.Diff, &items[i].Diff); err != nil {
			return nil, fmt.Errorf("audit record %d: %w", a.ID, err)
		}
	}
	return apiv1.ListAudit200JSONResponse{Items: items, NextCursor: next}, nil
}
