// Package load holds tests of the load scripts' jq rules; the scripts
// themselves run in bench.yml and on a laptop.
package load

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// jqProbe runs filter, with scaling.jq included, on a probe whose vCPU
// readings are gw, lg and up, and returns jq's raw output.
func jqProbe(t *testing.T, filter string, gw, lg, up float64) string {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not installed")
	}
	probe := fmt.Sprintf(`{"vcpu": {"gateway": %g, "loadgen": %g, "upstream": %g}}`, gw, lg, up)
	cmd := exec.CommandContext(t.Context(), jq, "-r", "-L", ".", `include "scaling"; `+filter) //nolint:gosec // G204: the test's own filter
	cmd.Stdin = strings.NewReader(probe)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jq: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGatewayBound pins NFR-02's probe rule as ADR-0084 sets it: the gateway
// at >= 0.95 × n vCPU, loadgen and the upstream together at <= 1.80 vCPU.
func TestGatewayBound(t *testing.T) {
	for _, c := range []struct {
		name       string
		n          int
		gw, lg, up float64
		want       string
	}{
		{"n=1 at the limits", 1, 0.95, 0.90, 0.90, "true"},
		{"n=1 gateway just under", 1, 0.9499, 0.60, 0.30, "false"},
		{"n=2 at the limits", 2, 1.90, 1.20, 0.60, "true"},
		{"n=2 gateway under", 2, 1.89, 0.80, 0.40, "false"},
		{"n=2 load side just over", 2, 1.95, 1.21, 0.60, "false"},
		// One container above the old 0.90 per-container limit is fine
		// now that they share two CPUs.
		{"n=2 loadgen alone above 0.90", 2, 1.92, 1.10, 0.50, "true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := jqProbe(t, fmt.Sprintf("gateway_bound(%d; 0.95; 1.80)", c.n), c.gw, c.lg, c.up); got != c.want {
				t.Errorf("gateway_bound = %s, want %s", got, c.want)
			}
		})
	}
}

func TestNotBoundReason(t *testing.T) {
	got := jqProbe(t, "not_bound_reason(2; 3; 0.95; 1.80)", 1.7012, 1.2, 0.71)
	want := "n=2 probe 3: not gateway-bound (vCPU gateway 1.7 < 1.9 or loadgen + upstream 1.91 > 1.8)"
	if got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}
