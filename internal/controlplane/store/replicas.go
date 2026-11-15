package store

import (
	"context"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Gateway replicas (CP-08, CP-22, RL-24, ADR-0038): heartbeats, the live
// replica count, the replicas list, the replicas that applied a version and
// the hourly prune.

// Replica is a replica's row and whether its last heartbeat is under 15 s
// old.
type Replica = db.ListReplicasRow

// Applied is a replica's first report of a version (NFR-08).
type Applied = db.ConfigVersionAppliedRow

// maxReason bounds a stored rejected_reason, in bytes.
const maxReason = 2000

// RecordHeartbeat records replica's heartbeat hb in one transaction (CP-08,
// ADR-0038): its row in replicas, the first report of each of its
// versions in replica_versions, its top_keys in the current minute's
// heavy_hitters windows (RL-21, ADR-0041), and returns live_replicas, the
// replicas whose last heartbeat is under 15 s old, replica included
// (RL-24).
func (s *Store) RecordHeartbeat(ctx context.Context, replica string, hb *configpb.HeartbeatRequest) (int32, error) {
	var live int32
	err := s.inTx(ctx, func(q *db.Queries) error {
		p := db.UpsertReplicaParams{
			ID: replica, ConfigVersion: hb.GetConfigVersion(), IdentityVersion: hb.GetIdentityVersion(),
			SchemaMajor: hb.GetSchemaVersion().GetMajor(), SchemaMinor: hb.GetSchemaVersion().GetMinor(),
			WatchConnected: hb.GetWatchConnected(),
		}
		if v := hb.GetRejectedConfigVersion(); v != 0 {
			p.RejectedConfigVersion = &v
		}
		if r := hb.GetRejectedReason(); r != "" {
			r = truncate(r, maxReason)
			p.RejectedReason = &r
		}
		if err := q.UpsertReplica(ctx, p); err != nil {
			return err
		}
		if err := q.InsertReplicaVersions(ctx, db.InsertReplicaVersionsParams{
			ReplicaID: replica, ConfigVersion: hb.GetConfigVersion(), IdentityVersion: hb.GetIdentityVersion(),
		}); err != nil {
			return err
		}
		if hh := heavyHitters(hb.GetTopKeys()); len(hh.PolicyIds) > 0 {
			if err := q.AddHeavyHitters(ctx, hh); err != nil {
				return err
			}
		}
		var err error
		live, err = q.LiveReplicas(ctx)
		return err
	})
	return live, err
}

// truncate cuts s to at most n bytes, dropping a rune cut in two.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// ListReplicas returns the replicas named after after, at most limit+1 of
// them, by name.
func (s *Store) ListReplicas(ctx context.Context, after string, limit int32) ([]Replica, error) {
	return db.New(s.pool).ListReplicas(ctx, db.ListReplicasParams{After: after, Lim: limit + 1})
}

// ReplicaStaleness is a replica's row and its staleness on Postgres's
// clock: clock_timestamp() minus last_synced_at, nil when it never synced
// (CP-08, ADR-0059).
type ReplicaStaleness struct {
	Replica   Replica
	Staleness *time.Duration
}

// replicasPage is AllReplicas's page size.
const replicasPage = 500

// AllReplicas returns every replica by name with its staleness, reading
// pages of 500 (the live-data stream, ADR-0059).
func (s *Store) AllReplicas(ctx context.Context) ([]ReplicaStaleness, error) {
	q := db.New(s.pool)
	var out []ReplicaStaleness
	for after := ""; ; {
		rows, err := q.ListReplicasClock(ctx, db.ListReplicasClockParams{After: after, Lim: replicasPage})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			rs := ReplicaStaleness{Replica: Replica{
				ID: r.ID, ConfigVersion: r.ConfigVersion, IdentityVersion: r.IdentityVersion, SchemaMajor: r.SchemaMajor,
				SchemaMinor: r.SchemaMinor, WatchConnected: r.WatchConnected, FirstSeenAt: r.FirstSeenAt,
				LastHeartbeatAt: r.LastHeartbeatAt, LastSyncedAt: r.LastSyncedAt, RejectedConfigVersion: r.RejectedConfigVersion,
				RejectedReason: r.RejectedReason, Live: r.Live,
			}}
			if r.LastSyncedAt != nil {
				d := r.Now.Sub(*r.LastSyncedAt)
				rs.Staleness = &d
			}
			out = append(out, rs)
		}
		if len(rows) < replicasPage {
			return out, nil
		}
		after = rows[len(rows)-1].ID
	}
}

// ConfigVersionApplied returns the replicas that reported config version v,
// each with the time of its first report, by name.
func (s *Store) ConfigVersionApplied(ctx context.Context, v int64) ([]Applied, error) {
	return db.New(s.pool).ConfigVersionApplied(ctx, v)
}

// PruneReplicas deletes the first reports older than 7 days, the replicas
// without a heartbeat for 7 days and the heavy-hitter windows older than
// 7 days, in batches of 10,000 rows (CP-22, ADR-0038, ADR-0041), and
// returns how many rows it deleted.
func (s *Store) PruneReplicas(ctx context.Context) (int64, error) {
	q := db.New(s.pool)
	var total int64
	for _, prune := range []func(context.Context) (int64, error){q.PruneReplicaVersions, q.PruneReplicas, q.PruneHeavyHitters} {
		for {
			n, err := prune(ctx)
			total += n
			if err != nil {
				return total, err
			}
			if n < pruneBatch {
				break
			}
		}
	}
	return total, nil
}

// pruneBatch is the prune queries' LIMIT.
const pruneBatch = 10000
