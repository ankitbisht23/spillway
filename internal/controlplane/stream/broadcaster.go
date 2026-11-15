// Package stream is the control plane's side of the config stream (CP-06,
// CP-22, ADR-0038): the broadcaster that holds the latest config and
// identity, the ConfigService that sends them to gateways, and the
// election of the one active replica that serves it.
package stream

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Source is what the broadcaster reads: the store.
type Source interface {
	// StreamConfig and StreamIdentity return a section with the stream
	// epoch it was read under, and when its version was made (the
	// config's published_at); StreamConfig's Config is nil before the
	// first publish.
	StreamConfig(ctx context.Context) (*configpb.ConfigUpdate, error)
	StreamIdentity(ctx context.Context) (*configpb.IdentityUpdate, time.Time, error)
	LatestVersions(ctx context.Context) (store.Versions, error)
	// RotateEpoch sets a new stream epoch, only while the epoch is old
	// unless old is empty; it returns the epoch now in place and whether
	// it set it (ADR-0055).
	RotateEpoch(ctx context.Context, old string) (epoch string, rotated bool, err error)
	Listen(ctx context.Context, ready func(), notify func(channel string)) error
}

// section is a part of the stream with its own versions.
type section int

const (
	sectionConfig section = iota
	sectionIdentity
	sections
)

func (s section) String() string { return [...]string{"config", "identity"}[s] }

// update is one section's version and the stream epoch it was read
// under, marshalled once as the WatchResponse that every stream sends.
type update struct {
	version int64
	at      time.Time // when the version was made; zero for no config
	epoch   string
	msg     encoded // nil: nothing to send (no config published yet)
}

// The broadcaster's timing (ADR-0038).
const (
	// safetyInterval is how often the versions are read besides the
	// notifications, which bounds how late a lost one can be.
	safetyInterval = 10 * time.Second
	// The LISTEN connection's reconnect backoff.
	listenBackoffMin = 100 * time.Millisecond
	listenBackoffMax = 5 * time.Second
)

// Broadcaster holds the latest config and identity of the active replica
// and wakes every Watch stream when either changes (ADR-0038). It learns of
// new versions from LISTEN and, every 10 s, from reading both versions.
// Each subscriber has a one-slot mailbox, so a slow gateway is sent the
// newest snapshot and never holds up the others.
type Broadcaster struct {
	src     Source
	log     *slog.Logger
	streams func(delta int) // counts open streams (cp_watch_streams)

	kick   [sections]chan struct{} // one slot each: a refresh is due
	loaded chan struct{}           // closed once both sections are read

	mu      sync.Mutex
	cur     [sections]*update // nil until the section is read
	subs    map[string]*Subscriber
	stopped bool
}

// NewBroadcaster returns a broadcaster over src; streams, if not nil, is
// told of each stream opened (+1) and closed (-1).
func NewBroadcaster(src Source, log *slog.Logger, streams func(delta int)) *Broadcaster {
	b := &Broadcaster{src: src, log: log, streams: streams, loaded: make(chan struct{}), subs: map[string]*Subscriber{}}
	if b.streams == nil {
		b.streams = func(int) {}
	}
	for i := range b.kick {
		b.kick[i] = make(chan struct{}, 1)
	}
	return b
}

// Loaded is closed once the broadcaster has read both sections.
func (b *Broadcaster) Loaded() <-chan struct{} { return b.loaded }

// Run starts a new stream epoch, keeps the broadcaster current until ctx
// ends, then closes every subscriber with UNAVAILABLE: the replica is
// stepping down or stopping.
func (b *Broadcaster) Run(ctx context.Context) {
	if !b.startEpoch(ctx) {
		b.stop()
		return
	}
	var wg sync.WaitGroup
	for s := range sections {
		wg.Go(func() { b.refreshLoop(ctx, s) })
	}
	wg.Go(func() { b.listenLoop(ctx) })
	t := time.NewTicker(safetyInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			b.stop()
			return
		case <-t.C:
			b.kickAll()
		}
	}
}

// startEpoch begins the replica's active term with a new stream epoch, so
// that a database restored while no replica was active cannot hand a
// gateway different content under the version numbers it has (ADR-0055).
// It retries with the LISTEN connection's backoff until it succeeds, or
// ctx ends (false).
func (b *Broadcaster) startEpoch(ctx context.Context) bool {
	backoff := listenBackoffMin
	for {
		epoch, _, err := b.src.RotateEpoch(ctx, "")
		if err == nil {
			b.log.Info("new stream epoch for this active term", "epoch", epoch)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		b.log.Error("setting a new stream epoch failed; retrying", "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, listenBackoffMax)
	}
}

func (b *Broadcaster) kickAll() {
	for s := range sections {
		b.kickSection(s)
	}
}

func (b *Broadcaster) kickSection(s section) {
	select {
	case b.kick[s] <- struct{}{}:
	default: // a refresh is already due; it reads the newest
	}
}

// listenLoop keeps the LISTEN connection up; after each (re)connect it
// refreshes both sections, which catches what was missed while it was down.
func (b *Broadcaster) listenLoop(ctx context.Context) {
	backoff := listenBackoffMin
	for {
		up := time.Now()
		err := b.src.Listen(ctx, b.kickAll, func(channel string) {
			switch channel {
			case store.ChannelConfig:
				b.kickSection(sectionConfig)
			case store.ChannelIdentity:
				b.kickSection(sectionIdentity)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(up) > listenBackoffMax {
			backoff = listenBackoffMin
		}
		b.log.Warn("LISTEN connection lost; reconnecting", "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, listenBackoffMax)
	}
}

// refreshLoop re-reads one section each time it is kicked; kicks that come
// while it reads are coalesced into one more read.
func (b *Broadcaster) refreshLoop(ctx context.Context, s section) {
	b.kickSection(s) // the first read
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.kick[s]:
		}
		if err := b.refresh(ctx, s); err != nil && ctx.Err() == nil {
			// The next notification or safety read tries again.
			b.log.Error("reading the "+s.String()+" for the config stream failed; keeping the last one", "err", err)
		}
	}
}

// refresh reads section s and, if its version, the time that version was
// made or the stream epoch differs from what is held, builds its update
// and wakes every subscriber. A different version is sent whether higher
// or lower: Postgres is the source of truth. Under the epoch held, a lower
// version, or the same version made at another time, means the database
// was restored from an older copy of itself: the update is not kept, a new
// epoch is set, and the section is read again under it, so every gateway,
// including one offline now, gets both sections in full (ADR-0055).
func (b *Broadcaster) refresh(ctx context.Context, s section) error {
	v, err := b.src.LatestVersions(ctx)
	if err != nil {
		return err
	}
	want, wantAt := v.Config, v.ConfigAt
	if s == sectionIdentity {
		want, wantAt = v.Identity, v.IdentityAt
	}
	cur := b.current(s)
	if cur != nil && cur.epoch == v.Epoch && cur.version == want && cur.at.Equal(wantAt) {
		return nil
	}
	u, err := b.read(ctx, s)
	if err != nil {
		return err
	}
	if cur != nil && u.epoch == cur.epoch && (u.version < cur.version || u.version == cur.version && !u.at.Equal(cur.at)) {
		next, rotated, err := b.src.RotateEpoch(ctx, u.epoch)
		if err != nil {
			return fmt.Errorf("the %s went back from version %d to %d; setting a new stream epoch: %w", s, cur.version, u.version, err)
		}
		if rotated {
			b.log.Warn("the "+s.String()+" went back under the same stream epoch: the database was restored; new stream epoch",
				"from_version", cur.version, "from_made_at", cur.at, "to_version", u.version, "to_made_at", u.at, "epoch", next)
		}
		b.kickSection(s)
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur[s] = u
	if other := b.cur[1-s]; other != nil && other.epoch != u.epoch {
		b.kickSection(1 - s) // it is sent again under this epoch
	}
	if b.cur[sectionConfig] != nil && b.cur[sectionIdentity] != nil {
		select {
		case <-b.loaded:
		default:
			close(b.loaded)
		}
	}
	for _, sub := range b.subs {
		sub.wakeUp()
	}
	if u.msg != nil {
		b.log.Info(s.String()+" ready to stream", "version", u.version, "epoch", u.epoch, "bytes", len(u.msg), "streams", len(b.subs))
	}
	return nil
}

// read reads section s as the stream sends it.
func (b *Broadcaster) read(ctx context.Context, s section) (*update, error) {
	var m *configpb.WatchResponse
	u := &update{}
	switch s {
	case sectionConfig:
		c, err := b.src.StreamConfig(ctx)
		if err != nil {
			return nil, err
		}
		u.version, u.epoch = c.GetConfig().GetVersion(), c.GetEpoch()
		if c.GetConfig() != nil {
			u.at = c.GetPublishedAt().AsTime()
			m = &configpb.WatchResponse{Update: &configpb.WatchResponse_Config{Config: c}}
		}
	case sectionIdentity:
		id, at, err := b.src.StreamIdentity(ctx)
		if err != nil {
			return nil, err
		}
		u.version, u.at, u.epoch = id.GetIdentity().GetVersion(), at, id.GetEpoch()
		m = &configpb.WatchResponse{Update: &configpb.WatchResponse_Identity{Identity: id}}
	}
	if m != nil {
		var err error
		if u.msg, err = proto.Marshal(m); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// current returns what section s holds, nil for none.
func (b *Broadcaster) current(s section) *update {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur[s]
}

// errStepDown closes the streams of a replica that is no longer active.
var errStepDown = status.Error(codes.Unavailable, "the control plane replica is stepping down; reconnect")

// stop closes every subscriber with errStepDown; later subscriptions fail
// with it.
func (b *Broadcaster) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	for name, sub := range b.subs {
		sub.close(errStepDown)
		delete(b.subs, name)
		b.streams(-1)
	}
}

// Subscriber is one Watch stream's mailbox.
type Subscriber struct {
	name string
	wake chan struct{} // one slot: something changed since the last look
	done chan struct{} // closed when the stream must end, with err
	err  error
}

func (s *Subscriber) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Subscriber) close(err error) {
	s.err = err
	close(s.done)
}

// errReplaced closes a stream when another one with the same replica name
// opens (ADR-0038).
var errReplaced = status.Error(codes.Aborted, "another Watch stream with this replica name replaced this one")

// subscribe registers a stream for replica name; an existing stream of
// that name is closed with ABORTED, and reports whether one was.
func (b *Broadcaster) subscribe(name string) (sub *Subscriber, replaced bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return nil, false, errStepDown
	}
	if old := b.subs[name]; old != nil {
		old.close(errReplaced)
		replaced = true
	} else {
		b.streams(1)
	}
	sub = &Subscriber{name: name, wake: make(chan struct{}, 1), done: make(chan struct{})}
	b.subs[name] = sub
	return sub, replaced, nil
}

// unsubscribe removes sub, unless another stream has replaced it or the
// broadcaster has stopped.
func (b *Broadcaster) unsubscribe(sub *Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[sub.name] == sub {
		delete(b.subs, sub.name)
		b.streams(-1)
	}
}
