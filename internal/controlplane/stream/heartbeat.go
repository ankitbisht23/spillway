package stream

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Recorder records heartbeats and prunes what they leave: the store.
type Recorder interface {
	// RecordHeartbeat records replica's heartbeat and returns the live
	// replica count, replica included (CP-08, RL-24).
	RecordHeartbeat(ctx context.Context, replica string, hb *configpb.HeartbeatRequest) (int32, error)
	// PruneReplicas deletes what is older than 7 days (CP-22).
	PruneReplicas(ctx context.Context) (int64, error)
}

// heartbeatErrorEvery bounds how often failing heartbeats are logged.
const heartbeatErrorEvery = time.Minute

// Heartbeat records what a gateway runs and answers the live replica count
// (CP-08, RL-24, ADR-0038), under the replica name of the client
// certificate, as for Watch (ADR-0040). A heartbeat of any schema version
// is recorded: the replicas list shows a gateway that cannot watch.
func (s *Service) Heartbeat(ctx context.Context, req *configpb.HeartbeatRequest) (*configpb.HeartbeatResponse, error) {
	name, err := s.replica(ctx, "Heartbeat", req.GetReplicaId())
	if err != nil {
		return nil, err
	}
	live, err := s.rec.RecordHeartbeat(ctx, name, req)
	if err != nil {
		now := time.Now()
		if last := s.lastHeartbeatErr.Load(); now.Sub(time.Unix(0, last)) >= heartbeatErrorEvery && s.lastHeartbeatErr.CompareAndSwap(last, now.UnixNano()) {
			s.log.Error("recording a heartbeat failed", "replica", name, "err", err)
		}
		return nil, status.Error(codes.Unavailable, "the heartbeat could not be recorded")
	}
	return &configpb.HeartbeatResponse{LiveReplicas: max(live, 1)}, nil
}

// pruneInterval is how often the active replica prunes (CP-22, ADR-0038).
const pruneInterval = time.Hour

// prune runs rec's prune when the replica becomes active and every hour
// after, until ctx ends. A failed prune is logged; the next one comes at
// its time.
func prune(ctx context.Context, rec Recorder, log *slog.Logger) {
	t := time.NewTicker(pruneInterval)
	defer t.Stop()
	for {
		n, err := rec.PruneReplicas(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Error("pruning replicas failed; trying again in an hour", "err", err)
		case n > 0:
			log.Info("pruned replicas and version reports older than 7 days", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
