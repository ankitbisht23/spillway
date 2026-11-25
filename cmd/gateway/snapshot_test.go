package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
	"github.com/ankitbisht23/spillway/internal/managed"
)

// contentMarker stands for the API key hashes inside a snapshot: neither helper may
// print it.
const contentMarker = "content-marker-in-identity"

func writeTestSnapshot(t *testing.T, path string, major int32) {
	t.Helper()
	f := &configpb.SnapshotFile{
		SchemaVersion: &configpb.SchemaVersion{Major: major, Minor: config.SchemaMinor},
		Config:        &configpb.Config{Version: 7},
		Identity:      &configpb.Identity{Version: 4, KeyFormat: contentMarker},
	}
	if err := managed.WriteSnapshot(path, f); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotSeed: the seed copies a valid snapshot into an empty volume,
// does nothing when the target exists or there is no seed, refuses a file
// that is not a snapshot or of another schema major, and never prints the
// content (CP-07, ADR-0075 §D11).
func TestSnapshotSeed(t *testing.T) {
	dir := t.TempDir()
	seed, to := filepath.Join(dir, "seed.pb"), filepath.Join(dir, "state", "snapshot.pb")
	if err := os.Mkdir(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestSnapshot(t, seed, config.SchemaMajor)
	var stderr bytes.Buffer
	if code := snapshotSeed([]string{"--from", seed, "--to", to}, &stderr); code != 0 {
		t.Fatalf("seed: exit %d: %s", code, stderr.String())
	}
	f, err := managed.ReadSnapshot(to)
	if err != nil || f.GetConfig().GetVersion() != 7 || f.GetIdentity().GetVersion() != 4 {
		t.Fatalf("seeded snapshot: %v, %v", f, err)
	}
	if !strings.Contains(stderr.String(), "config version 7, identity version 4") || strings.Contains(stderr.String(), contentMarker) {
		t.Errorf("seed said %q", stderr.String())
	}

	// The target exists: untouched, even by a newer seed.
	before, _ := os.ReadFile(to) //nolint:gosec // G304: the test's own temporary file
	writeTestSnapshot(t, seed, config.SchemaMajor)
	if err := os.WriteFile(seed, []byte("not a snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := snapshotSeed([]string{"--from", seed, "--to", to}, &stderr); code != 0 || !strings.Contains(stderr.String(), "exists; nothing to do") {
		t.Errorf("existing target: exit %d: %s", code, stderr.String())
	}
	if after, _ := os.ReadFile(to); !bytes.Equal(before, after) { //nolint:gosec // G304: as above
		t.Error("an existing snapshot was overwritten")
	}

	for name, tc := range map[string]struct {
		prepare func(path string)
		code    int
		said    string
	}{
		"no seed":         {func(string) {}, 0, "nothing to do"},
		"not a snapshot":  {func(p string) { _ = os.WriteFile(p, []byte("not a snapshot"), 0o600) }, 1, "not a snapshot file"},
		"another major":   {func(p string) { writeTestSnapshot(t, p, config.SchemaMajor+1) }, 1, "reads schema major"},
		"without a major": {func(p string) { writeTestSnapshot(t, p, 0) }, 1, "reads schema major"},
	} {
		d := t.TempDir()
		from, target := filepath.Join(d, "seed.pb"), filepath.Join(d, "snapshot.pb")
		tc.prepare(from)
		stderr.Reset()
		if code := snapshotSeed([]string{"--from", from, "--to", target}, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.said) {
			t.Errorf("%s: exit %d: %s", name, code, stderr.String())
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("%s: a snapshot was written", name)
		}
	}
	for _, args := range [][]string{nil, {"--from", seed}, {"--to", to}, {"--from", seed, "--to", to, "extra"}, {"--nope"}} {
		if code := snapshotSeed(args, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// TestSnapshotExport: the export writes the snapshot's bytes to stdout,
// from SPILLWAY_GATEWAY_SNAPSHOT by default, and nothing on a bad file.
func TestSnapshotExport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.pb")
	writeTestSnapshot(t, path, config.SchemaMajor)
	getenv := func(k string) string {
		if k == "SPILLWAY_GATEWAY_SNAPSHOT" {
			return path
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	if code := snapshotExport(nil, getenv, &stdout, &stderr); code != 0 {
		t.Fatalf("export: exit %d: %s", code, stderr.String())
	}
	f := &configpb.SnapshotFile{}
	if err := proto.Unmarshal(stdout.Bytes(), f); err != nil || f.GetConfig().GetVersion() != 7 || f.GetIdentity().GetKeyFormat() != contentMarker {
		t.Fatalf("exported %v, %v", f, err)
	}
	if strings.Contains(stderr.String(), contentMarker) || !strings.Contains(stderr.String(), "config version 7") {
		t.Errorf("export said %q", stderr.String())
	}
	// The export seeds a new volume.
	out := filepath.Join(dir, "exported.pb")
	if err := os.WriteFile(out, stdout.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "new", "snapshot.pb")
	_ = os.Mkdir(filepath.Dir(target), 0o700)
	if code := snapshotSeed([]string{"--from", out, "--to", target}, &stderr); code != 0 {
		t.Fatalf("seeding from the export: exit %d", code)
	}

	bad := filepath.Join(dir, "bad.pb")
	_ = os.WriteFile(bad, []byte("not a snapshot"), 0o600)
	for _, from := range []string{bad, filepath.Join(dir, "missing.pb")} {
		stdout.Reset()
		if code := snapshotExport([]string{"--from", from}, getenv, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
			t.Errorf("%s: exit %d, %d bytes on stdout", from, code, stdout.Len())
		}
	}
	if code := snapshotExport([]string{"extra"}, getenv, &stdout, &stderr); code != 2 {
		t.Errorf("extra argument: exit %d", code)
	}
}
