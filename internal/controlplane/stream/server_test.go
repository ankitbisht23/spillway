package stream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/mtls"
	"github.com/ankitbisht23/spillway/internal/testcert"
)

// clients dials an in-memory ConfigService server as gateways: as(name)
// is a client whose certificate, from the stream CA, names replica name.
type clients struct {
	t     *testing.T
	ln    *bufconn.Listener
	ca    *testcert.CA
	conns map[string]configpb.ConfigServiceClient
}

// as is the client of replica name, dialed once.
func (c *clients) as(name string) configpb.ConfigServiceClient {
	c.t.Helper()
	if cl, ok := c.conns[name]; ok {
		return cl
	}
	dir := c.t.TempDir()
	c.ca.Issue(c.t, dir, testcert.Leaf{DNSNames: []string{name}, Client: true})
	cl := c.dial(loadFiles(c.t, dir).ClientConfig("controlplane"))
	c.conns[name] = cl
	return cl
}

// dial is a client with the TLS config cfg.
func (c *clients) dial(cfg *tls.Config) configpb.ConfigServiceClient {
	c.t.Helper()
	conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return c.ln.DialContext(ctx) }))
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = conn.Close() })
	return configpb.NewConfigServiceClient(conn)
}

func loadFiles(t *testing.T, dir string) *mtls.Reloader {
	t.Helper()
	r, err := mtls.Load(mtls.Files{Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"), CA: filepath.Join(dir, "ca.pem")}, nil, discard())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// watchServer serves ConfigService over an in-memory connection with the
// real options, mTLS and codec included, and returns its clients and the
// broadcaster.
func watchServer(t *testing.T, src *fakeSource) (*clients, *Broadcaster) {
	t.Helper()
	return configServer(t, src, &fakeRecorder{}, discard())
}

// configServer is watchServer with heartbeats recorded in rec and the
// server's logs in log.
func configServer(t *testing.T, src *fakeSource, rec Recorder, log *slog.Logger) (*clients, *Broadcaster) {
	t.Helper()
	b := started(t, src)
	ca := testcert.NewCA(t, "stream CA")
	dir := t.TempDir()
	ca.Issue(t, dir, testcert.Leaf{DNSNames: []string{"controlplane"}})
	ln := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(serverOptions(loadFiles(t, dir).ServerConfig(), log)...)
	configpb.RegisterConfigServiceServer(gs, NewService(b, rec, log))
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	return &clients{t: t, ln: ln, ca: ca, conns: map[string]configpb.ConfigServiceClient{}}, b
}

func watchReq(name, epoch string, config, identity int64) *configpb.WatchRequest {
	return &configpb.WatchRequest{ReplicaId: name, ConfigVersion: config, IdentityVersion: identity, SchemaVersion: currentSchema(), Epoch: epoch}
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
	c, _ := watchServer(t, src)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	s, err := c.as("gateway-1").Watch(ctx, watchReq("gateway-1", "", 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"config 4", "identity 7"} {
		if got, err := recv(t, s); got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	// A gateway with the current config gets only the identity.
	epoch, _ := src.currentEpoch()
	s2, _ := c.as("gateway-2").Watch(ctx, watchReq("", epoch, 4, 6)) // replica_id is optional
	if got, err := recv(t, s2); got != "identity 7" {
		t.Fatalf("gateway-2: %q, %v", got, err)
	}
	for _, step := range []struct {
		config, identity int64
		channel          string
		want             []string
	}{
		{5, 7, store.ChannelConfig, []string{"config 5"}},
		{5, 8, store.ChannelIdentity, []string{"identity 8"}},
		// A restored database: a new epoch, both sections (ADR-0055).
		{3, 8, store.ChannelConfig, []string{"config 3", "identity 8"}},
	} {
		src.set(step.config, step.identity)
		src.notify <- step.channel
		for _, st := range []grpc.ServerStreamingClient[configpb.WatchResponse]{s, s2} {
			for _, want := range step.want {
				if got, err := recv(t, st); got != want {
					t.Fatalf("got %q, %v; want %q", got, err, want)
				}
			}
		}
	}
}

// TestWatchEpoch checks the stream epoch on Watch (CP-06, ADR-0055): a
// gateway whose epoch differs from the control plane's, or that has none,
// gets both sections at equal version numbers, each under the control
// plane's epoch; one with the same epoch and versions gets nothing until
// the next version.
func TestWatchEpoch(t *testing.T) {
	src := newFakeSource(4, 7)
	c, _ := watchServer(t, src)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	epoch, _ := src.currentEpoch()
	for name, gatewayEpoch := range map[string]string{"gateway-1": "0123456789abcdef0123456789abcdef", "gateway-2": ""} {
		s, err := c.as(name).Watch(ctx, watchReq(name, gatewayEpoch, 4, 7))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"config 4", "identity 7"} {
			m, err := s.Recv()
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprint(m.GetConfig().GetConfig().GetVersion() + m.GetIdentity().GetIdentity().GetVersion())
			if e := m.GetConfig().GetEpoch() + m.GetIdentity().GetEpoch(); !strings.HasSuffix(want, " "+got) || e != epoch {
				t.Fatalf("%s with epoch %q: version %s, epoch %q; want %q under %q", name, gatewayEpoch, got, e, want, epoch)
			}
		}
	}
	same, err := c.as("gateway-3").Watch(ctx, watchReq("gateway-3", epoch, 4, 7))
	if err != nil {
		t.Fatal(err)
	}
	src.set(4, 8)
	src.notify <- store.ChannelIdentity
	if got, err := recv(t, same); got != "identity 8" {
		t.Fatalf("same epoch and versions: first message %q, %v; want nothing before identity 8", got, err)
	}
}

// TestWatchRefuses checks what Watch refuses (CP-18, ADR-0038, ADR-0040):
// a schema of another major, or more than one minor behind; a replica_id
// that is not the client certificate's name. A gateway one minor behind,
// or ahead, is served.
func TestWatchRefuses(t *testing.T) {
	c, _ := watchServer(t, newFakeSource(1, 1))
	ctx := t.Context()
	for _, tc := range []struct {
		cert, id string
		schema   *configpb.SchemaVersion
		code     codes.Code
	}{
		{"gateway-1", "gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor + 1}, codes.FailedPrecondition},
		{"gateway-1", "gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor - 1, Minor: 99}, codes.FailedPrecondition},
		{"gateway-1", "gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor - 2}, codes.FailedPrecondition},
		{"gateway-1", "gateway-1", nil, codes.FailedPrecondition},
		{"gateway-1", "gateway-2", currentSchema(), codes.PermissionDenied},
		{"gateway-1", "Gateway_1", currentSchema(), codes.PermissionDenied},
		{"gateway-1", "gateway-1", &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor - 1}, codes.OK},
		{"gateway-2", "", &configpb.SchemaVersion{Major: config.SchemaMajor, Minor: config.SchemaMinor + 1}, codes.OK},
	} {
		s, err := c.as(tc.cert).Watch(ctx, &configpb.WatchRequest{ReplicaId: tc.id, SchemaVersion: tc.schema})
		if err != nil {
			t.Fatal(err)
		}
		_, err = recv(t, s)
		if status.Code(err) != tc.code {
			t.Errorf("certificate %q, replica_id %q, schema %v: %v, want %v", tc.cert, tc.id, tc.schema, err, tc.code)
		}
	}
}

// TestWatchReplacedAndStepDown checks how streams end (ADR-0038): a second
// stream with a replica's name ends the first with ABORTED; a replica that
// steps down ends every stream with UNAVAILABLE.
func TestWatchReplacedAndStepDown(t *testing.T) {
	src := newFakeSource(1, 1)
	c, b := watchServer(t, src)
	ctx := t.Context()
	epoch, _ := src.currentEpoch()
	first, _ := c.as("gateway-1").Watch(ctx, watchReq("gateway-1", epoch, 1, 1))
	if _, err := first.Header(); err != nil { // registered
		t.Fatal(err)
	}
	second, _ := c.as("gateway-1").Watch(ctx, watchReq("gateway-1", epoch, 1, 0))
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

// failingLock is a session whose every TryLock fails, as on a database
// that accepts connections but fails queries.
type failingLock struct{}

func (failingLock) TryLock(context.Context) (bool, error) { return false, errors.New("conn reset") }
func (failingLock) Ping(context.Context) error            { return nil }
func (failingLock) Close(context.Context) error           { return nil }

// lineCounter counts the log lines written to it.
type lineCounter struct {
	mu sync.Mutex
	n  int
}

func (c *lineCounter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += strings.Count(string(p), "\n")
	return len(p), nil
}

// TestElectionTryLockFails: when sessions open but every try of the lock
// fails, the elector reopens a session once a second, not in a busy loop,
// and logs the failure once (ADR-0038).
func TestElectionTryLockFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		const opens = 6
		var n int
		start := time.Now()
		logs := &lineCounter{}
		e := &Elector{
			Open: func(context.Context) (Lock, error) {
				if n++; n == opens {
					cancel()
				}
				return failingLock{}, nil
			},
			Active: func(context.Context) { t.Error("active without the lock") },
			Log:    slog.New(slog.NewTextHandler(logs, nil)),
		}
		e.Run(ctx)
		if d := time.Since(start); d != (opens-1)*lockPoll {
			t.Errorf("%d opens took %v, want %v: a failed try waits lockPoll before the next open", opens, d, (opens-1)*lockPoll)
		}
		if logs.n != 1 {
			t.Errorf("%d log lines, want 1: the failure is logged once", logs.n)
		}
	})
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
