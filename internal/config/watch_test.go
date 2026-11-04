package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func configYAML(version int, path string) string {
	return upstream + "version: " + strconv.Itoa(version) + "\nroutes: [{name: r, upstream: u, match: {path: " + path + "}}]\n"
}

func newTestWatcher(t *testing.T, content string) (*Watcher, string, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, content)
	var logs bytes.Buffer
	w, err := NewWatcher(path, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return w, path, &logs
}

// write replaces the file atomically, as editors and ConfigMap updates do.
func write(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestNewWatcherRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, "routes: [")
	if _, err := NewWatcher(path, slog.New(slog.DiscardHandler), nil); err == nil {
		t.Error("NewWatcher accepted an invalid file")
	}
	if _, err := NewWatcher(filepath.Join(t.TempDir(), "missing.yaml"), slog.New(slog.DiscardHandler), nil); err == nil {
		t.Error("NewWatcher accepted a missing file")
	}
}

// TestReloadKeepsInFlightSnapshot: a request that took the old snapshot keeps
// matching on it after a swap (GW-21).
func TestReloadKeepsInFlightSnapshot(t *testing.T) {
	w, path, _ := newTestWatcher(t, configYAML(1, "/old"))
	inFlight := w.Snapshot()

	write(t, path, configYAML(2, "/new"))
	if err := w.reload(); err != nil {
		t.Fatal(err)
	}
	if w.Snapshot().Version != 2 {
		t.Fatalf("version after reload = %d, want 2", w.Snapshot().Version)
	}
	if _, _, ok := inFlight.Match("", "GET", "/old", nil); !ok || inFlight.Version != 1 {
		t.Error("the in-flight snapshot changed under the request")
	}
	if _, _, ok := w.Snapshot().Match("", "GET", "/new", nil); !ok {
		t.Error("the new snapshot does not route /new")
	}
}

func TestReloadKeepsLastGoodSnapshot(t *testing.T) {
	w, path, _ := newTestWatcher(t, configYAML(1, "/a"))
	good := w.Snapshot()

	for name, content := range map[string]string{
		"invalid YAML":   "routes: [",
		"invalid config": upstream + "routes: [{name: r, upstream: missing, match: {path: /}}]",
	} {
		write(t, path, content)
		if err := w.reload(); err == nil {
			t.Errorf("%s: reload succeeded", name)
		}
		if w.Snapshot() != good {
			t.Errorf("%s: snapshot replaced", name)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := w.reload(); err == nil || w.Snapshot() != good {
		t.Errorf("missing file: err = %v, snapshot replaced = %v", err, w.Snapshot() != good)
	}
}

func TestReloadSkipsUnchangedContent(t *testing.T) {
	w, path, _ := newTestWatcher(t, configYAML(1, "/a"))
	s := w.Snapshot()
	write(t, path, configYAML(1, "/a"))
	if err := w.reload(); err != nil || w.Snapshot() != s {
		t.Errorf("same content: err = %v, recompiled = %v", err, w.Snapshot() != s)
	}

	// The same bad content is reported once, not on every poll.
	write(t, path, "routes: [")
	if err := w.reload(); err == nil {
		t.Fatal("bad content: no error")
	}
	if err := w.reload(); err != nil {
		t.Errorf("same bad content reported again: %v", err)
	}

	// So is a missing file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := w.reload(); err == nil {
		t.Fatal("missing file: no error")
	}
	if err := w.reload(); err != nil {
		t.Errorf("missing file reported again: %v", err)
	}
}

func TestRunPollsTheFile(t *testing.T) {
	w, path, logs := newTestWatcher(t, configYAML(1, "/a"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	write(t, path, "routes: [")
	write(t, path, configYAML(2, "/b"))
	deadline := time.Now().Add(5 * time.Second)
	for w.Snapshot().Version != 2 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not pick up the new file")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), "version=2") {
		t.Errorf("no load logged:\n%s", logs.String())
	}
}

func TestApplyHookCanRejectSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, configYAML(1, "/a"))
	var applied []int64
	w, err := NewWatcher(path, slog.New(slog.DiscardHandler), func(s *Snapshot) error {
		applied = append(applied, s.Version)
		if s.Version == 2 {
			return errors.New("rejected")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, configYAML(2, "/b"))
	if err := w.reload(); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("reload error = %v, want the hook's error", err)
	}
	if w.Snapshot().Version != 1 {
		t.Errorf("version = %d, want 1 kept", w.Snapshot().Version)
	}
	write(t, path, configYAML(3, "/c"))
	if err := w.reload(); err != nil || w.Snapshot().Version != 3 {
		t.Errorf("reload = %v, version %d; want 3", err, w.Snapshot().Version)
	}
	if fmt.Sprint(applied) != "[1 2 3]" {
		t.Errorf("hook saw versions %v, want [1 2 3]", applied)
	}
}
