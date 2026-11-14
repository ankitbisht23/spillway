package api

import (
	"context"
	"encoding/base64"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// Gateway replicas, from their heartbeats (CP-08, ADR-0038).

// ListReplicas lists the replicas by name; the cursor is base64url of the
// last name of the page.
func (s *Server) ListReplicas(ctx context.Context, req apiv1.ListReplicasRequestObject) (apiv1.ListReplicasResponseObject, error) {
	p, err := page(req.Params.Limit, nil)
	if err != nil {
		return nil, err
	}
	var after string
	if c := req.Params.Cursor; c != nil {
		b, err := base64.RawURLEncoding.DecodeString(*c)
		if err != nil || len(b) == 0 {
			return nil, invalid("cursor: not a cursor this API gave")
		}
		after = string(b)
	}
	rows, err := s.store.ListReplicas(ctx, after, p.Limit)
	if err != nil {
		return nil, err
	}
	var next *string
	if len(rows) > int(p.Limit) {
		rows = rows[:p.Limit]
		c := base64.RawURLEncoding.EncodeToString([]byte(rows[len(rows)-1].ID))
		next = &c
	}
	items := make([]apiv1.Replica, len(rows))
	for i, r := range rows {
		items[i] = replicaOf(r)
	}
	return apiv1.ListReplicas200JSONResponse{Items: items, NextCursor: next}, nil
}

func replicaOf(r store.Replica) apiv1.Replica {
	out := apiv1.Replica{
		Id: r.ID, ConfigVersion: r.ConfigVersion, IdentityVersion: r.IdentityVersion,
		SchemaVersion:  apiv1.SchemaVersion{Major: r.SchemaMajor, Minor: r.SchemaMinor},
		WatchConnected: r.WatchConnected, LastHeartbeatAt: r.LastHeartbeatAt.UTC(), Live: r.Live,
		RejectedConfigVersion: r.RejectedConfigVersion, RejectedReason: r.RejectedReason,
	}
	if r.LastSyncedAt != nil {
		t := r.LastSyncedAt.UTC()
		out.LastSyncedAt = &t
	}
	return out
}
