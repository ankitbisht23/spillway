// Package managed is a gateway's managed mode (CP-06 - CP-08, RL-24,
// ADR-0038): it watches the control plane's ConfigService for config and
// identity, applies each as a file reload would, heartbeats what it runs,
// divides limits by the live replica count, keeps the last good snapshot
// on disk, and boots from it when the control plane is unreachable.
package managed

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	grpcbackoff "google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Options configure a managed gateway.
type Options struct {
	// ReplicaID is the gateway's replica name, a DNS label.
	ReplicaID string
	// SnapshotPath is the on-disk snapshot file.
	SnapshotPath string
	// Compile compiles a config and reconciles the gateway's runtime state
	// with it, as a file reload does (config.Prepare); an error rejects
	// it. Its ratelimit.replicas is the live replica count once one is
	// known (RL-24).
	Compile func(*configpb.Config) (*config.Snapshot, error)
	Log     *slog.Logger
}

// Client is a managed gateway's view of the control plane: the config and
// identity it serves, kept current from the Watch stream.
type Client struct {
	opts   Options
	cs     configpb.ConfigServiceClient
	writer *writer
	beat   chan struct{} // one slot: a heartbeat is due now

	config   atomic.Pointer[config.Snapshot]
	identity atomic.Pointer[config.Identity]
	// watching is true while the Watch stream is established.
	watching atomic.Bool
	// live is the last known live replica count: the latest heartbeat
	// answer's, else the on-disk snapshot's when the gateway booted from
	// it; 0 when neither is known, and the config's ratelimit.replicas
	// applies (RL-24, RL-31).
	live atomic.Int32

	mu          sync.Mutex // serializes applies; guards what follows
	rawConfig   *configpb.Config
	rawIdentity *configpb.Identity
	both        chan struct{} // closed once a config and an identity are applied
	appliedAt   time.Time     // the last apply, gateway clock
	rejected    int64         // the last config version refused since the last apply; 0 for none
	reason      string        // why, or why the on-disk snapshot could not be used

	syncMu   sync.Mutex
	lastSync time.Time // gw_config_sync_age_seconds counts from it
	synced   bool      // a heartbeat has synced since the start
}

// New returns a client of the ConfigService that cs calls.
func New(cs configpb.ConfigServiceClient, opts Options) *Client {
	c := &Client{opts: opts, cs: cs, beat: make(chan struct{}, 1), both: make(chan struct{}), lastSync: time.Now()}
	c.writer = newWriter(opts.SnapshotPath, c.snapshotFile, opts.Log)
	return c
}

// DialOptions are the gRPC client options of the ConfigService connection
// (ADR-0038): keepalive pings after 10 s idle with a 5 s timeout, messages
// up to 64 MiB, then extra (M5's mtls-secrets task adds transport
// credentials there). The connection's own redials back off from 100 ms
// doubling to 5 s, as the Watch stream's reconnects do; gRPC's default
// grows to 120 s, which would keep a gateway away from a control plane
// that is back for up to 2 minutes after a long outage.
func DialOptions(extra ...grpc.DialOption) []grpc.DialOption {
	return append([]grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 10 * time.Second, Timeout: 5 * time.Second, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           grpcbackoff.Config{BaseDelay: backoffMin, Multiplier: 2, Jitter: 0.2, MaxDelay: backoffMax},
			MinConnectTimeout: 20 * time.Second, // gRPC's default
		}),
	}, extra...)
}

// Snapshot returns the config in use, nil before the first.
func (c *Client) Snapshot() *config.Snapshot { return c.config.Load() }

// Identity returns the identity in use, nil before the first.
func (c *Client) Identity() *config.Identity { return c.identity.Load() }

// versions are the config and identity versions in use, 0 for none.
func (c *Client) versions() (configVersion, identityVersion int64) {
	if s := c.Snapshot(); s != nil {
		configVersion = s.Version
	}
	if id := c.Identity(); id != nil {
		identityVersion = id.Version
	}
	return configVersion, identityVersion
}

// The Watch stream's reconnects (ADR-0038): full jitter from 100 ms
// doubling to 5 s, reset once a stream has stayed up 30 s.
const (
	backoffMin   = 100 * time.Millisecond
	backoffMax   = 5 * time.Second
	backoffReset = 30 * time.Second
	// streamErrorEvery bounds how often a failing stream is logged.
	streamErrorEvery = time.Minute
)

// Run keeps the Watch stream open, heartbeats, and writes the on-disk
// snapshot after each apply, until ctx ends.
func (c *Client) Run(ctx context.Context) {
	go c.writer.run(ctx)
	beats := make(chan struct{})
	go func() {
		defer close(beats)
		c.heartbeats(ctx)
	}()
	defer func() {
		<-c.writer.done
		<-beats
	}()
	backoff := backoffMin
	var lastErr time.Time
	for {
		start := time.Now()
		err := c.watch(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= backoffReset {
			backoff = backoffMin
		}
		if time.Since(lastErr) >= streamErrorEvery {
			cv, iv := c.versions()
			c.opts.Log.Warn("Watch stream to the control plane failed; reconnecting, serving the last good config meanwhile",
				"err", err, "config_version", cv, "identity_version", iv)
			lastErr = time.Now()
		}
		t := time.NewTimer(rand.N(backoff) + 1) //nolint:gosec // G404: jitter, not security
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, backoffMax)
	}
}

// watch opens one Watch stream with the versions in use and applies what it
// sends until it fails.
func (c *Client) watch(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cv, iv := c.versions()
	s, err := c.cs.Watch(ctx, &configpb.WatchRequest{
		ReplicaId:       c.opts.ReplicaID,
		ConfigVersion:   cv,
		IdentityVersion: iv,
		SchemaVersion:   &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor},
	})
	if err != nil {
		return err
	}
	// The server sends its headers once it has registered the stream. A
	// stream that ends first (refused, or a standby's closed port) has no
	// headers, and gRPC leaves its error to Recv.
	md, err := s.Header()
	if err != nil {
		return err
	}
	if md == nil {
		_, err := s.Recv()
		return err
	}
	c.watching.Store(true)
	defer c.watching.Store(false)
	c.opts.Log.Info("Watch stream to the control plane established", "config_version", cv, "identity_version", iv)
	for {
		m, err := s.Recv()
		if err != nil {
			return err
		}
		switch u := m.GetUpdate().(type) {
		case *configpb.WatchResponse_Config:
			c.applyConfig(u.Config.GetSchemaVersion(), u.Config.GetConfig(), sourceStream)
		case *configpb.WatchResponse_Identity:
			c.applyIdentity(u.Identity.GetSchemaVersion(), u.Identity.GetIdentity(), sourceStream)
		}
	}
}

// applyConfig compiles and applies a config of schema v from source, or
// rejects it and keeps the last good one (CP-18, GW-21, GW-29).
func (c *Client) applyConfig(v *configpb.SchemaVersion, cfg *configpb.Config, source string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if source == sourceFile && c.rawConfig != nil {
		return false // the stream delivered first; the file must not replace it
	}
	var err error
	var snap *config.Snapshot
	if err = checkSchema(v); err == nil {
		if cfg == nil {
			err = errors.New("no config in the update")
		} else {
			snap, err = c.compile(cfg, c.live.Load())
		}
	}
	if err != nil {
		cv, _ := c.versions()
		c.opts.Log.Error("config rejected; keeping the last good config", "source", source, "version", cfg.GetVersion(),
			"current_version", cv, "err", err)
		// Reported in heartbeats until a config applies (ADR-0038).
		c.rejected, c.reason = cfg.GetVersion(), err.Error()
		c.heartbeatNow()
		return false
	}
	c.config.Store(snap)
	c.rawConfig = cfg
	c.rejected, c.reason = 0, ""
	c.applied(source)
	c.opts.Log.Info("config applied", "source", source, "version", snap.Version, "routes", len(snap.Routes), "policies", len(snap.Policies))
	return true
}

// applyIdentity compiles and applies an identity of schema v from source,
// or rejects it and keeps the last good one (CP-20, ADR-0037).
func (c *Client) applyIdentity(v *configpb.SchemaVersion, id *configpb.Identity, source string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if source == sourceFile && c.rawIdentity != nil {
		return false // the stream delivered first; the file must not replace it
	}
	var err error
	var compiled *config.Identity
	if err = checkSchema(v); err == nil {
		if id == nil {
			err = errors.New("no identity in the update")
		} else {
			compiled, err = config.CompileIdentity(id)
		}
	}
	if err != nil {
		_, iv := c.versions()
		c.opts.Log.Error("identity rejected; keeping the last good identity", "source", source, "version", id.GetVersion(),
			"current_version", iv, "err", err)
		return false
	}
	c.identity.Store(compiled)
	c.rawIdentity = id
	c.applied(source)
	c.opts.Log.Info("identity applied", "source", source, "version", compiled.Version)
	return true
}

// applied notes an apply from source; c.mu is held. A heartbeat goes at
// once (CP-08). What came from the stream is written to disk; what came
// from the file is not written back, so the file's synced_at stays the time
// of its last sync.
func (c *Client) applied(source string) {
	c.appliedAt = time.Now()
	c.heartbeatNow()
	if c.rawConfig != nil && c.rawIdentity != nil {
		select {
		case <-c.both:
		default:
			close(c.both)
		}
		if source == sourceStream {
			c.writer.due()
		}
	}
}

// Where an applied version came from.
const (
	sourceStream = "stream"
	sourceFile   = "snapshot file"
)

// checkSchema refuses another major (CP-18): its fields may mean what this
// gateway does not know. Any minor of this major is applied; fields this
// gateway lacks are ignored, which a minor bump makes safe.
func checkSchema(v *configpb.SchemaVersion) error {
	if v.GetMajor() != config.SchemaMajor {
		return fmt.Errorf("schema %d.%d; this gateway applies schema major %d only", v.GetMajor(), v.GetMinor(), config.SchemaMajor)
	}
	return nil
}

// snapshotFile is what the on-disk snapshot holds now, nil until both
// sections are applied.
func (c *Client) snapshotFile() *configpb.SnapshotFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rawConfig == nil || c.rawIdentity == nil {
		return nil
	}
	return &configpb.SnapshotFile{
		SchemaVersion: &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor},
		Config:        c.rawConfig,
		Identity:      c.rawIdentity,
		SyncedAt:      timestamppb.Now(),
		LiveReplicas:  c.live.Load(),
	}
}

// bootWait is how long a starting gateway waits for the stream before it
// serves its on-disk snapshot (D9, ADR-0038).
const bootWait = 5 * time.Second

// Boot waits up to 5 s for a config and an identity from the stream (Run
// must be running). If they do not both come, it applies the on-disk
// snapshot for what is missing, if the file exists, parses and has this
// gateway's schema major; the stream replaces it as soon as it delivers.
// Without either, the gateway stays unready until the stream delivers.
func (c *Client) Boot(ctx context.Context) {
	t := time.NewTimer(bootWait)
	defer t.Stop()
	select {
	case <-c.both:
		return
	case <-ctx.Done():
		return
	case <-t.C:
	}
	f, err := ReadSnapshot(c.opts.SnapshotPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.opts.Log.Warn("no control plane within 5s and no on-disk snapshot; not ready until the control plane answers", "path", c.opts.SnapshotPath)
		return
	case err != nil:
		c.opts.Log.Error("no control plane within 5s and the on-disk snapshot cannot be used; not ready until the control plane answers", "err", err)
		c.mu.Lock()
		if c.rawConfig == nil && c.rejected == 0 { // reported until a config applies (ADR-0038)
			c.reason = "on-disk snapshot: " + err.Error()
		}
		c.mu.Unlock()
		return
	}
	// Until a heartbeat answers, the file's live replica count is the last
	// known one (RL-24, ADR-0038).
	if n := f.GetLiveReplicas(); n > 0 {
		c.live.CompareAndSwap(0, n)
	}
	// Only what the stream has not delivered yet: the applies skip a
	// section it has, checked under their lock, so a stream update that
	// lands meanwhile is never replaced by the older file.
	cfgOK := c.applyConfig(f.GetSchemaVersion(), f.GetConfig(), sourceFile)
	idOK := c.applyIdentity(f.GetSchemaVersion(), f.GetIdentity(), sourceFile)
	if cfgOK || idOK {
		c.bootSynced(f.GetSyncedAt().AsTime())
		c.opts.Log.Warn("no control plane within 5s: serving the on-disk snapshot until it answers",
			"config_version", f.GetConfig().GetVersion(), "config_from_file", cfgOK,
			"identity_version", f.GetIdentity().GetVersion(), "identity_from_file", idOK, "synced_at", f.GetSyncedAt().AsTime())
	}
}
