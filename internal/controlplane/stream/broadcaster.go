// Package stream is the control plane's side of the config stream (CP-06,
// CP-22, ADR-0038): the broadcaster that holds the latest config and
// identity, the ConfigService that sends them to gateways, and the
// election of the one active replica that serves it.
package stream

import (
	"context"
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
	LatestConfig(ctx context.Context) (*configpb.ConfigUpdate, error)
	Identity(ctx context.Context) (*configpb.Identity, error)
	LatestVersions(ctx context.Context) (configVersion, identityVersion int64, err error)
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

// update is one section's version, marshalled once as the WatchResponse
// that every stream sends.
type update struct {
	version int64
	msg     encoded
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
	cur     [sections]*update
	have    [sections]bool // the section was read at least once
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

// Run keeps the broadcaster current until ctx ends, then closes every
// subscriber with UNAVAILABLE: the replica is stepping down or stopping.
func (b *Broadcaster) Run(ctx context.Context) {
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

// refresh reads section s and, if its version differs from the one held,
// builds its update and wakes every subscriber. A different version is
// sent whether higher or lower: Postgres is the source of truth.
func (b *Broadcaster) refresh(ctx context.Context, s section) error {
	cv, iv, err := b.src.LatestVersions(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	cur, have := b.cur[s], b.have[s]
	b.mu.Unlock()
	if want := [...]int64{cv, iv}[s]; have && (cur == nil && want == 0 || cur != nil && cur.version == want) {
		return nil
	}
	var u *update
	switch s {
	case sectionConfig:
		c, err := b.src.LatestConfig(ctx)
		if err != nil {
			return err
		}
		if c != nil {
			if u, err = marshal(c.GetConfig().GetVersion(), &configpb.WatchResponse{Update: &configpb.WatchResponse_Config{Config: c}}); err != nil {
				return err
			}
		}
	case sectionIdentity:
		id, err := b.src.Identity(ctx)
		if err != nil {
			return err
		}
		if u, err = marshal(id.GetVersion(), &configpb.WatchResponse{Update: &configpb.WatchResponse_Identity{Identity: &configpb.IdentityUpdate{
			SchemaVersion: currentSchema(), Identity: id,
		}}}); err != nil {
			return err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur[s], b.have[s] = u, true
	if b.have[sectionConfig] && b.have[sectionIdentity] {
		select {
		case <-b.loaded:
		default:
			close(b.loaded)
		}
	}
	for _, sub := range b.subs {
		sub.wakeUp()
	}
	if u != nil {
		b.log.Info(s.String()+" ready to stream", "version", u.version, "bytes", len(u.msg), "streams", len(b.subs))
	}
	return nil
}

func marshal(version int64, m *configpb.WatchResponse) (*update, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &update{version: version, msg: b}, nil
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
