package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Watcher serves the snapshot compiled from a config file and reloads it when
// the file's content changes (CP-05, GW-21). It polls rather than watching for
// events, which also catches the symlink swaps of a Kubernetes ConfigMap.
type Watcher struct {
	path    string
	log     *slog.Logger
	current atomic.Pointer[Snapshot]
	sum     [sha256.Size]byte // content last tried, good or bad
}

// NewWatcher loads the file once; an invalid file is an error.
func NewWatcher(path string, log *slog.Logger) (*Watcher, error) {
	w := &Watcher{path: path, log: log}
	if err := w.reload(); err != nil {
		return nil, err
	}
	return w, nil
}

// Snapshot returns the current snapshot. A request calls it once and uses the
// result to the end, so a reload never changes a request midway.
func (w *Watcher) Snapshot() *Snapshot { return w.current.Load() }

// Run re-reads the file every interval until ctx ends. A file that fails to
// load or compile leaves the current snapshot in place.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.reload(); err != nil {
				w.log.Error("config reload failed; keeping the current config", "path", w.path, "version", w.Snapshot().Version, "err", err)
			}
		}
	}
}

// reload swaps in a new snapshot if the file changed. The same bad content is
// reported once, not on every poll.
func (w *Watcher) reload() error {
	data, err := os.ReadFile(w.path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	sum := sha256.Sum256(data)
	if w.current.Load() != nil && sum == w.sum {
		return nil
	}
	w.sum = sum
	c, err := Load(data)
	if err != nil {
		return err
	}
	s, err := Compile(c)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	w.current.Store(s)
	w.log.Info("config loaded", "path", w.path, "version", s.Version, "routes", len(s.Routes), "upstreams", len(s.Upstreams))
	return nil
}
