package stream

import (
	"context"
	"log/slog"
	"time"
)

// Lock is a session that takes and holds the active replica's advisory
// lock (store.LockSession).
type Lock interface {
	TryLock(ctx context.Context) (bool, error)
	Ping(ctx context.Context) error
	Close(ctx context.Context) error
}

// The election's timing (D6, ADR-0038).
const (
	// lockPoll is how often a standby tries the lock, and how often the
	// active replica pings its session.
	lockPoll = time.Second
	// pingTimeout bounds a ping: a slower answer means the lock may be
	// gone.
	pingTimeout  = 5 * time.Second
	closeTimeout = 2 * time.Second
)

// Elector makes this process the active control-plane replica while it
// holds the advisory lock (CP-22, D6, ADR-0038). Standbys try the lock
// every second. The active replica runs Active until its session fails a
// ping, then steps down and becomes a standby again.
type Elector struct {
	Open   func(ctx context.Context) (Lock, error)
	Active func(ctx context.Context) // runs while active; returns soon after ctx ends
	// Changed, if not nil, is told each change of role (cp_active).
	Changed func(active bool)
	Log     *slog.Logger
}

// Run elects until ctx ends. On return the lock is released and Active
// has returned.
func (e *Elector) Run(ctx context.Context) {
	failing := false // log a failing database once, not every second
	for ctx.Err() == nil {
		lock, err := e.Open(ctx)
		if err != nil {
			if !failing && ctx.Err() == nil {
				e.Log.Error("cannot open the lock session; retrying every second", "err", err)
			}
			failing = true
			sleep(ctx, lockPoll)
			continue
		}
		if failing {
			e.Log.Info("lock session open")
			failing = false
		}
		held := e.standby(ctx, lock)
		if held {
			e.active(ctx, lock)
		}
		cctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		_ = lock.Close(cctx) // frees the lock, if held
		cancel()
		if held {
			sleep(ctx, lockPoll) // a standby may take over before this process tries again
		}
	}
}

// standby tries the lock every second until it holds it (true), the
// session fails or ctx ends.
func (e *Elector) standby(ctx context.Context, lock Lock) bool {
	for {
		ok, err := lock.TryLock(ctx)
		if err != nil {
			if ctx.Err() == nil {
				e.Log.Warn("trying the lock failed; reopening the session", "err", err)
			}
			return false
		}
		if ok {
			return true
		}
		if !sleep(ctx, lockPoll) {
			return false
		}
	}
}

// active runs Active while lock answers its pings and ctx lasts.
func (e *Elector) active(ctx context.Context, lock Lock) {
	e.Log.Info("active: holding the lock")
	e.changed(true)
	actx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Active(actx)
	}()
	t := time.NewTicker(lockPoll)
	for alive := true; alive; {
		select {
		case <-ctx.Done():
			alive = false
		case <-done:
			alive = false
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := lock.Ping(pctx)
			pcancel()
			if err != nil && ctx.Err() == nil {
				e.Log.Error("the lock session failed a ping; stepping down", "err", err)
				alive = false
			}
		}
	}
	t.Stop()
	cancel()
	<-done
	e.changed(false)
	if ctx.Err() == nil {
		e.Log.Info("standby: stepped down")
	}
}

func (e *Elector) changed(active bool) {
	if e.Changed != nil {
		e.Changed(active)
	}
}

// sleep waits d, or until ctx ends (false).
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
