//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/ratelimit"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// TestLimiterKeepsConnections runs the limiter client on the compose Redis
// Cluster through proxies (ADR-0079):
//
//   - calls whose replies come after their 5 ms budget (the proxies hold
//     back replies on the connections that carry the script) are answered
//     as timeouts at the budget, and keep their connections: the next burst
//     of calls opens no new connection and reads its own replies;
//   - with the nodes cut off, calls are still answered at their budget, and
//     every connection a call was stuck on is closed within the 1 s hard
//     timeout although the nodes still do not answer.
func TestLimiterKeepsConnections(t *testing.T) {
	c, proxies, set := newLagClient(t)
	accepted := func() (n int64) {
		for _, p := range proxies {
			n += p.accepted.Load()
		}
		return n
	}

	limits := []redis.Limit{gcraLimit(redis.GlobalKey(scriptName("conns"), time.Second, redis.KeyDigest("k")), "global", true, 1_000_000, time.Second, 1_000_000)}
	const burst = 20
	// calls makes burst concurrent calls under budget and returns their
	// errors and longest answer time.
	calls := func(budget time.Duration) ([]error, time.Duration) {
		errs := make([]error, burst)
		var longest atomic.Int64
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				start := time.Now()
				var res redis.Result
				res, errs[i] = c.Call(ctx, 1, limits)
				if errs[i] == nil && !res.Allowed {
					errs[i] = errors.New("denied")
				}
				for d := int64(time.Since(start)); ; {
					if m := longest.Load(); d <= m || longest.CompareAndSwap(m, d) {
						break
					}
				}
			})
		}
		wg.Wait()
		return errs, time.Duration(longest.Load())
	}
	isTimeout := func(err error) bool {
		var e *redis.Error
		return errors.As(err, &e) && e.Kind == redis.KindTimeout
	}

	// Replies 30 ms late: below the health checks' 50 ms, above the budget.
	set(30*time.Millisecond, false)
	errs, longest := calls(5 * time.Millisecond)
	for i, err := range errs {
		if !isTimeout(err) {
			t.Fatalf("late reply, call %d: %v, want a timeout", i+1, err)
		}
	}
	if longest > 100*time.Millisecond {
		t.Errorf("late replies: answered after up to %v, want at the 5 ms budget", longest)
	}
	time.Sleep(300 * time.Millisecond) // the late replies come and are read off
	set(0, false)
	before := accepted()
	errs, _ = calls(5 * time.Second)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("after the late replies, call %d: %v", i+1, err)
		}
	}
	t.Logf("measure: %d timed-out calls answered within %v; the next %d calls opened %d connections", burst, longest, burst, accepted()-before)
	if n := accepted() - before; n != 0 {
		t.Errorf("the next %d calls opened %d connections, want 0", burst, n)
	}

	// Cut off: the burst's calls take the pool's connections and get no
	// reply.
	set(0, true)
	cut := time.Now()
	errs, longest = calls(5 * time.Millisecond)
	for i, err := range errs {
		if !isTimeout(err) {
			t.Fatalf("cut off, call %d: %v, want a timeout", i+1, err)
		}
	}
	if longest > 100*time.Millisecond {
		t.Errorf("cut off: answered after up to %v, want at the 5 ms budget", longest)
	}
	for {
		stuck := 0
		for _, p := range proxies {
			stuck += p.stuck()
		}
		if stuck == 0 {
			break
		}
		if time.Since(cut) > 2500*time.Millisecond {
			t.Fatalf("%d connections a call was stuck on still open %v after the cut, want none after the 1 s hard timeout", stuck, time.Since(cut))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("measure: every connection a call was stuck on closed %v after the cut", time.Since(cut))
}

// newLagClient returns a warmed limiter client on the compose Redis Cluster
// through one lagProxy per node, the proxies, and set, which sets every
// proxy's lag and down.
func newLagClient(t *testing.T) (c *redis.Client, proxies []*lagProxy, set func(lag time.Duration, down bool)) {
	t.Helper()
	nodes, err := redis.ParseAddrMap(os.Getenv(redis.AddrMapEnv))
	if err != nil || nodes == nil {
		t.Fatalf("%s: %v; run make test-integration", redis.AddrMapEnv, err)
	}
	addrMap := make(map[string]string)
	for i := range 6 {
		node := fmt.Sprintf("redis-%d:6379", i)
		p := newLagProxy(t, nodes[node])
		proxies = append(proxies, p)
		addrMap[node] = p.addr
	}
	set = func(lag time.Duration, down bool) {
		for _, p := range proxies {
			p.lag.Store(int64(lag))
			p.down.Store(down)
		}
	}
	c = redis.NewClient([]string{"redis-0:6379"}, addrMap, redis.Auth{}, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { set(0, false); _ = c.Close() })
	warm, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	c.Warm(warm, 100*time.Millisecond)
	cancel()
	return c, proxies, set
}

// TestLimiterLateReply: a lease reservation whose reply comes after its
// 5 ms budget is answered as a timeout, and its Late returns what the
// script did: an admission with the grant written. Refunding each limit's
// granted units as of the reply's now_us, as the gateway's late refund
// does, leaves the key as if the call had never run (RL-23, ADR-0079).
func TestLimiterLateReply(t *testing.T) {
	c, _, set := newLagClient(t)
	// 10 per minute: nothing comes back with time during the test.
	lim := gcraLimit(redis.GlobalKey(scriptName("late"), time.Minute, redis.KeyDigest("k")), "global", true, 10, time.Minute, 10)
	lease := lim
	lease.LeaseK, lease.Group = 5, 1

	set(30*time.Millisecond, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	_, err := c.Call(ctx, 1, []redis.Limit{lease})
	cancel()
	var e *redis.Error
	if !errors.As(err, &e) || e.Kind != redis.KindTimeout || e.Late == nil {
		t.Fatalf("late reply: %v, want a timeout with Late", err)
	}
	res, err := e.Late()
	if err != nil || !res.Allowed || len(res.Granted) != 1 || res.Granted[0] != 5 {
		t.Fatalf("Late: %+v, %v; want an admission granting 5", res, err)
	}
	set(0, false)

	ctx, cancel = context.WithTimeout(context.Background(), redis.HardTimeout)
	defer cancel()
	if _, err := c.Refund(ctx, res.Granted[0], res.NowUS, []redis.Limit{lim}); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res, err := c.Call(ctx, 1, []redis.Limit{lim}); err != nil || res.Remaining != 9 {
		t.Errorf("next call: %+v, %v; want 9 left, the late grant refunded", res, err)
	}
}

// lagProxy forwards a local port to a Redis node. Replies on connections
// that have carried a script call are held back by lag; while down it
// forwards nothing either way, as a paused node does.
type lagProxy struct {
	addr     string
	lag      atomic.Int64 // ns
	down     atomic.Bool
	accepted atomic.Int64

	mu    sync.Mutex
	conns []*lagConn
}

type lagConn struct {
	script atomic.Bool // has carried EVALSHA or EVAL
	stuck  atomic.Bool // was sent a script call or more while down
	closed atomic.Bool // the client closed it
}

// stuck returns how many connections that were sent a script call while
// the proxy was down are still open.
func (p *lagProxy) stuck() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.conns {
		if c.stuck.Load() && !c.closed.Load() {
			n++
		}
	}
	return n
}

func newLagProxy(t *testing.T, target string) *lagProxy {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &lagProxy{addr: ln.Addr().String()}
	var mu sync.Mutex
	var open []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range open {
			_ = c.Close()
		}
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			p.accepted.Add(1)
			lc := &lagConn{}
			p.mu.Lock()
			p.conns = append(p.conns, lc)
			p.mu.Unlock()
			var d net.Dialer
			server, err := d.DialContext(context.Background(), "tcp", target)
			if err != nil {
				_ = client.Close()
				lc.closed.Store(true)
				continue
			}
			mu.Lock()
			open = append(open, client, server)
			mu.Unlock()
			go p.toServer(lc, server, client)
			go p.toClient(lc, client, server)
		}
	}()
	return p
}

// toServer copies the client's commands to the node, marking the
// connection once it carries a script call.
func (p *lagProxy) toServer(lc *lagConn, server, client net.Conn) {
	defer func() { lc.closed.Store(true); _ = server.Close() }()
	buf := make([]byte, 32<<10)
	for {
		n, err := client.Read(buf)
		if n > 0 {
			if bytes.Contains(bytes.ToLower(buf[:n]), []byte("eval")) {
				lc.script.Store(true)
				if p.down.Load() {
					lc.stuck.Store(true)
				}
			}
			if !p.down.Load() {
				if _, werr := server.Write(buf[:n]); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// toClient copies the node's replies back, lag late on a connection that
// has carried a script call.
func (p *lagProxy) toClient(lc *lagConn, client, server net.Conn) {
	defer func() { _ = client.Close() }()
	buf := make([]byte, 32<<10)
	for {
		n, err := server.Read(buf)
		if n > 0 && !p.down.Load() {
			if lag := time.Duration(p.lag.Load()); lag > 0 && lc.script.Load() {
				time.Sleep(lag)
			}
			if _, werr := client.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
