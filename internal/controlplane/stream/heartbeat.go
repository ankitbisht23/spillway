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
// (CP-08, RL-24, ADR-0038). Until M5's mtls-secrets task the replica name
// is the request's, as for Watch. A heartbeat of any schema version is
// recorded: the replicas list shows a gateway that cannot watch.
func (s *Service) Heartbeat(ctx context.Context, req *configpb.HeartbeatRequest) (*configpb.HeartbeatResponse, error) {
	name := req.GetReplicaId()
	if !replicaName.MatchString(name) {
		return nil, status.Error(codes.InvalidArgument, "replica_id: want a DNS label, such as gateway-1")
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
