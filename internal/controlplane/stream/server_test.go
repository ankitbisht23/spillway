package stream

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// watchServer serves ConfigService over an in-memory connection with the
// real options, codec included, and returns a client and the broadcaster.
func watchServer(t *testing.T, src *fakeSource) (configpb.ConfigServiceClient, *Broadcaster) {
	t.Helper()
	return configServer(t, src, &fakeRecorder{})
}

// configServer is watchServer with heartbeats recorded in rec.
func configServer(t *testing.T, src *fakeSource, rec Recorder) (configpb.ConfigServiceClient, *Broadcaster) {
	t.Helper()
	b := started(t, src)
	ln := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(serverOptions()...)
	configpb.RegisterConfigServiceServer(gs, NewService(b, rec, discard()))
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return configpb.NewConfigServiceClient(conn), b
}

func watchReq(name string, config, identity int64) *configpb.WatchRequest {
	return &configpb.WatchRequest{ReplicaId: name, ConfigVersion: config, IdentityVersion: identity, SchemaVersion: currentSchema()}
}

// recv returns the stream's next message as "config N" or "identity N",
// or its error.
func recv(t *testing.T, s grpc.ServerStreamingClient[configpb.WatchResponse]) (string, error) {
	t.Helper()
	m, err := s.Recv()
	if err != nil {
		return "", err
	}
	switch u := m.GetUpdate().(type) {
	case *configpb.WatchResponse_Config:
		if u.Config.GetSchemaVersion().GetMajor() != config.SchemaMajor || u.Config.GetConfig() == nil {
			t.Errorf("config update without its schema or config: %v", u)
		}
		return "config " + strconv.FormatInt(u.Config.GetConfig().GetVersion(), 10), nil
	case *configpb.WatchResponse_Identity:
		if u.Identity.GetSchemaVersion().GetMajor() != config.SchemaMajor || u.Identity.GetIdentity().GetKeyFormat() != "spw1" {
			t.Errorf("identity update without its schema or key format: %v", u)
		}
		return "identity " + strconv.FormatInt(u.Identity.GetIdentity().GetVersion(), 10), nil
	}
	return "", errors.New("an empty update")
}

// TestWatch checks Watch (CP-06, ADR-0038): the current config, then the
// current identity, when the request's versions differ; nothing for a
// version the gateway has; then each new version, of either section, as
// it is published; a lower one too.
func TestWatch(t *testing.T) {
	src := newFakeSource(4, 7)
	client, _ := watchServer(t, src)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	s, err := client.Watch(ctx, watchReq("gateway-1", 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"config 4", "identity 7"} {
		if got, err := recv(t, s); got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	// A gateway with the current config gets only the identity.
	s2, _ := client.Watch(ctx, watchReq("gateway-2", 4, 6))
	if got, err := recv(t, s2); got != "identity 7" {
		t.Fatalf("gateway-2: %q, %v", got, err)
	}
	for _, step := range []struct {
		config, identity int64
		channel          string
		want             string
	}{
		{5, 7, store.ChannelConfig, "config 5"},
		{5, 8, store.ChannelIdentity, "identity 8"},
		{3, 8, store.ChannelConfig, "config 3"}, // a restored database
	} {
		src.set(step.config, step.identity)
		src.notify <- step.channel
		for _, st := range []grpc.ServerStreamingClient[configpb.WatchResponse]{s, s2} {
			if got, err := recv(t, st); got != step.want {
				t.Fatalf("got %q, %v; want %q", got, err, step.want)
			}
		}
	}
}

// TestWatchRefuses checks what Watch refuses (CP-18, ADR-0038): a schema
// of another major, or more than one minor behind; a replica_id that is
// not a DNS label. A gateway one minor behind, or ahead, is served.
func TestWatchRefuses(t *testing.T) {
	client, _ := watchServer(t, newFakeSource(1, 1))
	ctx := t.Context()
	for _, c := range []struct {
		name   string
		schema *configpb.SchemaVersion
		code   codes.Code
	}{
		{"gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor + 1}, codes.FailedPrecondition},
		{"gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor - 1, Minor: 99}, codes.FailedPrecondition},
		{"gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor - 2}, codes.FailedPrecondition},
		{"gateway-1", nil, codes.FailedPrecondition},
		{"Gateway_1", currentSchema(), codes.InvalidArgument},
		{"", currentSchema(), codes.InvalidArgument},
		{"gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor - 1}, codes.OK},
		{"gateway-2", &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor + 1}, codes.OK},
	} {
		s, err := client.Watch(ctx, &configpb.WatchRequest{ReplicaId: c.name, SchemaVersion: c.schema})
		if err != nil {
			t.Fatal(err)
		}
		_, err = recv(t, s)
		if status.Code(err) != c.code {
			t.Errorf("%q schema %v: %v, want %v", c.name, c.schema, err, c.code)
		}
	}
}

// TestWatchReplacedAndStepDown checks how streams end (ADR-0038): a second
// stream with a replica's name ends the first with ABORTED; a replica that
// steps down ends every stream with UNAVAILABLE.
func TestWatchReplacedAndStepDown(t *testing.T) {
	client, b := watchServer(t, newFakeSource(1, 1))
	ctx := t.Context()
	first, _ := client.Watch(ctx, watchReq("gateway-1", 1, 1))
	if _, err := first.Header(); err != nil { // registered
		t.Fatal(err)
	}
	second, _ := client.Watch(ctx, watchReq("gateway-1", 1, 0))
	if got, err := recv(t, second); got != "identity 1" {
		t.Fatalf("second stream: %q %v", got, err)
	}
	if _, err := recv(t, first); status.Code(err) != codes.Aborted {
		t.Errorf("replaced stream: %v", err)
	}
	b.stop()
	if _, err := recv(t, second); status.Code(err) != codes.Unavailable {
		t.Errorf("after step-down: %v", err)
	}
}

// fakeLock is an advisory lock shared by fake sessions: one holds it at a
// time; a session can be broken, which frees it as Postgres would.
type fakeLock struct {
	mu     sync.Mutex
	holder *fakeSession
}

type fakeSession struct {
	l      *fakeLock
	broken bool
	closed bool
}

func (l *fakeLock) open(context.Context) (Lock, error) { return &fakeSession{l: l}, nil }

func (s *fakeSession) TryLock(context.Context) (bool, error) {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	if s.broken || s.closed {
		return false, errors.New("conn closed")
	}
	if s.l.holder == nil {
		s.l.holder = s
	}
	return s.l.holder == s, nil
}

func (s *fakeSession) Ping(context.Context) error {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	if s.broken {
		return errors.New("conn reset")
	}
	return nil
}

func (s *fakeSession) Close(context.Context) error {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	s.closed = true
	if s.l.holder == s {
		s.l.holder = nil
	}
	return nil
}

// breakHolder breaks the holder's session and frees the lock.
func (l *fakeLock) breakHolder() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder.broken = true
	l.holder = nil
}

// replica is an Elector whose Active records how long it ran.
type replica struct {
	mu     sync.Mutex
	active bool
	spells int
}

func (r *replica) isActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *replica) elector(l *fakeLock) *Elector {
	return &Elector{Open: l.open, Log: discard(), Active: func(ctx context.Context) {
		r.mu.Lock()
		r.active = true
		r.spells++
		r.mu.Unlock()
		<-ctx.Done()
		r.mu.Lock()
		r.active = false
		r.mu.Unlock()
	}}
}

// TestElection checks the active replica (CP-22, D6, ADR-0038) on a fake
// clock: one of two replicas is active; a clean exit hands over within a
// second; a session that fails a ping steps its replica down within a
// second, and the standby takes over; the lock is released on exit.
func TestElection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lock := &fakeLock{}
		a, b := &replica{}, &replica{}
		actx, stopA := context.WithCancel(context.Background())
		aDone := make(chan struct{})
		go func() { defer close(aDone); a.elector(lock).Run(actx) }()
		synctest.Wait()
		bctx, stopB := context.WithCancel(context.Background())
		bDone := make(chan struct{})
		go func() { defer close(bDone); b.elector(lock).Run(bctx) }()
		synctest.Wait()
		if !a.isActive() || b.isActive() {
			t.Fatalf("active: a %v, b %v; want a only", a.isActive(), b.isActive())
		}

		// a exits cleanly: b takes over within its next try, a second.
		start := time.Now()
		stopA()
		<-aDone
		for !b.isActive() {
			time.Sleep(10 * time.Millisecond)
		}
		if d := time.Since(start); d > lockPoll+10*time.Millisecond {
			t.Errorf("handover after a clean exit took %v", d)
		}

		// b's session breaks (a paused or cut-off replica): it steps down at
		// its next ping, a new session takes the lock again.
		lock.breakHolder()
		start = time.Now()
		for b.isActive() {
			time.Sleep(10 * time.Millisecond)
		}
		if d := time.Since(start); d > lockPoll+10*time.Millisecond {
			t.Errorf("stepping down took %v", d)
		}
		for !b.isActive() {
			time.Sleep(10 * time.Millisecond)
		}
		if b.spells != 2 {
			t.Errorf("b was active %d times, want 2", b.spells)
		}
		stopB()
		<-bDone
		if lock.holder != nil {
			t.Error("the lock is held after every replica exited")
		}
	})
}
