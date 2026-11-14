package managed

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// fakeCP is the control plane as a gateway sees it: Watch streams fed by
// the test through channels, so the tests run in a synctest bubble on its
// fake clock.
type fakeCP struct {
	mu      sync.Mutex
	down    bool  // Watch fails at once, as a refused connection does
	refuse  error // Watch streams end with it before their headers, as a refused call's do
	reqs    []*configpb.WatchRequest
	at      []time.Time // when each Watch came
	streams chan *fakeStream
}

func newFakeCP(down bool) *fakeCP { return &fakeCP{down: down, streams: make(chan *fakeStream, 100)} }

func (f *fakeCP) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeCP) requests() ([]*configpb.WatchRequest, []time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*configpb.WatchRequest(nil), f.reqs...), append([]time.Time(nil), f.at...)
}

func (f *fakeCP) Watch(ctx context.Context, req *configpb.WatchRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[configpb.WatchResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	f.at = append(f.at, time.Now())
	if f.down {
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	if f.refuse != nil {
		return &fakeStream{ctx: ctx, refused: f.refuse}, nil
	}
	s := &fakeStream{ctx: ctx, msgs: make(chan *configpb.WatchResponse, 10), end: make(chan error, 1)}
	f.streams <- s
	return s, nil
}

func (f *fakeCP) Heartbeat(context.Context, *configpb.HeartbeatRequest, ...grpc.CallOption) (*configpb.HeartbeatResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// fakeStream is one Watch stream; the methods a client does not call come
// from the nil embedded interface.
type fakeStream struct {
	grpc.ClientStream
	ctx     context.Context
	msgs    chan *configpb.WatchResponse
	end     chan error
	refused error // the stream ended before its headers with this error
}

// Header answers as gRPC's does: headers, never nil, once the server has
// sent them; nil and no error for a stream that ended first, whose error
// Recv returns.
func (s *fakeStream) Header() (metadata.MD, error) {
	if s.refused != nil {
		return nil, nil
	}
	return metadata.MD{}, nil
}

func (s *fakeStream) Recv() (*configpb.WatchResponse, error) {
	if s.refused != nil {
		return nil, s.refused
	}
	select {
	case m := <-s.msgs:
		return m, nil
	case err := <-s.end:
		return nil, err
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func schema(major int32) *configpb.SchemaVersion {
	return &configpb.SchemaVersion{Major: major, Minor: config.SchemaMinor}
}

// cfg is a config of version v with one route.
func cfg(v int64) *configpb.Config {
	return &configpb.Config{Version: v,
		Upstreams: []*configpb.Upstream{{Name: "u", Endpoints: []*configpb.Endpoint{{Address: "127.0.0.1:9"}}}},
		Routes:    []*configpb.Route{{Name: "r", Upstream: "u", Match: &configpb.RouteMatch{PathPrefix: "/"}}},
	}
}

func configMsg(major int32, c *configpb.Config) *configpb.WatchResponse {
	return &configpb.WatchResponse{Update: &configpb.WatchResponse_Config{Config: &configpb.ConfigUpdate{SchemaVersion: schema(major), Config: c}}}
}

func identityMsg(major int32, v int64) *configpb.WatchResponse {
	return &configpb.WatchResponse{Update: &configpb.WatchResponse_Identity{Identity: &configpb.IdentityUpdate{
		SchemaVersion: schema(major), Identity: &configpb.Identity{Version: v, KeyFormat: config.KeyFormatManaged},
	}}}
}

// client runs a managed client against cp until the test ends, with its
// snapshot in dir.
func client(t *testing.T, cp *fakeCP, dir string) *Client {
	t.Helper()
	c := New(cp, Options{
		ReplicaID: "gateway-1", SnapshotPath: filepath.Join(dir, "snapshot.pb"),
		Compile: func(c *configpb.Config) (*config.Snapshot, error) { return config.Prepare(c, discard(), nil) },
		Log:     discard(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return c
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func versionsOf(c *Client) [2]int64 {
	cv, iv := c.versions()
	return [2]int64{cv, iv}
}

// TestApplyFromStream checks that updates from the stream are applied, the
// last good one kept when one is refused — another schema major (CP-18),
// a config that does not compile, an invalid identity — and that the
// newest good state reaches the disk (CP-07).
func TestApplyFromStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp, dir := newFakeCP(false), t.TempDir()
		c := client(t, cp, dir)
		s := <-cp.streams
		s.msgs <- configMsg(config.SchemaMajor, cfg(3))
		synctest.Wait()
		if versionsOf(c) != [2]int64{3, 0} {
			t.Fatalf("after config 3: %v", versionsOf(c))
		}
		if _, err := os.Stat(filepath.Join(dir, "snapshot.pb")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a snapshot was written before the identity came: %v", err)
		}
		s.msgs <- identityMsg(config.SchemaMajor, 2)
		synctest.Wait()
		if versionsOf(c) != [2]int64{3, 2} {
			t.Fatalf("after identity 2: %v", versionsOf(c))
		}
		bad := cfg(5)
		bad.Routes[0].Upstream = "missing"
		for _, m := range []*configpb.WatchResponse{
			configMsg(config.SchemaMajor+1, cfg(4)), // another major
			configMsg(config.SchemaMajor, bad),      // does not compile
			identityMsg(config.SchemaMajor+1, 3),
			{Update: &configpb.WatchResponse_Identity{Identity: &configpb.IdentityUpdate{SchemaVersion: schema(config.SchemaMajor),
				Identity: &configpb.Identity{Version: 4, KeyFormat: "spw9"}}}},
			{Update: &configpb.WatchResponse_Config{Config: &configpb.ConfigUpdate{SchemaVersion: schema(config.SchemaMajor)}}}, // no config
		} {
			s.msgs <- m
		}
		synctest.Wait()
		if versionsOf(c) != [2]int64{3, 2} {
			t.Errorf("after refused updates: %v, want the last good 3 and 2", versionsOf(c))
		}
		// A minor this gateway does not know is applied: it can ignore what
		// a minor adds.
		s.msgs <- &configpb.WatchResponse{Update: &configpb.WatchResponse_Config{Config: &configpb.ConfigUpdate{
			SchemaVersion: &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor + 1}, Config: cfg(6)}}}
		synctest.Wait()
		f, err := ReadSnapshot(filepath.Join(dir, "snapshot.pb"))
		if err != nil || f.GetConfig().GetVersion() != 6 || f.GetIdentity().GetVersion() != 2 || !f.GetSyncedAt().AsTime().Equal(time.Now()) {
			t.Errorf("on-disk snapshot %v, %v; want config 6, identity 2, synced now", f, err)
		}
	})
}

// TestReconnect checks the reconnects (ADR-0038): each Watch carries the
// versions in use; full-jitter backoff from 100 ms doubling to 5 s while
// the control plane is down, the last good config served meanwhile; the
// backoff starts over after a stream that stayed up 30 s.
func TestReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp := newFakeCP(false)
		c := client(t, cp, t.TempDir())
		s := <-cp.streams
		s.msgs <- configMsg(config.SchemaMajor, cfg(3))
		s.msgs <- identityMsg(config.SchemaMajor, 2)
		synctest.Wait()

		cp.setDown(true)
		s.end <- status.Error(codes.Unavailable, "stepping down")
		time.Sleep(time.Minute)
		reqs, at := cp.requests()
		if len(reqs) < 10 {
			t.Fatalf("%d Watch calls in a minute", len(reqs))
		}
		for i := 1; i < len(at); i++ {
			most := min(backoffMin<<(i-1), backoffMax)
			if gap := at[i].Sub(at[i-1]); gap <= 0 || gap > most {
				t.Errorf("gap %d: %v, want (0, %v]", i, gap, most)
			}
		}
		for _, r := range reqs[1:] {
			if r.GetConfigVersion() != 3 || r.GetIdentityVersion() != 2 || r.GetReplicaId() != "gateway-1" || r.GetSchemaVersion().GetMajor() != config.SchemaMajor {
				t.Fatalf("reconnect request %v", r)
			}
		}
		if versionsOf(c) != [2]int64{3, 2} {
			t.Errorf("while the control plane is down: %v", versionsOf(c))
		}

		// Up again: a stream that stays up 30 s resets the backoff.
		cp.setDown(false)
		s = <-cp.streams
		time.Sleep(backoffReset)
		ended := time.Now()
		s.end <- io.EOF
		<-cp.streams
		_, at = cp.requests()
		if gap := at[len(at)-1].Sub(ended); gap > backoffMin {
			t.Errorf("reconnect after a long stream took %v, want at most %v", gap, backoffMin)
		}
	})
}

// TestRefusedWatch checks that a Watch the server ends before its headers
// (here another schema major's refusal, CP-18) is a failed stream, retried
// with backoff, and never counted as established.
func TestRefusedWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		cp := newFakeCP(false)
		cp.refuse = status.Error(codes.FailedPrecondition, "schema 2.0 is not served")
		c := New(cp, Options{ReplicaID: "gateway-1", SnapshotPath: filepath.Join(t.TempDir(), "snapshot.pb"),
			Log: slog.New(slog.NewTextHandler(&logs, nil))})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx)
		}()
		time.Sleep(10 * time.Second)
		cancel()
		<-done
		if reqs, _ := cp.requests(); len(reqs) < 3 {
			t.Errorf("%d Watch calls in 10 s", len(reqs))
		}
		if out := logs.String(); strings.Contains(out, "established") || !strings.Contains(out, "schema 2.0 is not served") {
			t.Errorf("logs:\n%s", out)
		}
	})
}

// TestBootFileNeverReplacesStream checks that the on-disk snapshot applies
// only sections the stream has not delivered, checked under the apply
// lock: a stream update that lands as the 5 s wait ends is kept (D9).
func TestBootFileNeverReplacesStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cp := newFakeCP(false)
		c := client(t, cp, t.TempDir())
		s := <-cp.streams
		s.msgs <- configMsg(config.SchemaMajor, cfg(9))
		synctest.Wait()
		if c.applyConfig(schema(config.SchemaMajor), cfg(7), sourceFile) || !c.applyIdentity(schema(config.SchemaMajor),
			&configpb.Identity{Version: 4, KeyFormat: config.KeyFormatManaged}, sourceFile) {
			t.Error("the file's config was applied over the stream's, or its identity was not")
		}
		if c.applyIdentity(schema(config.SchemaMajor), &configpb.Identity{Version: 3, KeyFormat: config.KeyFormatManaged}, sourceFile) {
			t.Error("a second file identity was applied over the first")
		}
		if versionsOf(c) != [2]int64{9, 4} {
			t.Errorf("versions %v, want the stream's config 9 and the file's identity 4", versionsOf(c))
		}
	})
}

// writeFile writes a snapshot file of schema major, config cv and
// identity iv, synced at at.
func writeFile(t *testing.T, dir string, major int32, cv, iv int64, at time.Time) string {
	t.Helper()
	path := filepath.Join(dir, "snapshot.pb")
	if err := WriteSnapshot(path, &configpb.SnapshotFile{
		SchemaVersion: schema(major), Config: cfg(cv), Identity: &configpb.Identity{Version: iv, KeyFormat: config.KeyFormatManaged},
		SyncedAt: timestamppb.New(at),
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBoot checks a start without the control plane (D9, ADR-0038): the
// stream gets 5 s, then the on-disk snapshot is served, and not written
// back (its synced_at stays); the stream replaces it once the control
// plane answers.
func TestBoot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		synced := time.Now().Add(-time.Hour)
		path := writeFile(t, dir, config.SchemaMajor, 7, 4, synced)
		cp := newFakeCP(true)
		c := client(t, cp, dir)
		start := time.Now()
		c.Boot(context.Background())
		if d := time.Since(start); d != bootWait || versionsOf(c) != [2]int64{7, 4} {
			t.Fatalf("Boot returned after %v with %v; want 5s and the file's 7 and 4", d, versionsOf(c))
		}
		synctest.Wait()
		if f, err := ReadSnapshot(path); err != nil || !f.GetSyncedAt().AsTime().Equal(synced) {
			t.Errorf("the file was written back: %v, %v", f.GetSyncedAt().AsTime(), err)
		}

		cp.setDown(false)
		s := <-cp.streams
		reqs, _ := cp.requests()
		if r := reqs[len(reqs)-1]; r.GetConfigVersion() != 7 || r.GetIdentityVersion() != 4 {
			t.Errorf("the Watch after the boot asked from %d and %d", r.GetConfigVersion(), r.GetIdentityVersion())
		}
		s.msgs <- configMsg(config.SchemaMajor, cfg(8))
		synctest.Wait()
		if f, _ := ReadSnapshot(path); versionsOf(c) != [2]int64{8, 4} || f.GetConfig().GetVersion() != 8 || !f.GetSyncedAt().AsTime().Equal(time.Now()) {
			t.Errorf("after the stream's config 8: %v, file %v", versionsOf(c), f)
		}
	})
}

// TestBootWithoutSnapshot checks the other starts (ADR-0038): the stream
// within 5 s needs no file; no file, a corrupt one or one of another
// schema major leaves the gateway unready until the stream delivers.
func TestBootWithoutSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, config.SchemaMajor, 7, 4, time.Now())
		cp := newFakeCP(false)
		c := client(t, cp, dir)
		go func() {
			s := <-cp.streams
			time.Sleep(time.Second)
			s.msgs <- configMsg(config.SchemaMajor, cfg(9))
			s.msgs <- identityMsg(config.SchemaMajor, 5)
		}()
		start := time.Now()
		c.Boot(context.Background())
		if d := time.Since(start); d != time.Second || versionsOf(c) != [2]int64{9, 5} {
			t.Errorf("Boot with a live stream: %v, %v", d, versionsOf(c))
		}
	})
	for name, prepare := range map[string]func(t *testing.T, dir string){
		"no file": func(*testing.T, string) {},
		"corrupt file": func(t *testing.T, dir string) {
			_ = os.WriteFile(filepath.Join(dir, "snapshot.pb"), []byte("not a snapshot"), 0o600)
		},
		"another major": func(t *testing.T, dir string) { writeFile(t, dir, config.SchemaMajor+1, 7, 4, time.Now()) },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				prepare(t, dir)
				cp := newFakeCP(true)
				c := client(t, cp, dir)
				c.Boot(context.Background())
				if c.Snapshot() != nil || c.Identity() != nil {
					t.Fatalf("serving %v without a usable file", versionsOf(c))
				}
				cp.setDown(false)
				s := <-cp.streams
				s.msgs <- configMsg(config.SchemaMajor, cfg(9))
				s.msgs <- identityMsg(config.SchemaMajor, 5)
				synctest.Wait()
				if versionsOf(c) != [2]int64{9, 5} {
					t.Errorf("after the stream delivered: %v", versionsOf(c))
				}
			})
		})
	}
}

// TestWriteSnapshot checks the file write (D9): it reads back, replaces the
// old file whole, and leaves no temporary file.
func TestWriteSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, config.SchemaMajor, 1, 1, time.Unix(1, 0))
	writeFile(t, dir, config.SchemaMajor, 2, 3, time.Unix(2, 0))
	f, err := ReadSnapshot(path)
	if err != nil || f.GetConfig().GetVersion() != 2 || f.GetIdentity().GetVersion() != 3 || !proto.Equal(f.GetConfig(), cfg(2)) {
		t.Fatalf("read back %v, %v", f, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory holds %d files, want only the snapshot", len(entries))
	}
	if err := WriteSnapshot(filepath.Join(dir, "missing", "snapshot.pb"), f); err == nil {
		t.Error("a write into a missing directory succeeded")
	}
	if _, err := ReadSnapshot(filepath.Join(dir, "none.pb")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
}
