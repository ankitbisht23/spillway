package stream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// fakeSource is the store as the broadcaster sees it: versions set by the
// test, notifications sent on a channel, and a LISTEN connection the test
// can break. Every wait is on a channel, so it runs in a synctest bubble,
// whose fake clock moves when every goroutine is blocked.
type fakeSource struct {
	mu       sync.Mutex
	config   int64 // 0: nothing published
	identity int64
	reads    [sections]int // section reads
	fail     error         // what LatestConfig returns, if not nil

	notify  chan string // a notification on the LISTEN connection
	breakLn chan error  // ends the LISTEN connection with the error
	listens int         // LISTEN connections opened
	block   chan struct{}
}

func newFakeSource(config, identity int64) *fakeSource {
	return &fakeSource{config: config, identity: identity, notify: make(chan string), breakLn: make(chan error)}
}

func (f *fakeSource) set(config, identity int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config, f.identity = config, identity
}

func (f *fakeSource) readCount(s section) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[s]
}

func (f *fakeSource) LatestVersions(context.Context) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.config, f.identity, nil
}

// LatestConfig reads the version current when it starts, as a
// transaction's snapshot would, then waits on block, if set.
func (f *fakeSource) LatestConfig(context.Context) (*configpb.ConfigUpdate, error) {
	f.mu.Lock()
	block, v, fail := f.block, f.config, f.fail
	f.reads[sectionConfig]++
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if fail != nil {
		return nil, fail
	}
	if v == 0 {
		return nil, nil
	}
	return &configpb.ConfigUpdate{SchemaVersion: currentSchema(), Config: &configpb.Config{Version: v}}, nil
}

func (f *fakeSource) listenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listens
}

func (f *fakeSource) Identity(context.Context) (*configpb.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[sectionIdentity]++
	return &configpb.Identity{Version: f.identity, KeyFormat: "spw1"}, nil
}

func (f *fakeSource) Listen(ctx context.Context, ready func(), notify func(string)) error {
	f.mu.Lock()
	f.listens++
	f.mu.Unlock()
	ready()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-f.breakLn:
			return err
		case ch := <-f.notify:
			notify(ch)
		}
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// started runs a broadcaster over src until the test ends.
func started(t *testing.T, src Source) *Broadcaster {
	t.Helper()
	b := NewBroadcaster(src, discard(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-b.Loaded()
	return b
}

func version(t *testing.T, u *update) int64 {
	t.Helper()
	if u == nil {
		return 0
	}
	var m configpb.WatchResponse
	if err := proto.Unmarshal(u.msg, &m); err != nil {
		t.Fatal(err)
	}
	if v := m.GetConfig().GetConfig().GetVersion() + m.GetIdentity().GetIdentity().GetVersion(); v != u.version {
		t.Fatalf("update of version %d holds version %d", u.version, v)
	}
	return u.version
}

func woken(sub *Subscriber) bool {
	select {
	case <-sub.wake:
		return true
	default:
		return false
	}
}

// TestBroadcasterNotifications checks that a notification on either
// channel brings that section's new version, marshalled once, and wakes
// every subscriber (ADR-0038).
func TestBroadcasterNotifications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource(0, 1)
		b := started(t, src)
		if c, id := b.current(sectionConfig), b.current(sectionIdentity); c != nil || version(t, id) != 1 {
			t.Fatalf("loaded config %v, identity %v; want none and 1", c, id)
		}
		a, _, _ := b.subscribe("gateway-1")
		c, _, _ := b.subscribe("gateway-2")

		src.set(3, 1)
		src.notify <- store.ChannelConfig
		synctest.Wait()
		if v := version(t, b.current(sectionConfig)); v != 3 || !woken(a) || !woken(c) {
			t.Errorf("after the config notification: version %d, woken %v %v", v, woken(a), woken(c))
		}
		src.set(3, 2)
		src.notify <- store.ChannelIdentity
		synctest.Wait()
		if v := version(t, b.current(sectionIdentity)); v != 2 || !woken(a) {
			t.Errorf("after the identity notification: version %d", v)
		}
		// A notification without a new version reads only the versions.
		reads := src.readCount(sectionConfig)
		src.notify <- store.ChannelConfig
		synctest.Wait()
		if src.readCount(sectionConfig) != reads || woken(a) {
			t.Errorf("an unchanged version was read again or woke a stream")
		}
		// A lower version (a restored database) is the one to send.
		src.set(2, 2)
		src.notify <- store.ChannelConfig
		synctest.Wait()
		if v := version(t, b.current(sectionConfig)); v != 2 {
			t.Errorf("after the database went back: version %d, want 2", v)
		}
	})
}

// TestBroadcasterSafetyRead checks that a lost notification is caught by
// the read of both versions every 10 s (ADR-0038).
func TestBroadcasterSafetyRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource(1, 1)
		b := started(t, src)
		synctest.Wait() // the reads after LISTEN is up are done
		start := time.Now()
		src.set(2, 5) // no notification
		time.Sleep(safetyInterval - time.Millisecond)
		synctest.Wait()
		if version(t, b.current(sectionConfig)) != 1 {
			t.Fatal("the new version came before the safety read")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if c, id := version(t, b.current(sectionConfig)), version(t, b.current(sectionIdentity)); c != 2 || id != 5 {
			t.Errorf("after %v: config %d, identity %d; want 2 and 5", time.Since(start), c, id)
		}
	})
}

// TestBroadcasterRelisten checks the LISTEN connection's reconnects: a
// backoff from 100 ms doubling, both sections read again after each
// reconnect, so a notification lost while it was down is caught at once.
func TestBroadcasterRelisten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource(1, 1)
		b := started(t, src)
		for i, wait := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
			src.breakLn <- errors.New("connection reset")
			src.set(int64(i+2), 1) // committed while LISTEN was down
			time.Sleep(wait - time.Millisecond)
			synctest.Wait()
			if src.listenCount() != i+1 {
				t.Fatalf("reconnect %d came before its %v backoff", i+1, wait)
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if src.listenCount() != i+2 || version(t, b.current(sectionConfig)) != int64(i+2) {
				t.Fatalf("after %v: %d connections, config %d", wait, src.listenCount(), version(t, b.current(sectionConfig)))
			}
		}
	})
}

// TestBroadcasterCoalesces checks that notifications arriving while a
// snapshot is being read make one more read, not one each.
func TestBroadcasterCoalesces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource(1, 1)
		b := started(t, src)
		block := make(chan struct{})
		src.mu.Lock()
		src.block = block
		src.mu.Unlock()
		src.set(2, 1)
		src.notify <- store.ChannelConfig
		synctest.Wait() // the read of 2 waits on block
		reads := src.readCount(sectionConfig)
		for v := int64(3); v <= 10; v++ {
			src.set(v, 1)
			src.notify <- store.ChannelConfig
		}
		src.mu.Lock()
		src.block = nil
		src.mu.Unlock()
		close(block)
		synctest.Wait()
		if got := src.readCount(sectionConfig) - reads; got != 1 || version(t, b.current(sectionConfig)) != 10 {
			t.Errorf("%d more reads after 8 notifications during one read, version %d; want 1 and 10", got, version(t, b.current(sectionConfig)))
		}
	})
}

// TestBroadcasterKeepsLastOnError checks that a version that cannot be
// read (another schema major) leaves the last one in place.
func TestBroadcasterKeepsLastOnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := newFakeSource(1, 1)
		b := started(t, src)
		src.mu.Lock()
		src.fail = errors.New("config version 2 has schema 2.0")
		src.mu.Unlock()
		src.set(2, 1)
		src.notify <- store.ChannelConfig
		synctest.Wait()
		if v := version(t, b.current(sectionConfig)); v != 1 {
			t.Errorf("after an unreadable version: %d, want 1", v)
		}
	})
}

// TestBroadcasterSubscribers checks the mailboxes: a second stream with a
// replica's name closes the first with ABORTED, and stopping closes all
// with UNAVAILABLE and refuses new ones (ADR-0038).
func TestBroadcasterSubscribers(t *testing.T) {
	var opened int
	b := NewBroadcaster(newFakeSource(1, 1), discard(), func(d int) { opened += d })
	first, replaced, err := b.subscribe("gateway-1")
	if err != nil || replaced {
		t.Fatal(err, replaced)
	}
	second, replaced, _ := b.subscribe("gateway-1")
	other, _, _ := b.subscribe("gateway-2")
	select {
	case <-first.done:
		if status.Code(first.err) != codes.Aborted || !replaced {
			t.Errorf("replaced stream: %v", first.err)
		}
	default:
		t.Fatal("the replaced stream is still open")
	}
	b.unsubscribe(first) // the old stream's handler returns: the new one stays
	if opened != 2 {
		t.Errorf("%d streams counted, want 2", opened)
	}
	b.stop()
	for _, s := range []*Subscriber{second, other} {
		<-s.done
		if status.Code(s.err) != codes.Unavailable {
			t.Errorf("%s after stop: %v", s.name, s.err)
		}
		b.unsubscribe(s)
	}
	if _, _, err := b.subscribe("gateway-3"); status.Code(err) != codes.Unavailable {
		t.Errorf("subscribe after stop: %v", err)
	}
	if opened != 0 {
		t.Errorf("%d streams counted after stop", opened)
	}
}
