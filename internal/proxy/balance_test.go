package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/config"
)

// testPool is a pool balanced by lb over n healthy endpoints 10.0.0.1:80 …
func testPool(lb string, n int) (*pool, []*endpoint) {
	p := &pool{name: "u", log: discard}
	p.settings.Store(&config.Upstream{Name: "u", LB: lb, Outlier: config.OutlierDetection{MaxEjectedPercent: 50}})
	eps := make([]*endpoint, n)
	for i := range eps {
		eps[i] = &endpoint{addr: fmt.Sprintf("10.0.0.%d:80", i+1)}
		eps[i].healthy.Store(true)
	}
	p.endpoints.Store(&eps)
	return p, eps
}

// picks picks n times with key and tried and counts the picks per endpoint.
func picks(t *testing.T, p *pool, n int, key string, tried ...*endpoint) map[*endpoint]int {
	t.Helper()
	got := map[*endpoint]int{}
	for range n {
		e, err := p.pick(t0, key, tried)
		if err != nil {
			t.Fatal(err)
		}
		got[e]++
	}
	return got
}

// TestRoundRobinBalancing: round robin takes the candidates in turn, so each
// gets exactly its share; an unselectable endpoint is skipped and a retry
// takes an endpoint it has not tried (GW-07).
func TestRoundRobinBalancing(t *testing.T) {
	p, eps := testPool(config.LBRoundRobin, 3)
	var order []string
	for range 6 {
		e, _ := p.pick(t0, "", nil)
		order = append(order, e.addr[len("10.0.0."):len("10.0.0.")+1])
	}
	if got := strings.Join(order, ""); got != "123123" {
		t.Errorf("order %s, want 123123", got)
	}
	if got := picks(t, p, 3000, ""); got[eps[0]] != 1000 || got[eps[1]] != 1000 || got[eps[2]] != 1000 {
		t.Errorf("3000 picks: %v, want 1000 each", counts(got, eps))
	}
	eps[1].healthy.Store(false)
	if got := picks(t, p, 3000, ""); got[eps[0]] != 1500 || got[eps[1]] != 0 || got[eps[2]] != 1500 {
		t.Errorf("3000 picks with endpoint 2 unhealthy: %v, want 1500, 0, 1500", counts(got, eps))
	}
	eps[1].healthy.Store(true)
	if got := picks(t, p, 100, "", eps[0], eps[2]); got[eps[1]] != 100 {
		t.Errorf("retry after 1 and 3: %v, want 2 every time", counts(got, eps))
	}
	if got := picks(t, p, 300, "", eps...); got[eps[0]] != 100 || got[eps[1]] != 100 || got[eps[2]] != 100 {
		t.Errorf("retry after all three: %v, want all three in turn", counts(got, eps))
	}
}

// TestP2CBalancing: p2c draws two distinct candidates and takes the one with
// fewer attempts in flight, so with distinct loads the least-loaded endpoint
// wins every pair it is in and the most-loaded never wins; with equal loads
// the picks are uniform; a retry takes an endpoint it has not tried (GW-07,
// D3).
func TestP2CBalancing(t *testing.T) {
	p, eps := testPool(config.LBP2C, 4)
	const n = 60_000
	got := picks(t, p, n, "")
	for _, e := range eps {
		if math.Abs(float64(got[e])/n-0.25) > 0.02 {
			t.Errorf("equal loads: %v, want about %d each", counts(got, eps), n/4)
			break
		}
	}

	// Loads 0, 1, 2, 3: of the 6 pairs, endpoint k wins the 3-k that pair it
	// with a busier endpoint.
	for i, e := range eps {
		e.inFlight.Store(int64(i))
	}
	got = picks(t, p, n, "")
	for i, e := range eps {
		if want := float64(3-i) / 6; math.Abs(float64(got[e])/n-want) > 0.02 {
			t.Errorf("loads 0-3: %v, want shares 3/6, 2/6, 1/6, 0", counts(got, eps))
			break
		}
	}
	if got[eps[3]] != 0 {
		t.Errorf("the busiest endpoint won %d picks", got[eps[3]])
	}

	// Two candidates are always both drawn: the idle one always wins.
	if got := picks(t, p, 1000, "", eps[0], eps[1]); got[eps[2]] != 1000 {
		t.Errorf("retry after 1 and 2, loads 2 and 3 left: %v, want 3 every time", counts(got, eps))
	}
	// A key does not change p2c.
	if got := picks(t, p, 1000, "cart-1", eps[0], eps[1]); got[eps[2]] != 1000 {
		t.Errorf("with a key: %v, want 3 every time", counts(got, eps))
	}
	if got := picks(t, p, 1000, "", eps[1], eps[2], eps[3]); got[eps[0]] != 1000 {
		t.Errorf("retry after 2, 3 and 4: %v, want 1 every time", counts(got, eps))
	}
}

// TestConsistentHashBalancing (ADR-0030): 10,000 keys over 5 endpoints. No
// endpoint holds more than 1.3× its fair share. Removing an endpoint, from
// the upstream or from the candidates, moves exactly its keys and no other;
// adding it back restores the first mapping. A retry of a key takes the
// endpoint the key moves to when its own leaves: its second-ranked one. The
// mapping does not depend on the endpoints' order, so every replica agrees.
// A request without a key is balanced by p2c.
func TestConsistentHashBalancing(t *testing.T) {
	p, eps := testPool(config.LBConsistentHash, 5)
	const keys = 10_000
	mapping := func() []*endpoint {
		m := make([]*endpoint, keys)
		for k := range m {
			e, err := p.pick(t0, fmt.Sprintf("cart-%d", k), nil)
			if err != nil {
				t.Fatal(err)
			}
			m[k] = e
		}
		return m
	}
	first := mapping()
	share := map[*endpoint]int{}
	for _, e := range first {
		share[e]++
	}
	for _, e := range eps {
		if share[e] > keys/5*13/10 {
			t.Errorf("shares %v: %s holds more than 1.3× its fair share of %d", counts(share, eps), e.addr, keys/5)
		}
	}
	if fmt.Sprint(first[:20]) != fmt.Sprint(mapping()[:20]) {
		t.Fatal("the same keys mapped differently twice")
	}

	gone := eps[2]
	moved := func(what string, after []*endpoint) {
		t.Helper()
		n := 0
		for k := range first {
			switch {
			case first[k] != gone && after[k] != first[k]:
				t.Fatalf("%s: key %d moved from %s, which stayed, to %s", what, k, first[k].addr, after[k].addr)
			case first[k] == gone && after[k] == gone:
				t.Fatalf("%s: key %d still on the endpoint that left", what, k)
			case first[k] == gone:
				n++
			}
		}
		if n != share[gone] {
			t.Fatalf("%s: %d keys moved, want %d", what, n, share[gone])
		}
	}
	gone.healthy.Store(false)
	unhealthy := mapping()
	moved("endpoint unhealthy", unhealthy)
	gone.healthy.Store(true)
	if fmt.Sprint(mapping()) != fmt.Sprint(first) {
		t.Error("healthy again: the first mapping was not restored")
	}

	rest := []*endpoint{eps[0], eps[1], eps[3], eps[4]}
	p.endpoints.Store(&rest)
	removed := mapping()
	moved("endpoint removed", removed)
	if fmt.Sprint(removed) != fmt.Sprint(unhealthy) {
		t.Error("a removed endpoint's keys moved elsewhere than an unhealthy one's")
	}
	reversed := []*endpoint{eps[4], eps[3], eps[2], eps[1], eps[0]}
	p.endpoints.Store(&reversed)
	if fmt.Sprint(mapping()) != fmt.Sprint(first) {
		t.Error("added back in another order: the first mapping was not restored")
	}

	for k := range 1000 {
		key := fmt.Sprintf("cart-%d", k)
		if e, _ := p.pick(t0, key, []*endpoint{first[k]}); e == first[k] || first[k] == gone && e != unhealthy[k] {
			t.Fatalf("retry of key %d after %s: %s, want its second-ranked endpoint", k, first[k].addr, e.addr)
		}
	}

	for i, e := range eps {
		e.inFlight.Store(int64(i + 1))
	}
	eps[3].inFlight.Store(0)
	if got := picks(t, p, 1000, ""); got[eps[3]] < 350 || got[eps[4]] != 0 {
		t.Errorf("no key, endpoint 4 idle: %v, want p2c's picks", counts(got, eps))
	}
}

// TestPickConcurrently runs picks, in-flight counts, health changes and DNS
// re-resolutions at once, for the race detector; every count ends at 0.
func TestPickConcurrently(t *testing.T) {
	for _, lb := range []string{config.LBRoundRobin, config.LBP2C, config.LBConsistentHash} {
		t.Run(lb, func(t *testing.T) {
			p, eps := testPool(lb, 5)
			ctx, stop := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			wg.Go(func() {
				for i := 0; ctx.Err() == nil; i++ {
					eps[i%5].healthy.Store(i%3 != 0)
					sub := eps[:3+i%3]
					p.endpoints.Store(&sub)
				}
			})
			var workers sync.WaitGroup
			for w := range 8 {
				workers.Go(func() {
					for i := range 2000 {
						e, err := p.pick(t0, fmt.Sprintf("k%d", i), nil)
						if err != nil {
							t.Error(err)
							return
						}
						release := e.begin()
						if i%2 == 0 {
							release()
						}
						if e2, _ := p.pick(t0, "", []*endpoint{e}); w%2 == 0 {
							e2.begin()()
						}
						release()
					}
				})
			}
			workers.Wait()
			stop()
			wg.Wait()
			for _, e := range eps {
				if n := e.inFlight.Load(); n != 0 {
					t.Errorf("%s: %d in flight after every attempt ended", e.addr, n)
				}
			}
		})
	}
}

// TestInFlightReleased: an endpoint's in-flight count covers an attempt from
// sending it until its response body is closed, and returns to 0 however
// the request ends: a response, a 5xx, a refused connection, a timeout, a
// client that goes away before or during the response, and a 101 whose
// tunnel closes (GW-07). The gateway's own in-flight count, which the
// in-flight cap uses, does the same (GW-26).
func TestInFlightReleased(t *testing.T) {
	hold, held := make(chan struct{}), make(chan struct{}, 1)
	u := newUpstream(t, "u")
	u.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, "{}")
		case "/503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/headers": // holds before the headers
			held <- struct{}{}
			<-hold
		case "/body": // holds while the body streams
			_, _ = io.WriteString(w, "first part")
			_ = http.NewResponseController(w).Flush()
			held <- struct{}{}
			<-hold
		case "/upgrade":
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "test")
			w.WriteHeader(http.StatusSwitchingProtocols)
			conn, brw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = brw.ReadString('\n') // until the client closes the tunnel
		}
	})
	t.Cleanup(func() { close(hold) })
	refused := newUpstream(t, "refused")
	refused.Close()
	g := newGateway(t, newResolver(u, refused), fmt.Sprintf(`
upstreams:
  - {name: u, endpoints: [{address: 'u:%s'}]}
  - {name: refused, endpoints: [{address: 'refused:%s'}]}
routes:
  - {name: refused, upstream: refused, match: {path_prefix: /refused}}
  - {name: timeout, upstream: u, match: {path_prefix: /headers, headers: [{name: x-timeout, exact: '1'}]}, timeouts: {per_try: 100ms}}
  - {name: u, upstream: u, match: {path_prefix: /}}
`, u.port(), refused.port()))
	inFlight := func(name string) int64 { return (*(*g.reg.pools.Load())[name].endpoints.Load())[0].inFlight.Load() }
	zero := func(what, name string) {
		t.Helper()
		waitFor(t, what+": in-flight count back to 0", func() bool { return inFlight(name) == 0 && g.h.InFlight() == 0 })
	}

	for _, path := range []string{"/ok", "/503"} {
		g.get(t, path, nil)
		zero(path, "u")
	}
	g.get(t, "/refused", nil)
	zero("refused connection", "refused")
	if status, _, _ := g.get(t, "/headers", http.Header{"X-Timeout": {"1"}}); status != http.StatusGatewayTimeout {
		t.Errorf("per-try timeout: %d, want 504", status)
	}
	<-held
	zero("per-try timeout", "u")

	// The client goes away before the headers.
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/headers", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := g.Client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-held
	if n, m := inFlight("u"), g.h.InFlight(); n != 1 || m != 1 {
		t.Errorf("waiting for the headers: %d in flight at the endpoint and %d at the gateway, want 1", n, m)
	}
	cancel()
	<-done
	zero("client gone before the headers", "u")

	// The client goes away while the body streams: the copy fails and the
	// proxy panics with http.ErrAbortHandler.
	ctx, cancel = context.WithCancel(t.Context())
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/body", nil)
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-held
	if n, m := inFlight("u"), g.h.InFlight(); n != 1 || m != 1 {
		t.Errorf("while the body streams: %d in flight at the endpoint and %d at the gateway, want 1", n, m)
	}
	cancel()
	_ = resp.Body.Close()
	zero("client gone during the body", "u")

	// An upgrade holds the attempt until its tunnel closes.
	conn := dial(t, g.Listener.Addr().String())
	_, _ = io.WriteString(conn, "GET /upgrade HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	upgraded, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = upgraded.Body.Close()
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d, want 101", upgraded.StatusCode)
	}
	if n, m := inFlight("u"), g.h.InFlight(); n != 1 || m != 1 {
		t.Errorf("tunnel open: %d in flight at the endpoint and %d at the gateway, want 1", n, m)
	}
	_ = conn.Close()
	zero("tunnel closed", "u")
}

// TestP2CAvoidsSlowEndpoint: through the gateway, concurrent requests to two
// fast endpoints and one that takes 50 ms. The slow one keeps attempts in
// flight, so p2c sends it far less than round robin's third (GW-07, D3).
func TestP2CAvoidsSlowEndpoint(t *testing.T) {
	fast1, fast2, slow := newUpstream(t, "fast1"), newUpstream(t, "fast2"), newUpstream(t, "slow")
	echoes := slow.Config.Handler
	slow.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		echoes.ServeHTTP(w, r)
	})
	g := newGateway(t, newResolver(fast1, fast2, slow), upstreamOf("lb: p2c", fast1, fast2, slow))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.URL+"/", nil)
				resp, err := g.Client().Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		})
	}
	wg.Wait()
	total := fast1.requests.Load() + fast2.requests.Load() + slow.requests.Load()
	if share := float64(slow.requests.Load()) / float64(total); share > 0.15 {
		t.Errorf("slow endpoint served %d of %d requests (%.0f%%), want under 15%%", slow.requests.Load(), total, share*100)
	}
	t.Logf("slow endpoint served %d of %d requests", slow.requests.Load(), total)
}

// TestConsistentHashThroughGateway: requests with the same hash_on key reach
// the same endpoint; requests without it still reach every endpoint (p2c);
// {ip} fills even without rate limit policies (GW-07).
func TestConsistentHashThroughGateway(t *testing.T) {
	a, b, c := newUpstream(t, "a"), newUpstream(t, "b"), newUpstream(t, "c")
	g := newGateway(t, newResolver(a, b, c), upstreamOf("lb: consistent_hash, hash_on: 'cart:{header:X-Cart-Id}'", a, b, c))
	seen := map[string]bool{}
	for k := range 30 {
		header := http.Header{"X-Cart-Id": {fmt.Sprint(k)}}
		_, _, first := g.get(t, "/", header)
		for range 3 {
			if _, _, e := g.get(t, "/", header); e.Upstream != first.Upstream {
				t.Fatalf("cart %d: served by %s, then %s", k, first.Upstream, e.Upstream)
			}
		}
		seen[first.Upstream] = true
	}
	if len(seen) != 3 {
		t.Errorf("30 carts reached %v, want all three endpoints", seen)
	}
	if _, from := g.served(t, 60); !strings.Contains(strings.Join(from, ","), "a") || !strings.Contains(strings.Join(from, ","), "b") || !strings.Contains(strings.Join(from, ","), "c") {
		t.Errorf("without a cart: served by %v, want every endpoint", from)
	}

	g.apply(t, upstreamOf("lb: consistent_hash, hash_on: '{ip}'", a, b, c))
	_, from := g.served(t, 10)
	if strings.Count(strings.Join(from, ","), from[0]) != 10 {
		t.Errorf("hash on {ip} from one client: served by %v, want one endpoint", from)
	}
}

func counts(got map[*endpoint]int, eps []*endpoint) []int {
	var out []int
	for _, e := range eps {
		out = append(out, got[e])
	}
	return out
}
