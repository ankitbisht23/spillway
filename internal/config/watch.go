package config

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Watcher serves the snapshot compiled from a config file and reloads it when
// the file's content changes (CP-05, GW-21). It polls rather than watching for
// events, which also catches the symlink swaps of a Kubernetes ConfigMap.
type Watcher struct{ watcher[Snapshot] }

// NewWatcher loads the file once; an invalid file is an error. apply, if not
// nil, runs on each new snapshot before it is swapped in; an error rejects the
// snapshot. The gateway uses it to check upstreams and reconcile runtime
// state (GW-29).
func NewWatcher(path string, log *slog.Logger, apply func(*Snapshot) error) (*Watcher, error) {
	w := &Watcher{watcher[Snapshot]{path: path, name: "config", log: log}}
	w.compile = func(data []byte) (*Snapshot, error) {
		c, err := Load(data)
		if err != nil {
			return nil, err
		}
		return Prepare(c, log, apply)
	}
	w.describe = func(s *Snapshot) []any {
		return []any{"version", s.Version, "routes", len(s.Routes), "upstreams", len(s.Upstreams), "policies", len(s.Policies)}
	}
	if err := w.reload(); err != nil {
		return nil, err
	}
	return w, nil
}

// Prepare compiles c and runs apply, if not nil, on the result, as a reload
// does before it swaps the snapshot in; an error rejects c. Static mode's
// file reloads and managed mode's stream updates both go through it
// (GW-21, GW-29, ADR-0038).
func Prepare(c *configpb.Config, log *slog.Logger, apply func(*Snapshot) error) (*Snapshot, error) {
	return prepare(Compile, c, log, apply)
}

// PrepareManaged is Prepare for a managed gateway's configs, compiled with
// CompileManaged.
func PrepareManaged(c *configpb.Config, log *slog.Logger, apply func(*Snapshot) error) (*Snapshot, error) {
	return prepare(CompileManaged, c, log, apply)
}

func prepare(compile func(*configpb.Config) (*Snapshot, error), c *configpb.Config, log *slog.Logger, apply func(*Snapshot) error) (*Snapshot, error) {
	s, err := compile(c)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if apply != nil {
		if err := apply(s); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
	}
	warnClientChosenKeys(s, log)
	return s, nil
}

// Snapshot returns the current snapshot. A request calls it once and uses the
// result to the end, so a reload never changes a request midway.
func (w *Watcher) Snapshot() *Snapshot { return w.current.Load() }

// IdentityWatcher serves the identity compiled from an identity file and
// reloads it the way Watcher reloads the config (GW-17, ADR-0017).
type IdentityWatcher struct{ watcher[Identity] }

// NewIdentityWatcher loads the file once; an invalid file is an error.
func NewIdentityWatcher(path string, log *slog.Logger) (*IdentityWatcher, error) {
	w := &IdentityWatcher{watcher[Identity]{path: path, name: "identity", log: log}}
	w.compile = func(data []byte) (*Identity, error) {
		c, err := LoadIdentity(data)
		if err != nil {
			return nil, err
		}
		id, err := CompileIdentity(c)
		if err != nil {
			return nil, fmt.Errorf("identity: %w", err)
		}
		return id, nil
	}
	w.describe = func(id *Identity) []any { return []any{"version", id.Version, "api_keys", len(id.keys)} }
	if err := w.reload(); err != nil {
		return nil, err
	}
	return w, nil
}

// Identity returns the current identity; like a snapshot, a request uses the
// one it took to the end.
func (w *IdentityWatcher) Identity() *Identity { return w.current.Load() }

// watcher polls a file and serves what it compiles to.
type watcher[T any] struct {
	path     string
	name     string // "config" or "identity", for errors and logs
	log      *slog.Logger
	compile  func([]byte) (*T, error)
	describe func(*T) []any // log attributes, version first
	current  atomic.Pointer[T]
	sum      [sha256.Size]byte // content last tried, good or bad
}

// Run re-reads the file every interval until ctx ends. A file that fails to
// load or compile leaves the current value in place.
func (w *watcher[T]) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.reload(); err != nil {
				attrs := append([]any{"path", w.path}, w.describe(w.current.Load())...)
				w.log.Error(w.name+" reload failed; keeping the current "+w.name, append(attrs, "err", err)...)
			}
		}
	}
}

// reload swaps in a new value if the file changed. The same bad content, or
// the same unreadable file (hashed as empty), is reported once, not on every
// poll.
func (w *watcher[T]) reload() error {
	data, err := os.ReadFile(w.path)
	sum := sha256.Sum256(data)
	if w.current.Load() != nil && sum == w.sum {
		return nil
	}
	w.sum = sum
	if err != nil {
		return fmt.Errorf("%s: %w", w.name, err)
	}
	v, err := w.compile(data)
	if err != nil {
		return err
	}
	w.current.Store(v)
	w.log.Info(w.name+" loaded", append([]any{"path", w.path}, w.describe(v)...)...)
	return nil
}

// warnClientChosenKeys logs, once per snapshot load, each Redis policy whose
// keys come from a client-chosen header: a client can send distinct values
// until a noeviction Redis node is full, after which the policy follows its
// failure mode. Such limits should be local, where the key cap bounds them,
// or keyed by a header the client cannot vary freely (D2, ADR-0030).
func warnClientChosenKeys(s *Snapshot, log *slog.Logger) {
	for i := range s.Policies {
		if p := &s.Policies[i]; !p.Local && p.Mode != Off && p.Key.ClientChosen {
			log.Warn("policy keys come from a client-chosen header; distinct values can fill a noeviction Redis node, after which the policy follows its failure mode",
				"policy", p.Name)
		}
	}
}
