// Package release holds static tests of the release preparation (ADR-0076,
// ADR-0092): release.yml's triggers, permissions and gating, the pins of
// every workflow action and third-party image, CHANGELOG.md and its release
// notes, and the chart's version. They run with go test, without Docker or
// network; the release build itself is make release-build.
package release

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// repo is the repository root.
var repo = os.DirFS("../..")

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, name string) map[string]any {
	t.Helper()
	var d map[string]any
	if err := yaml.Unmarshal(read(t, name), &d); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// chartVersions returns Chart.yaml's version and appVersion.
func chartVersions(t *testing.T) (string, string) {
	t.Helper()
	c := decode(t, "deploy/helm/spillway/Chart.yaml")
	v, _ := c["version"].(string)
	a, _ := c["appVersion"].(string)
	return v, a
}

// TestReleaseTriggers: release.yml runs only on a v* tag push or a manual
// dispatch whose dry_run input defaults to true; never on a branch push, a
// pull request or a schedule (ADR-0076).
func TestReleaseTriggers(t *testing.T) {
	w := decode(t, ".github/workflows/release.yml")
	on, _ := w["on"].(map[string]any)
	keys := make([]string, 0, len(on))
	for k := range on {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"push", "workflow_dispatch"}) {
		t.Fatalf("triggers %v, want push (tags) and workflow_dispatch only", keys)
	}
	push, _ := on["push"].(map[string]any)
	if len(push) != 1 || !slices.Equal(get(push, "tags").([]any), []any{"v*"}) {
		t.Errorf("push trigger %v, want tags [v*] only", push)
	}
	in := get(on, "workflow_dispatch", "inputs", "dry_run")
	if get(in, "type") != "boolean" || get(in, "default") != true {
		t.Errorf("dry_run input %v, want a boolean defaulting to true", in)
	}
}

// TestReleasePermissions: the workflow and the build job can only read;
// only the publish job, behind the release environment and only from a v*
// tag that is not a dry run, may write packages and contents and mint an
// OIDC token.
func TestReleasePermissions(t *testing.T) {
	w := decode(t, ".github/workflows/release.yml")
	if p := w["permissions"]; !mapEqual(p, map[string]any{"contents": "read"}) {
		t.Errorf("workflow permissions %v, want contents: read", p)
	}
	jobs, _ := w["jobs"].(map[string]any)
	if len(jobs) != 2 {
		t.Fatalf("jobs %v, want build and publish", jobs)
	}
	if p := get(jobs, "build", "permissions"); p != nil {
		t.Errorf("build job permissions %v, want the workflow's read-only ones", p)
	}
	pub := get(jobs, "publish")
	want := map[string]any{"contents": "write", "packages": "write", "id-token": "write"}
	if p := get(pub, "permissions"); !mapEqual(p, want) {
		t.Errorf("publish permissions %v, want %v", p, want)
	}
	if get(pub, "environment") != "release" || get(pub, "needs") != "build" {
		t.Errorf("publish environment %v, needs %v", get(pub, "environment"), get(pub, "needs"))
	}
	cond, _ := get(pub, "if").(string)
	for _, s := range []string{"github.ref_type == 'tag'", "startsWith(github.ref_name, 'v')", "!(github.event_name == 'workflow_dispatch' && inputs.dry_run)"} {
		if !strings.Contains(cond, s) {
			t.Errorf("publish if %q lacks %q", cond, s)
		}
	}
	// What a dry run does stays in the build job: nothing there logs in,
	// pushes, signs or releases.
	b, _ := yaml.Marshal(get(jobs, "build"))
	for _, s := range []string{"auth login", "--push", "crane", "cosign", "release-publish", "gh release", "id-token"} {
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("the build job mentions %q", s)
		}
	}
}

func mapEqual(v any, want map[string]any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(want) {
		return false
	}
	for k, w := range want {
		if m[k] != w {
			return false
		}
	}
	return true
}

// TestActionsPinned: every action in every workflow is pinned to a full
// commit SHA with its version in a comment.
func TestActionsPinned(t *testing.T) {
	files, err := fs.Glob(repo, ".github/workflows/*.yml")
	if err != nil || len(files) < 3 {
		t.Fatalf("workflows %v %v", files, err)
	}
	uses := regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(.*)$`)
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\d+(\.\d+)*$`)
	n := 0
	for _, f := range files {
		for _, m := range uses.FindAllStringSubmatch(string(read(t, f)), -1) {
			n++
			if !pinned.MatchString(strings.TrimSpace(m[1])) {
				t.Errorf("%s: uses %q is not pinned to a commit SHA with a version comment", f, m[1])
			}
		}
	}
	if n < 20 {
		t.Errorf("found only %d uses lines", n)
	}
}

// TestImagesPinnedByDigest: every third-party image compose, the
// Dockerfiles, deploy/kind and the Makefile run is tag@sha256:<index
// digest> (ADR-0076); only the locally built spillway-* images are not.
func TestImagesPinnedByDigest(t *testing.T) {
	files := []string{"compose.yaml", "Dockerfile", "Makefile"}
	for _, g := range []string{"deploy/compose/*/Dockerfile", "deploy/kind/*.yaml", "deploy/kind/*/*.yaml"} {
		m, err := fs.Glob(repo, g)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	ref := regexp.MustCompile(`(?m)^[ \t]*(?:-[ \t]+)?image:[ \t]*(\S+)|^FROM[ \t]+(?:--platform=\S+[ \t]+)?(\S+)|^\w+_IMAGE[ \t]*:=[ \t]*(\S+)`)
	digest := regexp.MustCompile(`^[\w./-]+:[\w.-]+@sha256:[0-9a-f]{64}$`)
	n := 0
	for _, f := range files {
		for _, m := range ref.FindAllStringSubmatch(string(read(t, f)), -1) {
			img := m[1] + m[2] + m[3]
			switch {
			case strings.HasPrefix(img, "spillway-"), img == "build": // local images; the Dockerfile's own stage
				continue
			case !digest.MatchString(img):
				t.Errorf("%s: image %q is not tag@sha256:<digest>", f, img)
			}
			n++
		}
	}
	if n < 30 {
		t.Errorf("found only %d third-party image references", n)
	}
}

// TestChartVersion: the chart is 1.0.0 with appVersion 1.0.0, and its
// images default to the ghcr names; the template tags them v<appVersion>,
// the tag release.yml pushes (test/kind's TestChartReleaseImages renders it).
func TestChartVersion(t *testing.T) {
	v, a := chartVersions(t)
	if v != "1.0.0" || a != "1.0.0" {
		t.Errorf("Chart.yaml version %q appVersion %q, want 1.0.0 and 1.0.0", v, a)
	}
	vals := decode(t, "deploy/helm/spillway/values.yaml")
	for _, c := range []string{"gateway", "controlplane"} {
		if r := get(vals, c, "image", "repository"); r != "ghcr.io/ankitbisht23/spillway-"+c {
			t.Errorf("%s image repository %v", c, r)
		}
		if tag := get(vals, c, "image", "tag"); tag != "" {
			t.Errorf("%s image tag %q, want empty (v<appVersion>)", c, tag)
		}
	}
}

// TestChangelog: Keep a Changelog layout, an Unreleased section, a section
// for the chart's appVersion with a subsection per milestone and the known
// limitations, and link references for both.
func TestChangelog(t *testing.T) {
	_, app := chartVersions(t)
	s := string(read(t, "CHANGELOG.md"))
	for _, want := range []string{
		"# Changelog\n",
		"[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/)",
		"\n## [Unreleased]\n",
		"\n[Unreleased]: https://github.com/ankitbisht23/spillway/compare/v" + app + "...HEAD\n",
		"\n[" + app + "]: https://github.com/ankitbisht23/spillway/releases/tag/v" + app + "\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("CHANGELOG.md lacks %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^## \[` + regexp.QuoteMeta(app) + `\] - \d{4}-\d{2}-\d{2}$`).MatchString(s) {
		t.Errorf("CHANGELOG.md has no dated section for %s", app)
	}
	notes := releaseNotes(t, app)
	for _, want := range []string{
		"### M0 Foundations", "### M1 Proxy core", "### M2 Local rate limiting", "### M3 Distributed rate limiting",
		"### M4 Resilience", "### M5 Control plane and auth", "### M6 Admin UI", "### M7 Performance and hardening",
		"### Known limitations",
		// The known limits the release must name (the user's decision on
		// NFR-04, 2026-11-27, and the M7 ADRs).
		"NFR-04", "Docker Desktop for Mac", "ADR-0034", "a69b856", "m7-20261126T233441Z",
		"NFR-02", "ADR-0084", "ADR-0089", "ADR-0088", "ADR-0079",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the %s release notes lack %q", app, want)
		}
	}
}

// releaseNotes runs deploy/release/notes.sh, what release.yml publishes.
func releaseNotes(t *testing.T, version string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "../../deploy/release/notes.sh", version).Output() //nolint:gosec // G204: the repository's script
	if err != nil {
		t.Fatalf("notes.sh %s: %v", version, err)
	}
	return string(out)
}

// TestReleaseNotes: the notes are the section's body only (no heading, no
// neighbouring section, no link references, no blank edges), and a version
// with no section fails.
func TestReleaseNotes(t *testing.T) {
	_, app := chartVersions(t)
	notes := releaseNotes(t, app)
	switch {
	case strings.Contains(notes, "## ["), strings.Contains(notes, "]: https://"):
		t.Errorf("notes contain a heading or link reference:\n%s", notes)
	case strings.HasPrefix(notes, "\n"), strings.HasSuffix(notes, "\n\n"), !strings.HasPrefix(notes, "The first release"):
		t.Errorf("notes edges: %q … %q", notes[:min(40, len(notes))], notes[max(0, len(notes)-40):])
	}
	if err := exec.CommandContext(t.Context(), "../../deploy/release/notes.sh", "0.0.0-none").Run(); err == nil { //nolint:gosec // G204: the repository's script
		t.Error("notes.sh succeeded for a version CHANGELOG.md lacks")
	}
}
