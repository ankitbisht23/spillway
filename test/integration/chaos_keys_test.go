//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

// TestChaosKeys runs a small NFR-10 key-count sweep ("chaos keys", ADR-0074
// §D9) in the compose network: every call creates its key, Σ Δ DBSIZE of
// the primaries matches the keys created, both kinds are measured, and the
// test's keys are gone afterwards. The 200 B rule is judged at 1M keys by
// make nfr-keys; at 40,000 keys fixed overheads weigh more, so this test
// allows 1,000 B.
func TestChaosKeys(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "docker", "compose", "run", "--rm", "--build", "--quiet-build", "-T", "chaos", //nolint:gosec // G204: fixed arguments
		"keys", "-steps", "20000,40000", "-max-bytes", "1000")
	cmd.Dir = "../.."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("chaos keys: %v\n%s\n%s", err, out, stderr.String())
	}
	var rep struct {
		Pass   bool `json:"pass"`
		Sweeps []struct {
			Kind    string `json:"kind"`
			Removed int64  `json:"removed"`
			Steps   []struct {
				Created     int64 `json:"created"`
				DBSizeDelta int64 `json:"dbsize_delta"`
				OK          bool  `json:"ok"`
			} `json:"steps"`
		} `json:"sweeps"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	if !rep.Pass || len(rep.Sweeps) != 2 {
		t.Fatalf("report: %s", out)
	}
	for _, s := range rep.Sweeps {
		last := s.Steps[len(s.Steps)-1]
		if !last.OK || last.Created != 40000 || s.Removed != 40000 {
			t.Errorf("%s: last step %+v, removed %d; want 40000 created and removed", s.Kind, last, s.Removed)
		}
	}
}
