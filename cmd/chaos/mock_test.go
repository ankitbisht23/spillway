package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// startMock starts a mock on a free port; it returns its traffic URL and
// its control server.
func startMock(t *testing.T) (*mock, string, *httptest.Server) {
	t.Helper()
	m := &mock{addr: "127.0.0.1:0"}
	if err := m.listen(); err != nil {
		t.Fatal(err)
	}
	ctl := httptest.NewServer(m.controlHandler())
	t.Cleanup(func() { ctl.Close(); m.close() })
	return m, "http://" + m.addr + "/bytes/1024", ctl
}

func get(t *testing.T, c *http.Client, url string) (int, int, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, len(b), err
}

// TestMockModes: healthy, 5xx, refused (new and kept-alive connections),
// and healthy again on the same address (ADR-0031).
func TestMockModes(t *testing.T) {
	_, url, ctl := startMock(t)
	c := &http.Client{}
	if code, n, err := get(t, c, url); err != nil || code != 200 || n != 1024 {
		t.Fatalf("healthy: %d, %d bytes, %v; want 200 with 1024 bytes", code, n, err)
	}
	set := func(mode string) {
		t.Helper()
		if err := setFault(t.Context(), ctl.Client(), ctl.URL, mode); err != nil {
			t.Fatal(err)
		}
	}
	set(mode5xx)
	if code, n, err := get(t, c, url); err != nil || code != 503 || n != 0 {
		t.Errorf("5xx: %d, %d bytes, %v; want 503 with an empty body", code, n, err)
	}
	// The client keeps its connection from the requests above: refused
	// closes it too, and refuses a new one.
	set(modeRefused)
	if _, _, err := get(t, c, url); err == nil {
		t.Error("refused: a kept-alive request succeeded")
	}
	_, _, err := get(t, &http.Client{Transport: &http.Transport{}}, url)
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("refused: new connection error %v, want connection refused", err)
	}
	if oe, ok := errors.AsType[*net.OpError](err); !ok || oe.Op != "dial" {
		t.Errorf("refused: error %v is not a dial error", err)
	}
	set(modeRefused) // again: still refusing, no error
	set(modeNone)
	if code, n, err := get(t, c, url); err != nil || code != 200 || n != 1024 {
		t.Errorf("none after refused: %d, %d bytes, %v", code, n, err)
	}
	if err := setFault(t.Context(), ctl.Client(), ctl.URL, "slow"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("unknown mode: error %v, want a 400", err)
	}
}

func TestMockClient(t *testing.T) {
	m, url, ctl := startMock(t)
	addr := strings.TrimPrefix(ctl.URL, "http://")
	if code := runMock(t.Context(), []string{"-control", addr, "-healthcheck"}, io.Discard); code != 0 {
		t.Errorf("-healthcheck: exit code %d", code)
	}
	if code := runMock(t.Context(), []string{"-control", addr, "-set", mode5xx}, io.Discard); code != 0 || !m.fail.Load() {
		t.Errorf("-set 5xx: exit code %d, failing %v", code, m.fail.Load())
	}
	if code, _, _ := get(t, &http.Client{}, url); code != 503 {
		t.Errorf("after -set 5xx: %d", code)
	}
	if code := runMock(t.Context(), []string{"-control", addr, "-set", "slow"}, io.Discard); code != 1 {
		t.Errorf("-set slow: exit code %d, want 1", code)
	}
	if code := runMock(t.Context(), []string{"extra"}, io.Discard); code != 2 {
		t.Errorf("extra argument: exit code %d, want 2", code)
	}
	ctl.Close()
	if code := runMock(t.Context(), []string{"-control", addr, "-healthcheck"}, io.Discard); code != 1 {
		t.Errorf("-healthcheck with the mock gone: exit code %d, want 1", code)
	}
}
