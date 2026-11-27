//go:build demo

package demo

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestScenarioD1 runs make demo-d1 on the running demo (SC-02, ADR-0096
// §1, §2): the base-load service stops, D1 runs in a one-off container
// (60 s, base load plus free-co-mobile at 3× and pro-co-mobile at half of
// its plan on orders) with a line a second and a summary per tenant, then
// the service runs again. free-co-mobile is held near its 10/s with 429s
// and RateLimit fields, pro-co-mobile gets no 429, the base consumers no
// 429 either, and the output names keys by ID only: no key, token or
// secret. Counts allow -10% and +20% of the limit, above a host stall's
// effect (ADR-0098's stall table for d1.3), and no rule counts 503s, which
// a stall can give fail_closed payments (ADR-0101 §11).
func TestScenarioD1(t *testing.T) {
	out := runMake(t, "demo-d1")
	noLeak(t, "make demo-d1", out)
	if strings.Contains(out, "spw_") {
		t.Error("make demo-d1 printed an API key")
	}
	for _, want := range []string{
		"keys: free-co-web key #", "free-co-mobile orders: 2xx", `RateLimit-Policy: "consumer-plan";q=10;w=1`,
		`RateLimit-Policy: "consumer-plan";q=100;w=1`, "Retry-After: 1", "summary (d1, ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("make demo-d1 printed no %q:\n%s", want, out)
		}
	}
	// The summary's rows: answers, allowed, denied, 403/401, 503, other.
	row := func(name string) []int {
		m := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)\s`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no summary row for %s:\n%s", name, out)
		}
		var n []int
		for _, s := range m[1:] {
			v, _ := strconv.Atoi(s)
			n = append(n, v)
		}
		return n
	}
	free := row("free-co-mobile orders")
	t.Logf("free-co-mobile orders: %d answers, %d allowed, %d denied", free[0], free[1], free[2])
	if free[0] < 1700 || free[1] < int(0.9*10*60) || free[1] > int(1.2*(20+10*60)) || free[2] == 0 || free[0] != free[1]+free[2] {
		t.Errorf("free-co-mobile orders: %v, want 1,800 answers, about 620 allowed (10/s and its burst of 20), the rest 429", free)
	}
	pro := row("pro-co-mobile orders")
	if pro[0] < 2950 || pro[1] != pro[0] {
		t.Errorf("pro-co-mobile orders: %v, want 3,000 answers, all allowed", pro)
	}
	for _, c := range []string{"free-co-web", "pro-co-web", "enterprise-co-web"} {
		for _, r := range []string{"orders", "catalog", "payments"} {
			if n := row(c + " " + r); n[2] != 0 || n[1] == 0 {
				t.Errorf("%s %s: %v, want answers allowed and none denied", c, r, n)
			}
		}
	}
	// The base load runs again.
	ps, err := exec.CommandContext(t.Context(), "docker", "ps", "--filter", "label=com.docker.compose.service=demo-traffic",
		"--filter", "label=com.docker.compose.project=spillway", "--filter", "label=com.docker.compose.oneoff=False", "--format", "{{.ID}} {{.Status}}").CombinedOutput()
	id, status, _ := strings.Cut(strings.TrimSpace(string(ps)), " ")
	if err != nil || !strings.Contains(status, "(healthy)") {
		t.Fatalf("the demo-traffic service after make demo-d1: %q %v", ps, err)
	}
	logs, _ := exec.CommandContext(t.Context(), "docker", "logs", id).CombinedOutput() //nolint:gosec // G204: a container ID from docker ps
	noLeak(t, "the demo-traffic service's log", string(logs))
	if strings.Contains(string(logs), "spw_") {
		t.Error("the demo-traffic service logged an API key")
	}
}
