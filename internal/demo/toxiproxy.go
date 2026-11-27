package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The demo's toxiproxy proxies (ADR-0094 §5, docs/ports.md): the demo
// gateways reach Redis node N through demo-redis-N, which the
// SPILLWAY_REDIS_ADDR_MAP of deploy/compose/demo.compose.yml names, and the
// orders upstream's endpoints through demo-upstream-N. They listen inside
// the compose network only. ADR-0074's transient redis-N proxies of make
// chaos-redis-latency have other names and ports, so neither path deletes
// the other's.

// Proxy is one toxiproxy proxy.
type Proxy struct {
	Name     string `json:"name"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
}

// DemoPrefix starts the name of every demo proxy.
const DemoPrefix = "demo-"

// DemoProxies are the demo's proxies, in creation order.
func DemoProxies() []Proxy {
	var ps []Proxy
	for n := range 6 {
		ps = append(ps, Proxy{Name: fmt.Sprintf("demo-redis-%d", n), Listen: fmt.Sprintf("0.0.0.0:%d", 26380+n), Upstream: fmt.Sprintf("redis-%d:6379", n)})
	}
	for n := 1; n <= 3; n++ {
		ps = append(ps, Proxy{Name: fmt.Sprintf("demo-upstream-%d", n), Listen: fmt.Sprintf("0.0.0.0:%d", 26390+n), Upstream: fmt.Sprintf("upstream-%d:8080", n)})
	}
	return ps
}

// Toxiproxy calls toxiproxy's HTTP API.
type Toxiproxy struct {
	base string
	http *http.Client
}

// NewToxiproxy returns a client of the toxiproxy API at base, such as
// http://toxiproxy:8474; timeout bounds each call.
func NewToxiproxy(base string, timeout time.Duration) *Toxiproxy {
	return &Toxiproxy{base: strings.TrimSuffix(base, "/"), http: &http.Client{Timeout: timeout}}
}

func (t *Toxiproxy) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		j, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, rd) //nolint:gosec // G704: toxiproxy in the compose network is the point
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.http.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return 0, nil, fmt.Errorf("toxiproxy %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// Names returns the names of toxiproxy's proxies, sorted.
func (t *Toxiproxy) Names(ctx context.Context) ([]string, error) {
	status, b, err := t.do(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("toxiproxy GET /proxies: status %d", status)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("toxiproxy GET /proxies: %w", err)
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}

// Delete removes the proxy name, with its toxics; one that does not exist
// is not an error.
func (t *Toxiproxy) Delete(ctx context.Context, name string) error {
	status, _, err := t.do(ctx, http.MethodDelete, "/proxies/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("toxiproxy DELETE /proxies/%s: status %d", name, status)
	}
	return nil
}

// Create deletes any proxy named p.Name, then creates p, enabled and with
// no toxics.
func (t *Toxiproxy) Create(ctx context.Context, p Proxy) error {
	if err := t.Delete(ctx, p.Name); err != nil {
		return err
	}
	status, b, err := t.do(ctx, http.MethodPost, "/proxies", map[string]any{"name": p.Name, "listen": p.Listen, "upstream": p.Upstream, "enabled": true})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("toxiproxy POST /proxies %s: status %d: %s", p.Name, status, bytes.TrimSpace(b))
	}
	return nil
}
