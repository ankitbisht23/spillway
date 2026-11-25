//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestGatewayPerIPCap opens 1,100 idle connections to gateway-1's traffic
// port from the host, which all reach it from Docker's one forwarding
// address, against its default per-address cap of 1,024 (GW-26, ADR-0071).
// At least 76 waiting connections are closed for newer ones, as
// gw_connections_capped_total{cap="per_ip",action="closed_waiting"} counts,
// none is shed, and a client in the compose network, another address, still
// gets every answer 2xx. Every wait allows 30 s, far above the host stalls
// of ADR-0034; nothing here is timed. The 1,100 dials take well under a
// second, so none of them reaches the gateway's 10 s header timeout
// (GW-28) before the cap is reached.
func TestGatewayPerIPCap(t *testing.T) {
	const (
		open     = 1100
		perIPCap = 1024 // compose's SPILLWAY_GATEWAY_MAX_CONNECTIONS_PER_IP default
	)
	closedSeries := `gw_connections_capped_total{action="closed_waiting",cap="per_ip"}`
	shedSeries := `gw_connections_capped_total{action="shed",cap="per_ip"}`
	metric := func(series string) float64 {
		_, body := get(t, "http://127.0.0.1:8001/metrics")
		for _, line := range strings.Split(body, "\n") {
			if v, ok := strings.CutPrefix(line, series+" "); ok {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					t.Fatal(err)
				}
				return f
			}
		}
		t.Fatalf("gateway-1's /metrics has no %s", series)
		return 0
	}
	// Docker's port forwarding can lag a gateway just recreated (make
	// dev): its first connections are reset or closed. Wait for three
	// answers in a row first.
	for ok, deadline := 0, time.Now().Add(30*time.Second); ok < 3; time.Sleep(100 * time.Millisecond) {
		if answered(t, "http://127.0.0.1:8000/status/200") {
			ok++
		} else {
			ok = 0
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway-1's traffic port does not answer 200 from the host")
		}
	}
	closedBefore, shedBefore := metric(closedSeries), metric(shedSeries)

	// One dial at a time: Docker Desktop's port forwarding reset some of
	// 20 concurrent dials.
	conns := make([]net.Conn, 0, open)
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	for range open {
		c, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(t.Context(), "tcp", "127.0.0.1:8000")
		if err != nil {
			t.Fatalf("dial %d: %v", len(conns)+1, err)
		}
		conns = append(conns, c)
	}

	var closed float64
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if closed = metric(closedSeries) - closedBefore; closed >= open-perIPCap {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v waiting connections closed at the per-address cap, want at least %d", closed, open-perIPCap)
		}
	}
	if shed := metric(shedSeries) - shedBefore; shed != 0 {
		t.Errorf("%v connections shed at the per-address cap, want 0 (every one was waiting)", shed)
	}

	// The connections closed by the gateway see EOF. This read takes up
	// to 20 ms per open connection, so connections still waiting reach
	// the gateway's 10 s header timeout meanwhile (GW-28) and close too.
	gone := 0
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			gone++
		}
	}
	t.Logf("%d connections opened, %v closed at the cap (gateway-1's metric), %d closed as the client sees it (the cap's and the header timeout's)", open, closed, gone)
	if gone < open-perIPCap {
		t.Errorf("%d connections closed as the client sees it, want at least %d", gone, open-perIPCap)
	}

	// Another address, with the host's connections still open: loadgen in
	// the compose network gets every request answered 2xx.
	cmd := exec.CommandContext(t.Context(), "docker", "compose", "run", "--rm", "--build", "--quiet-build", "-T", "loadgen", //nolint:gosec // G204: fixed arguments
		"-rate", "100", "-warmup", "1s", "-duration", "3s", "-url", "http://gateway-1:8000/status/200")
	cmd.Dir = "../.."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose run loadgen: %v\n%s\n%s", err, out, stderr.String())
	}
	var r struct {
		Valid    bool               `json:"valid"`
		Requests struct{ Sent int } `json:"requests"`
		Status   map[string]int     `json:"status"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("loadgen result: %v\n%s", err, out)
	}
	if !r.Valid || r.Requests.Sent == 0 || r.Status["2xx"] != r.Requests.Sent {
		t.Errorf("loadgen from another address: %s\nwant valid and every request sent answered 2xx", out)
	}
}

// answered reports whether a GET of target got 200; a failed request is
// false, not an error.
func answered(t *testing.T, target string) bool {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
