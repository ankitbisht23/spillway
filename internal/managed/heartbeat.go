package managed

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Heartbeats, the live replica count and the sync age (CP-08, RL-24,
// ADR-0038).

// The heartbeat's timing (ADR-0038).
const (
	heartbeatEvery   = 5 * time.Second
	heartbeatTimeout = 4 * time.Second
	// heartbeatErrorEvery bounds how often failing heartbeats are logged.
	heartbeatErrorEvery = time.Minute
)

// heartbeatNow asks for a heartbeat at once, as after an apply.
func (c *Client) heartbeatNow() {
	select {
	case c.beat <- struct{}{}:
	default:
	}
}

// heartbeats sends a heartbeat every 5 s, and at once when one is asked
// for, until ctx ends. One is in flight at a time; a failed one is logged
// at most once a minute, and the next goes at its time.
func (c *Client) heartbeats(ctx context.Context) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	var lastErr time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.beat:
		}
		if err := c.heartbeat(ctx); err != nil && ctx.Err() == nil && time.Since(lastErr) >= heartbeatErrorEvery {
			cv, iv := c.versions()
			c.opts.Log.Warn("heartbeat to the control plane failed; serving the last good config, the next one in 5s",
				"err", err, "config_version", cv, "identity_version", iv)
			lastErr = time.Now()
		}
	}
}

// heartbeat sends one heartbeat with what the gateway runs. Its answer
// is a sync when the Watch stream was established as it was sent; its
// live replica count replaces the last known one.
func (c *Client) heartbeat(ctx context.Context) error {
	watching := c.watching.Load()
	cv, iv := c.versions()
	req := &configpb.HeartbeatRequest{
		ReplicaId:       c.opts.ReplicaID,
		ConfigVersion:   cv,
		IdentityVersion: iv,
		SchemaVersion:   &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor},
		WatchConnected:  watching,
	}
	c.mu.Lock()
	req.RejectedConfigVersion, req.RejectedReason = c.rejected, c.reason
	if !c.appliedAt.IsZero() {
		req.AppliedAt = timestamppb.New(c.appliedAt)
	}
	c.mu.Unlock()
	hctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	resp, err := c.cs.Heartbeat(hctx, req)
	cancel()
	if err != nil {
		return err
	}
	if watching {
		c.syncMu.Lock()
		c.lastSync, c.synced = time.Now(), true
		c.syncMu.Unlock()
	}
	c.setLive(max(resp.GetLiveReplicas(), 1), watching)
	return nil
}

// setLive makes n the live replica count (RL-24). When the config in use
// was compiled with another count, it is compiled again with n, which
// reconciles the limiters as a change of ratelimit.replicas does, their
// state kept (GW-29); if that fails, the config in use stays. After a
// sync, a changed count is written to the on-disk snapshot.
func (c *Client) setLive(n int32, synced bool) {
	if c.live.Swap(n) == n {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if synced {
		c.writer.due()
	}
	cur := c.config.Load()
	if c.rawConfig == nil || cur.Replicas == int64(n) {
		return
	}
	snap, err := c.compile(c.rawConfig, n)
	if err != nil {
		c.opts.Log.Error("the config does not compile with the live replica count; keeping its limits for the previous count",
			"live_replicas", n, "replicas_in_use", cur.Replicas, "config_version", cur.Version, "err", err)
		return
	}
	c.config.Store(snap)
	c.opts.Log.Info("live replica count changed; per-replica limits follow it", "live_replicas", n, "config_version", snap.Version)
}

// compile compiles cfg for live replicas: its ratelimit.replicas becomes
// live, unless live is 0 (none known yet), when the config's own applies
// (RL-24, RL-31). cfg itself is not changed.
func (c *Client) compile(cfg *configpb.Config, live int32) (*config.Snapshot, error) {
	if live > 0 {
		cfg = proto.CloneOf(cfg)
		if cfg.Ratelimit == nil {
			cfg.Ratelimit = &configpb.RateLimitSettings{}
		}
		cfg.Ratelimit.Replicas = live
	}
	return c.opts.Compile(cfg)
}

// bootSynced starts the sync age from the on-disk snapshot's synced_at,
// unless a heartbeat has synced already (ADR-0038).
func (c *Client) bootSynced(at time.Time) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	if !c.synced {
		c.lastSync = at
	}
}

// SyncAge is gw_config_sync_age_seconds (CP-08, ADR-0038): the time since
// the last heartbeat answered while the Watch stream was established;
// before one, since the on-disk snapshot's synced_at when the gateway
// booted from it, else since the start.
func (c *Client) SyncAge() time.Duration {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	return time.Since(c.lastSync)
}
