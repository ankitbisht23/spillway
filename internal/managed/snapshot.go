package managed

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// The on-disk last good snapshot (CP-07, D9, ADR-0038).

// DefaultSnapshotPath is where a managed gateway keeps its snapshot: a
// writable volume under the read-only root filesystem (SEC-10).
const DefaultSnapshotPath = "/var/lib/spillway/snapshot.pb"

// ReadSnapshot reads the snapshot file at path. A file of another schema
// major, or without both sections, is an error: it is never applied
// (CP-18).
func ReadSnapshot(path string) (*configpb.SnapshotFile, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file
	if err != nil {
		return nil, err
	}
	f := &configpb.SnapshotFile{}
	if err := proto.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("snapshot %s: not a snapshot file: %w", path, err)
	}
	if v := f.GetSchemaVersion(); v.GetMajor() != config.SchemaMajor {
		return nil, fmt.Errorf("snapshot %s: schema %d.%d; this gateway reads schema major %d only", path, v.GetMajor(), v.GetMinor(), config.SchemaMajor)
	}
	if f.GetConfig() == nil || f.GetIdentity() == nil {
		return nil, fmt.Errorf("snapshot %s: lacks its config or identity", path)
	}
	return f, nil
}

// WriteSnapshot writes f to path so that a crash at any moment leaves the
// old file or the new one, never a mix: a temporary file in the same
// directory, fsync, rename over path, fsync of the directory (D9).
func WriteSnapshot(path string, f *configpb.SnapshotFile) (err error) {
	b, err := proto.Marshal(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir) //nolint:gosec // G304: the snapshot's own directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// writer writes the newest snapshot in the background after each apply;
// applies that come while it writes are coalesced into one more write.
// A failed write is logged at most once a minute, and serving goes on.
type writer struct {
	path  string
	state func() *configpb.SnapshotFile // nil until both sections are applied
	log   *slog.Logger
	kick  chan struct{}
	done  chan struct{} // closed when run returns
}

func newWriter(path string, state func() *configpb.SnapshotFile, log *slog.Logger) *writer {
	return &writer{path: path, state: state, log: log, kick: make(chan struct{}, 1), done: make(chan struct{})}
}

// due asks for a write of the current state.
func (w *writer) due() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// writeErrorEvery bounds how often a failing write is logged.
const writeErrorEvery = time.Minute

func (w *writer) run(ctx context.Context) {
	defer close(w.done)
	var lastErr time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		}
		f := w.state()
		if f == nil {
			continue
		}
		if err := WriteSnapshot(w.path, f); err != nil {
			if time.Since(lastErr) >= writeErrorEvery {
				w.log.Error("writing the on-disk snapshot failed; serving goes on", "path", w.path, "err", err)
				lastErr = time.Now()
			}
			continue
		}
		w.log.Debug("on-disk snapshot written", "path", w.path, "config_version", f.GetConfig().GetVersion(), "identity_version", f.GetIdentity().GetVersion())
	}
}
