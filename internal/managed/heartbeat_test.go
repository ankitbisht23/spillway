package managed

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// lockedBuffer is a log buffer the test reads while the client writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestHeartbeats checks the heartbeat's schedule and content (CP-08,
// ADR-0038): one at once after an apply, then every 5 s, each with a 4 s
// deadline, the versions in use, the schema, whether the Watch stream is
// established and the time of the last apply.
func TestHeartbeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp := newFakeCP(false)
		start := time.Now()
		client(t, cp, t.TempDir())
		s := <-cp.streams
		time.Sleep(time.Second)
		s.msgs <- configMsg(config.SchemaMajor, cfg(3))
		s.msgs <- identityMsg(config.SchemaMajor, 2)
		synctest.Wait()
		hbs := cp.heartbeats()
		if len(hbs) == 0 {
			t.Fatal("no heartbeat after the applies")
		}
		last := hbs[len(hbs)-1]
		if r := last.req; last.at != start.Add(time.Second) || r.GetConfigVersion() != 3 || r.GetIdentityVersion() != 2 || !r.GetWatchConnected() ||
			r.GetReplicaId() != "gateway-1" || r.GetSchemaVersion().GetMajor() != config.SchemaMajor || !r.GetAppliedAt().AsTime().Equal(last.at) ||
			last.deadline != last.at.Add(heartbeatTimeout) || r.GetRejectedConfigVersion() != 0 || r.GetRejectedReason() != "" {
			t.Fatalf("the heartbeat after the applies, at %v: %v, deadline %v", last.at.Sub(start), r, last.deadline.Sub(last.at))
		}
		n := len(hbs)
		time.Sleep(21 * time.Second)
		hbs = cp.heartbeats()[n:]
		if len(hbs) != 4 {
			t.Fatalf("%d heartbeats in the next 21 s, want 4", len(hbs))
		}
		for i, hb := range hbs {
			if want := start.Add(time.Duration(i+1) * heartbeatEvery); hb.at != want {
				t.Errorf("heartbeat %d at %v, want %v", i, hb.at.Sub(start), want.Sub(start))
			}
		}
	})
}

// TestHeartbeatReportsRejected checks that a refused config is reported
// with its version and error, at once, until a config applies (ADR-0038).
func TestHeartbeatReportsRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp := newFakeCP(false)
		client(t, cp, t.TempDir())
		s := <-cp.streams
		s.msgs <- configMsg(config.SchemaMajor, cfg(3))
		s.msgs <- identityMsg(config.SchemaMajor, 2)
		synctest.Wait()
		bad := cfg(4)
		bad.Routes[0].Upstream = "missing"
		s.msgs <- configMsg(config.SchemaMajor, bad)
		synctest.Wait()
		hbs := cp.heartbeats()
		if r := hbs[len(hbs)-1].req; r.GetConfigVersion() != 3 || r.GetRejectedConfigVersion() != 4 || !strings.Contains(r.GetRejectedReason(), `upstream "missing" is not defined`) {
			t.Fatalf("after a refused version 4: %v", r)
		}
		time.Sleep(heartbeatEvery)
		synctest.Wait()
		if r := lastHeartbeat(cp).req; r.GetRejectedConfigVersion() != 4 {
			t.Errorf("the next heartbeat forgot the refusal: %v", r)
		}
		s.msgs <- configMsg(config.SchemaMajor, cfg(5))
		synctest.Wait()
		if r := lastHeartbeat(cp).req; r.GetConfigVersion() != 5 || r.GetRejectedConfigVersion() != 0 || r.GetRejectedReason() != "" {
			t.Errorf("after version 5 applied: %v", r)
		}
	})
}

// limited is cfg(v) with ratelimit.replicas and a local GCRA policy of
// rate and burst per second on every route, which costs cost.
func limited(v int64, replicas int32, rate, cost int64) *configpb.Config {
	c := cfg(v)
	c.Ratelimit = &configpb.RateLimitSettings{Replicas: replicas}
	c.Routes[0].Cost = cost
	c.Policies = []*configpb.Policy{{Name: "p", Level: "global", Algorithm: config.GCRA, Local: true, KeyTemplate: "{ip}",
		Match: &configpb.PolicyMatch{Routes: []string{"*"}}, Limits: []*configpb.Limit{{Rate: rate, Period: "1s"}}}}
	return c
}

// perReplica is the rate of the config in use and the replica count it
// was compiled with.
func perReplica(c *Client) [2]int64 {
	s := c.Snapshot()
	return [2]int64{s.Policies[0].Limits[0].Rate, s.Replicas}
}

// TestLiveReplicas checks the replica count of a managed gateway (RL-24,
// RL-31, ADR-0038): the config's ratelimit.replicas until a heartbeat
// answers; then live_replicas, which recompiles the per-replica limits
// without a new version; a failed heartbeat keeps the last known count;
// the count is written with the on-disk snapshot, and a gateway that
// starts with that file uses it before its first answer.
func TestLiveReplicas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp, dir := newFakeCP(false), t.TempDir()
		cp.answer(0, status.Error(codes.Unavailable, "no database"))
		c := client(t, cp, dir)
		s := <-cp.streams
		s.msgs <- configMsg(config.SchemaMajor, limited(3, 2, 100, 1))
		s.msgs <- identityMsg(config.SchemaMajor, 2)
		synctest.Wait()
		if perReplica(c) != [2]int64{50, 2} {
			t.Fatalf("before an answer: rate %v, want ratelimit.replicas 2's 50", perReplica(c))
		}
		cp.answer(4, nil)
		time.Sleep(heartbeatEvery)
		synctest.Wait()
		if perReplica(c) != [2]int64{25, 4} || c.Snapshot().Version != 3 {
			t.Fatalf("after live_replicas 4: rate %v at version %d, want 25 at 3", perReplica(c), c.Snapshot().Version)
		}
		if f, err := ReadSnapshot(filepath.Join(dir, "snapshot.pb")); err != nil || f.GetLiveReplicas() != 4 || f.GetConfig().GetRatelimit().GetReplicas() != 2 {
			t.Errorf("on-disk snapshot: live %d, config's replicas %d, %v; want 4 and the published 2",
				f.GetLiveReplicas(), f.GetConfig().GetRatelimit().GetReplicas(), err)
		}
		cp.answer(0, status.Error(codes.Unavailable, "no database"))
		time.Sleep(3 * heartbeatEvery)
		if perReplica(c) != [2]int64{25, 4} {
			t.Errorf("after failed heartbeats: %v, want the last known 4", perReplica(c))
		}
		// A new version compiles with the live count.
		s.msgs <- configMsg(config.SchemaMajor, limited(4, 2, 200, 1))
		synctest.Wait()
		if perReplica(c) != [2]int64{50, 4} {
			t.Errorf("version 4: %v, want 200 / 4", perReplica(c))
		}
		// 0 from the control plane counts as 1: the gateway itself.
		cp.answer(0, nil)
		time.Sleep(heartbeatEvery)
		synctest.Wait()
		if perReplica(c) != [2]int64{200, 1} {
			t.Errorf("after live_replicas 0: %v, want 1", perReplica(c))
		}
	})

	synctest.Test(t, func(t *testing.T) {
		// A start from a file written with 3 live, the control plane down.
		dir := t.TempDir()
		path := writeFile(t, dir, config.SchemaMajor, 7, 4, time.Now())
		f, _ := ReadSnapshot(path)
		f.Config = limited(7, 0, 90, 1)
		f.LiveReplicas = 3
		if err := WriteSnapshot(path, f); err != nil {
			t.Fatal(err)
		}
		c := client(t, newFakeCP(true), dir)
		c.Boot(context.Background())
		if perReplica(c) != [2]int64{30, 3} {
			t.Errorf("booted from a file with 3 live: %v, want 90 / 3", perReplica(c))
		}
	})
}

// TestLiveReplicasConfigDoesNotCompile checks a count with which the config
// does not compile (a route's cost above a per-replica capacity): the
// config in use keeps its limits, and a new version that does not compile
// with it is refused and reported.
func TestLiveReplicasConfigDoesNotCompile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &lockedBuffer{}
		cp := newFakeCP(false)
		c := New(cp, Options{
			ReplicaID: "gateway-1", SnapshotPath: filepath.Join(t.TempDir(), "snapshot.pb"),
			Compile: func(c *configpb.Config) (*config.Snapshot, error) { return config.Prepare(c, discard(), nil) },
			Log:     slog.New(slog.NewTextHandler(logs, nil)),
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go c.Run(ctx)
		s := <-cp.streams
		s.msgs <- configMsg(config.SchemaMajor, limited(3, 1, 4, 2)) // burst 4, cost 2
		s.msgs <- identityMsg(config.SchemaMajor, 2)
		synctest.Wait()
		cp.answer(3, nil) // 4 / 3 = 1 per replica: below the cost
		time.Sleep(heartbeatEvery)
		synctest.Wait()
		if perReplica(c) != [2]int64{4, 1} || !strings.Contains(logs.String(), "does not compile with the live replica count") {
			t.Errorf("with 3 live: %v; logs:\n%s", perReplica(c), logs.String())
		}
		s.msgs <- configMsg(config.SchemaMajor, limited(4, 1, 4, 2))
		synctest.Wait()
		hbs := cp.heartbeats()
		if r := hbs[len(hbs)-1].req; c.Snapshot().Version != 3 || r.GetRejectedConfigVersion() != 4 {
			t.Errorf("version 4 with 3 live: running %d, heartbeat %v", c.Snapshot().Version, r)
		}
		cancel()
		synctest.Wait()
	})
}

func lastHeartbeat(cp *fakeCP) sentHeartbeat {
	hbs := cp.heartbeats()
	return hbs[len(hbs)-1]
}

// TestSyncAge checks gw_config_sync_age_seconds (CP-08, ADR-0038): from
// the start before any sync; 0 after a heartbeat answered with the stream
// established; growing while heartbeats fail, and while they succeed with
// the stream down; after a boot from the on-disk snapshot, from its
// synced_at.
func TestSyncAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp := newFakeCP(true)
		c := client(t, cp, t.TempDir())
		time.Sleep(10 * time.Second)
		if age := c.SyncAge(); age != 10*time.Second {
			t.Fatalf("before any sync: %v, want 10s from the start", age)
		}
		cp.setDown(false)
		s := <-cp.streams
		s.msgs <- configMsg(config.SchemaMajor, cfg(3))
		s.msgs <- identityMsg(config.SchemaMajor, 2)
		synctest.Wait()
		if age := c.SyncAge(); age != 0 {
			t.Fatalf("after the heartbeat on apply: %v", age)
		}
		// The periodic heartbeats sync too; between them it grows.
		applied := time.Now()
		time.Sleep(heartbeatEvery)
		synctest.Wait()
		last := lastHeartbeat(cp)
		if age := c.SyncAge(); !last.at.After(applied) || age != time.Since(last.at) || age >= heartbeatEvery {
			t.Errorf("5 s after the apply: %v, last heartbeat %v after it", age, last.at.Sub(applied))
		}

		// Heartbeats fail: it grows from the last answered one.
		cp.answer(1, status.Error(codes.Unavailable, "no database"))
		last = lastHeartbeat(cp)
		time.Sleep(20 * time.Second)
		if age := c.SyncAge(); age != time.Since(last.at) || age < 20*time.Second {
			t.Errorf("heartbeats failing for 20 s: %v", age)
		}
		// Heartbeats answer but the stream is down: it still grows.
		cp.answer(2, nil)
		time.Sleep(heartbeatEvery)
		synctest.Wait()
		last = lastHeartbeat(cp)
		if age := c.SyncAge(); age != time.Since(last.at) || !last.req.GetWatchConnected() {
			t.Errorf("heartbeats answered again: %v", age)
		}
		cp.mu.Lock()
		cp.refuse = status.Error(codes.Unavailable, "not the active replica")
		cp.mu.Unlock()
		s.end <- status.Error(codes.Unavailable, "stepping down")
		time.Sleep(20 * time.Second)
		if age, hb := c.SyncAge(), lastHeartbeat(cp); age != time.Since(last.at) || age < 20*time.Second || hb.req.GetWatchConnected() {
			t.Errorf("stream down, heartbeats answered: age %v, last heartbeat %v", age, hb.req)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, config.SchemaMajor, 7, 4, time.Now().Add(-time.Hour))
		cp := newFakeCP(true)
		c := client(t, cp, dir)
		c.Boot(context.Background())
		if age := c.SyncAge(); age != time.Hour+bootWait {
			t.Errorf("booted from a file synced an hour ago: %v", age)
		}
	})
}

// TestHeartbeatErrorsLoggedOncePerMinute checks that failing heartbeats
// log at most once a minute, and a file that cannot be used is reported.
func TestHeartbeatErrorsLoggedOncePerMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &lockedBuffer{}
		dir := t.TempDir()
		writeFile(t, dir, config.SchemaMajor+1, 7, 4, time.Now())
		cp := newFakeCP(true)
		c := New(cp, Options{ReplicaID: "gateway-1", SnapshotPath: filepath.Join(dir, "snapshot.pb"), Log: slog.New(slog.NewTextHandler(logs, nil))})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx)
		}()
		c.Boot(ctx)
		time.Sleep(66*time.Second - bootWait) // heartbeats at 5 s … 65 s
		synctest.Wait()
		if n := strings.Count(logs.String(), "heartbeat to the control plane failed"); n != 2 {
			t.Errorf("logged %d times in 66 s, want 2 (at 5 s and 65 s):\n%s", n, logs.String())
		}
		hbs := cp.heartbeats()
		if r := hbs[len(hbs)-1].req; r.GetRejectedConfigVersion() != 0 || !strings.Contains(r.GetRejectedReason(), "on-disk snapshot") ||
			!strings.Contains(r.GetRejectedReason(), "schema major 1 only") {
			t.Errorf("a file of another major: heartbeat %v", r)
		}
		if len(hbs) != 13 {
			t.Errorf("%d heartbeats in 66 s, want 13", len(hbs))
		}
		cancel()
		<-done
	})
}
