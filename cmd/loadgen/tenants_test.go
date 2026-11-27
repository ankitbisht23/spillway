package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTenant(t *testing.T) {
	got, err := parseTenant("name=A,rate=2000,key-file=/keys/a,toggle")
	if err != nil || got != (tenantSpec{name: "A", rate: 2000, keyFile: "/keys/a", toggle: true}) {
		t.Errorf("parseTenant = %+v, %v", got, err)
	}
	for _, bad := range []string{
		"", "name=A,rate=1", "name=A,key-file=/k", "rate=1,key-file=/k", "name=,rate=1,key-file=/k",
		"name=A B,rate=1,key-file=/k", "name=A,rate=0,key-file=/k", "name=A,rate=-1,key-file=/k",
		"name=A,rate=x,key-file=/k", "name=A,rate=1,key-file=", "name=A,rate=1,key-file=/k,toggle=yes",
		"name=A,rate=1,key-file=/k,colour=red",
	} {
		if _, err := parseTenant(bad); err == nil {
			t.Errorf("parseTenant(%q): no error", bad)
		}
	}
	var f tenantFlags
	if err := f.Set("name=A,rate=1,key-file=/k"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("name=A,rate=2,key-file=/j"); err == nil {
		t.Error("a repeated tenant name: no error")
	}
}

func TestTenantFlags(t *testing.T) {
	ok := []string{"-url", "http://gateway-1:8000/", "-duration", "1m", "-tenant", "name=A,rate=1,key-file=/k"}
	o, err := parseFlags(ok, io.Discard)
	if err != nil || len(o.tenants) != 1 || o.slot != 10*time.Second {
		t.Fatalf("options %+v, %v", o, err)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{append(ok, "-rate", "5"), "-rate"},
		{append(ok, "-slot", "1s"), "-slot"},
		{append(ok, "-slot", "2m"), "-slot"},
		{append(ok, "-tenant", "name=A,rate=2,key-file=/j"), "twice"},
	} {
		if _, err := parseFlags(tt.args, io.Discard); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error %v, want one about %s", tt.args, err, tt.want)
		}
	}
}

// writeKey writes a key file into dir.
func writeKey(t *testing.T, dir, name, key string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil { //nolint:gosec // G703: the test's own temp directory
		t.Fatal(err)
	}
	return path
}

// TestTenantRun drives a server with three tenants through run: each sends
// its own key at its own rate, the noisy one toggled; the JSON has every
// tenant with its slots, the top level adds them up, and no key appears in
// stdout or stderr. A tenant whose key the server rejects gets 4xx answers
// there, not an invalid run.
func TestTenantRun(t *testing.T) {
	keys := map[string]string{"A": "key-of-tenant-a-0123456789", "B": "key-of-tenant-b-0123456789", "C": "key-of-tenant-c-0123456789"}
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get(apiKeyHeader)
		mu.Lock()
		seen[k]++
		mu.Unlock()
		if k == keys["C"] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	args := []string{"-url", srv.URL + "/bytes/1024", "-warmup", "0s", "-duration", "4s", "-slot", "2s",
		"-tenant", "name=A,rate=200,key-file=" + writeKey(t, dir, "a", keys["A"]) + ",toggle",
		"-tenant", "name=B,rate=50,key-file=" + writeKey(t, dir, "b", keys["B"]),
		"-tenant", "name=C,rate=20,key-file=" + writeKey(t, dir, "c", keys["C"]),
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), args, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, k := range keys {
		if strings.Contains(stdout.String(), k) || strings.Contains(stderr.String(), k) {
			t.Fatalf("a key leaked into the output")
		}
	}
	var r result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	a, b, c := r.Tenants["A"], r.Tenants["B"], r.Tenants["C"]
	if len(r.Tenants) != 3 || !a.Toggle || b.Toggle || !a.Valid || !b.Valid || !c.Valid || !r.Valid {
		t.Fatalf("tenants = %+v", r.Tenants)
	}
	// A sends in slot 1 only (2 s at 200 rps); B and C all the time.
	if a.Requests.Scheduled != 400 || b.Requests.Scheduled != 200 || c.Requests.Scheduled != 80 {
		t.Errorf("scheduled A %d, B %d, C %d; want 400, 200, 80", a.Requests.Scheduled, b.Requests.Scheduled, c.Requests.Scheduled)
	}
	if a.Status["2xx"] != 400 || b.Status["2xx"] != 200 || c.Status["4xx"] != 80 {
		t.Errorf("status A %v, B %v, C %v", a.Status, b.Status, c.Status)
	}
	if len(a.Slots) != 2 || a.Slots[0].On || !a.Slots[1].On || a.Slots[0].Requests != 0 || a.Slots[1].Requests != 200 {
		t.Errorf("A's slots = %+v, want off then on with the 200 settled requests", a.Slots)
	}
	if len(b.Slots) != 2 || !b.Slots[0].On || b.Slots[0].Requests != 50 || b.Slots[1].P99 <= 0 {
		t.Errorf("B's slots = %+v", b.Slots)
	}
	if b.PooledP99 == nil || b.PooledP99.Even <= 0 || b.PooledP99.Odd <= 0 || a.PooledP99 == nil || a.PooledP99.Even != 0 || a.PooledP99.Odd <= 0 {
		t.Errorf("pooled p99s: A %+v, B %+v; want both of B's and only A's odd slots measured", a.PooledP99, b.PooledP99)
	}
	if r.TargetRPS != 270 || r.Requests.Sent != 680 || r.Status["2xx"] != 600 || r.Status["4xx"] != 80 || r.LatencyUS.P99 <= 0 {
		t.Errorf("top level: target %d, requests %+v, status %v, p99 %d", r.TargetRPS, r.Requests, r.Status, r.LatencyUS.P99)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[keys["A"]] != 400 || seen[keys["B"]] != 200 || seen[keys["C"]] != 80 || seen[""] != 0 {
		t.Errorf("server saw %d, %d, %d requests with the keys and %d without", seen[keys["A"]], seen[keys["B"]], seen[keys["C"]], seen[""])
	}
	var out map[string]any
	_ = json.Unmarshal(stdout.Bytes(), &out)
	tenant := out["tenants"].(map[string]any)["A"].(map[string]any)
	for _, f := range []string{"target_rps", "toggle", "achieved_rps", "valid", "requests", "status", "errors", "latency_us", "pooled_p99", "slots"} {
		if _, ok := tenant[f]; !ok {
			t.Errorf("tenant JSON lacks %q", f)
		}
	}
}

// TestKeyFileErrors: a bad key file is a flag error that names the file
// but never prints its content.
func TestKeyFileErrors(t *testing.T) {
	dir := t.TempDir()
	const secret = "two-line-secret-material"
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte(secret+"\n"+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := writeKey(t, dir, "empty", "  ")
	for _, f := range []string{bad, empty, filepath.Join(dir, "missing")} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"-url", "http://127.0.0.1:1/", "-duration", "2s", "-slot", "2s", "-tenant", "name=A,rate=1,key-file=" + f}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), f) {
			t.Errorf("%s: exit %d, stderr %q; want 2 naming the file", f, code, stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Errorf("%s: the key file's content was printed", f)
		}
	}
}

func TestServe(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, 1024) }()

	resp, err := http.Post("http://"+addr+"/any/path", "text/plain", strings.NewReader("ignored")) //nolint:noctx // a test of a local server
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) != 1024 || resp.Header.Get("Content-Length") != "1024" {
		t.Errorf("status %d, %d bytes, Content-Length %q; want 200 with 1024", resp.StatusCode, len(body), resp.Header.Get("Content-Length"))
	}
	var stderr bytes.Buffer
	if code := runServe(t.Context(), []string{"-addr", addr, "-healthcheck"}, &stderr); code != 0 {
		t.Errorf("healthcheck: exit %d: %s", code, stderr.String())
	}
	if code := runServe(t.Context(), []string{"-addr", addr, "-bytes", "10", "-healthcheck"}, io.Discard); code != 1 {
		t.Errorf("healthcheck expecting another size: exit %d, want 1", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
	if code := runServe(t.Context(), []string{"-addr", addr, "-healthcheck"}, io.Discard); code != 1 {
		t.Errorf("healthcheck of a stopped server: exit %d, want 1", code)
	}
	for _, args := range [][]string{{"extra"}, {"-bytes", "-1"}, {"-nope"}} {
		if code := runServe(t.Context(), args, io.Discard); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}
