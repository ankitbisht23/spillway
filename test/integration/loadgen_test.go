//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

// TestLoadgen runs the loadgen compose service inside the compose network
// against gateway-1's plain-HTTP and TLS listeners (ADR-0004, ADR-0014):
// each run is valid, every request gets a 2xx, over HTTP/1.1 and HTTP/2.
func TestLoadgen(t *testing.T) {
	for _, tt := range []struct {
		name, proto string
		args        []string
	}{
		{"http", "HTTP/1.1", []string{"-url", "http://gateway-1:8000/bytes/1024"}},
		{"https", "HTTP/2.0", []string{"-url", "https://gateway-1:8443/bytes/1024", "-cacert", "/etc/spillway-certs/gateway.pem"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"compose", "run", "--rm", "--build", "--quiet-build", "-T", "loadgen",
				"-rate", "200", "-warmup", "1s", "-duration", "3s"}, tt.args...)
			cmd := exec.CommandContext(t.Context(), "docker", args...) //nolint:gosec // G204: fixed arguments from the table above
			// From the repository root, where compose.yaml is.
			cmd.Dir = "../.."
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("docker compose run loadgen: %v\n%s\n%s", err, out, stderr.String())
			}
			var r struct {
				Protocol string             `json:"protocol"`
				Valid    bool               `json:"valid"`
				Requests struct{ Sent int } `json:"requests"`
				Status   map[string]int     `json:"status"`
			}
			if err := json.Unmarshal(out, &r); err != nil {
				t.Fatalf("loadgen result: %v\n%s", err, out)
			}
			if !r.Valid || r.Protocol != tt.proto || r.Requests.Sent != 600 || r.Status["2xx"] != 600 {
				t.Errorf("result: %s\nwant valid, %s, 600 requests sent and answered 2xx", out, tt.proto)
			}
		})
	}
}
