package redis

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: EVALSHA names scripts by SHA-1
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

// script is the limiter script (RL-11). Calls use EVALSHA and fall back to
// EVAL on NOSCRIPT, which also caches it on that node (RL-16).
//
//go:embed limiter_v1.lua
var script string

var scriptSHA = func() string {
	sum := sha1.Sum([]byte(script)) //nolint:gosec // G401: as above
	return hex.EncodeToString(sum[:])
}()

// MaxLimits is the most limits one call carries: shadow_would_deny is a
// 32-bit mask.
const MaxLimits = 32

// Error kinds, the label values of gw_redis_errors_total{kind} (ADR-0021).
const (
	KindTimeout = "timeout" // the deadline passed during the call
	KindConn    = "conn"    // no reply from Redis, or a cluster-state reply
	KindScript  = "script"  // any other error reply, or a reply that does not decode
)

// Error is every error Call and Refund return.
type Error struct {
	Kind string // KindTimeout, KindConn or KindScript
	Node string // primary address owning the call's slot, e.g. "redis-1:6379"; "" if unknown
	Err  error
}

func (e *Error) Error() string {
	if e.Node == "" {
		return fmt.Sprintf("redis limiter (%s): %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("redis limiter (%s) on %s: %v", e.Kind, e.Node, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// errOldRedis fails calls to a node older than Redis 7 (RL-30).
var errOldRedis = errors.New("node runs Redis older than 7")

// Limit is one limit in a call. All Keys of one call share a hash slot.
type Limit struct {
	Key       string        // full Redis key (keys.go)
	Level     string        // global | tenant | consumer | route
	Enforce   bool          // false: shadow
	Algorithm string        // config.GCRA or config.SlidingWindow
	Rate      int64         // configured, cluster-wide
	Period    time.Duration // whole seconds
	Burst     int64         // GCRA only, after its default
}

// Result is a call's reply.
type Result struct {
	Allowed         bool
	DenyingLevel    string               // "" when allowed
	DenyingLimit    int                  // index into the call's limits; -1 when allowed
	Remaining       int64                // of DenyingLimit, else of the tightest enforce limit
	ResetAfter      time.Duration        // as Remaining
	RetryAfter      time.Duration        // 0 when allowed
	ShadowWouldDeny uint32               // bit i: limits[i] is shadow and would deny
	NowUS           int64                // Redis TIME the script used, µs since the Unix epoch
	Limits          []ratelimit.Decision // per limit, in call order
}

// Client runs the limiter script on Redis Cluster.
type Client struct {
	rdb      *goredis.ClusterClient
	log      *slog.Logger
	breakers *breakers
	dialer   func(ctx context.Context, network, addr string) (net.Conn, error) // nil: go-redis's
	stop     context.CancelFunc                                                // ends the health checks
	checks   sync.WaitGroup                                                    // the health check loop

	mu    sync.Mutex
	nodes map[string]bool            // node addresses checked to run Redis 7+ (RL-30)
	pings map[string]*goredis.Client // the health checks' own client per node
}

// NewClient returns the limiter's cluster client, dialing through addrMap
// when it is set (ADR-0004), with one circuit breaker per node on clock and
// the health checks that drive them (RL-22, ADR-0025). It never contacts
// Redis itself; Warm and calls do (GW-24), and the health checks PING only
// the nodes those have met.
//
// MaxRetries 0 stops the node clients from retrying, and
// ContextTimeoutEnabled makes a call's deadline reach the socket (RL-16).
// DisableRoutingPolicies sends each call straight to its slot's node: with
// routing policies on, go-redis fetches COMMAND on the first call, inside
// that call's budget, and takes one client-wide lock on every call
// (ADR-0025).
func NewClient(addrs []string, addrMap map[string]string, clock ratelimit.Clock, log *slog.Logger) *Client {
	opt := &goredis.ClusterOptions{Addrs: addrs, MaxRetries: 0, ContextTimeoutEnabled: true, DisableRoutingPolicies: true}
	if addrMap != nil {
		opt.Dialer = Dialer(addrMap)
	}
	c := &Client{rdb: goredis.NewClusterClient(opt), log: log, dialer: opt.Dialer, nodes: make(map[string]bool), pings: make(map[string]*goredis.Client)}
	// A breaker that opens may mean a failover: reload the slot map now, so
	// the slot reaches the promoted replica as soon as it is elected
	// (ADR-0021). The reload runs in the background.
	c.breakers = newBreakers(clock, log, c.reload, func(addr string) { go c.refill(addr) })
	ctx, stop := context.WithCancel(context.Background())
	c.stop = stop
	c.checks.Go(func() { c.health(ctx) })
	return c
}

// reload reloads the slot map in the background; go-redis runs at most one
// reload per 200 ms.
func (c *Client) reload() { c.rdb.ReloadState(context.Background()) }

// refill opens one request connection to the node at addr, outside any
// budget: the timeouts of an outage closed the node's pooled connections,
// so without it the first call after the breaker closes would dial inside
// its budget (ADR-0025).
func (c *Client) refill(addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
	defer cancel()
	_ = c.rdb.ForEachShard(ctx, func(ctx context.Context, node *goredis.Client) error {
		if node.Options().Addr != addr {
			return nil
		}
		return node.Ping(ctx).Err()
	})
}

// health PINGs every node the client has met every pingEvery, until ctx
// ends (ADR-0025).
func (c *Client) health(ctx context.Context) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.ping(ctx)
	}
}

// ping makes one health check round: one PING to each node at once, each
// on the node's own health check connection with pingTimeout, outside any
// request budget. While a breaker is open each round also reloads the slot
// map, so a failover reaches the client soon after the cluster elects the
// replica.
func (c *Client) ping(ctx context.Context) {
	addrs, open := c.breakers.checked()
	if open {
		c.reload()
	}
	var wg sync.WaitGroup
	for _, addr := range addrs {
		node := c.pinger(addr)
		wg.Go(func() {
			sent := c.breakers.clock.Now()
			ctx, cancel := context.WithTimeout(ctx, pingTimeout)
			defer cancel()
			err := node.Ping(ctx).Err()
			if errors.Is(ctx.Err(), context.Canceled) {
				return // the client is closing
			}
			c.breakers.pinged(addr, sent, err == nil)
		})
	}
	wg.Wait()
}

// pinger returns the health checks' client for addr: one connection, no
// retries, every timeout pingTimeout.
func (c *Client) pinger(addr string) *goredis.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	node := c.pings[addr]
	if node == nil {
		node = goredis.NewClient(&goredis.Options{
			Addr: addr, Dialer: c.dialer, MaxRetries: -1, PoolSize: 1, ContextTimeoutEnabled: true,
			DialTimeout: pingTimeout, ReadTimeout: pingTimeout, WriteTimeout: pingTimeout,
		})
		c.pings[addr] = node
	}
	return node
}

// warmTimeout bounds one warm-up pass.
const warmTimeout = 5 * time.Second

// Warm prepares every node for the request path (ADR-0024): it loads the
// slot map and, on every primary and replica, opens a connection, checks the
// Redis version (RL-30) and loads the script, so that a request's first call
// to a node, or to a replica promoted later, is one EVALSHA within its
// budget. It tries again every retry until one pass succeeds or ctx ends.
// The gateway runs it in the background, so readiness never waits for Redis
// (GW-24).
func (c *Client) Warm(ctx context.Context, retry time.Duration) {
	t := time.NewTimer(0)
	defer t.Stop()
	for failed := false; ; {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := c.warm(ctx)
		if err == nil {
			c.log.Info("redis limiter warmed", "nodes", n)
			return
		}
		if !failed {
			c.log.Warn("redis limiter warm-up failed; trying again", "err", err, "every", retry)
			failed = true
		}
		t.Reset(retry)
	}
}

// warm makes one warm-up pass and returns how many nodes it prepared. A node
// below Redis 7 is skipped: its breaker is held open for good.
func (c *Client) warm(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, warmTimeout)
	defer cancel()
	var n atomic.Int64
	err := c.rdb.ForEachShard(ctx, func(ctx context.Context, node *goredis.Client) error {
		addr := node.Options().Addr
		c.breakers.add(addr) // health checks cover replicas before a failover promotes them
		if err := c.checkVersion(ctx, node, addr); err != nil {
			if errors.Is(err, errOldRedis) {
				return nil
			}
			return err
		}
		if err := node.ScriptLoad(ctx, script).Err(); err != nil {
			return classify(ctx, addr, err)
		}
		n.Add(1)
		return nil
	})
	return int(n.Load()), err
}

// Close stops the health checks and closes the client's connections.
func (c *Client) Close() error {
	c.stop()
	c.checks.Wait() // no round is left to make a health check client after this
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, node := range c.pings {
		_ = node.Close()
	}
	return c.rdb.Close()
}

// Call checks every limit and writes per the write rule, in one script call.
// cost must be positive.
func (c *Client) Call(ctx context.Context, cost int64, limits []Limit) (Result, error) {
	return c.run(ctx, encode(cost, limits), limits)
}

// Refund reverses an earlier Call's deduction of cost on the limits it wrote.
// admittedUS is that Call's NowUS; 0 refunds from the current window.
func (c *Client) Refund(ctx context.Context, cost, admittedUS int64, limits []Limit) (Result, error) {
	return c.run(ctx, append(encode(-cost, limits), admittedUS), limits)
}

// encode returns the script's ARGV for cost and limits.
func encode(cost int64, limits []Limit) []any {
	args := make([]any, 0, 1+6*len(limits)+1)
	args = append(args, cost)
	for _, l := range limits {
		mode, burst := "shadow", int64(0)
		if l.Enforce {
			mode = "enforce"
		}
		if l.Algorithm == config.GCRA {
			burst = l.Burst
		}
		args = append(args, l.Level, mode, l.Algorithm, l.Rate, l.Period.Milliseconds(), burst)
	}
	return args
}

func (c *Client) run(ctx context.Context, args []any, limits []Limit) (Result, error) {
	if len(limits) == 0 || len(limits) > MaxLimits {
		return Result{}, &Error{Kind: KindScript, Err: fmt.Errorf("a call carries 1 to %d limits, not %d", MaxLimits, len(limits))}
	}
	keys := make([]string, len(limits))
	for i, l := range limits {
		keys[i] = l.Key
	}
	node, err := c.rdb.MasterForKey(ctx, keys[0])
	if err != nil {
		kind := KindConn
		if timedOut(ctx, err) {
			kind = KindTimeout
		}
		return Result{}, &Error{Kind: kind, Err: err}
	}
	addr := node.Options().Addr
	if err := c.breakers.allow(addr); err != nil {
		return Result{}, &Error{Kind: KindConn, Node: addr, Err: err}
	}
	res, err := c.send(ctx, node, addr, keys, args, len(limits))
	replied, broken := outcome(err)
	c.breakers.done(addr, replied, broken)
	return res, err
}

// outcome says what a call's error means for its node's breaker
// (ADR-0025): replied, Redis answered (a decision or an error reply);
// broken, the connection broke without a reply (a dial error, a reset, EOF,
// a broken pipe). A timeout is neither: it says how long the call waited,
// not that the node is down; the health checks find a node that stopped
// answering.
func outcome(err error) (replied, broken bool) {
	var e *Error
	var rerr goredis.Error
	switch {
	case err == nil, errors.As(err, &rerr):
		return true, false
	case !errors.As(err, &e), e.Kind != KindConn:
		return false, false
	}
	// Waiting for a pooled connection or a closing client says nothing
	// about the node; nor does a node refused for its version (RL-30).
	return false, !errors.Is(err, goredis.ErrPoolTimeout) && !errors.Is(err, goredis.ErrClosed) && !errors.Is(err, errOldRedis)
}

// send runs the script on node, the primary at addr, once its version is
// checked.
func (c *Client) send(ctx context.Context, node *goredis.Client, addr string, keys []string, args []any, n int) (Result, error) {
	if err := c.checkVersion(ctx, node, addr); err != nil {
		return Result{}, err
	}
	cmd := scriptCmd(ctx, "evalsha", scriptSHA, keys, args)
	err := c.rdb.Process(ctx, cmd)
	if isNoScript(err) {
		cmd = scriptCmd(ctx, "eval", script, keys, args)
		err = c.rdb.Process(ctx, cmd)
	}
	if err != nil {
		return Result{}, classify(ctx, addr, err)
	}
	res, err := decode(cmd.Val(), n)
	if err != nil {
		return Result{}, &Error{Kind: KindScript, Node: addr, Err: err}
	}
	return res, nil
}

// noRetryCmd is a script call that go-redis never resends (RL-16). NoRetry
// stops its cluster and node loops from resending after an error that may
// follow execution; MOVED, ASK and READONLY replies, which mean the script
// did not run, are still followed.
type noRetryCmd struct{ *goredis.Cmd }

func (noRetryCmd) NoRetry() bool { return true }

func scriptCmd(ctx context.Context, name, script string, keys []string, args []any) noRetryCmd {
	a := make([]any, 0, 3+len(keys)+len(args))
	a = append(a, name, script, len(keys))
	for _, k := range keys {
		a = append(a, k)
	}
	a = append(a, args...)
	cmd := goredis.NewCmd(ctx, a...)
	cmd.SetFirstKeyPos(3)
	return noRetryCmd{cmd}
}

func isNoScript(err error) bool {
	var rerr goredis.Error
	return errors.As(err, &rerr) && strings.HasPrefix(rerr.Error(), "NOSCRIPT")
}

// clusterReplies are error replies that mean the node could not run the
// script: they count as conn, like no reply at all.
var clusterReplies = []string{"CLUSTERDOWN", "TRYAGAIN", "MASTERDOWN", "LOADING", "READONLY", "MOVED", "ASK"}

// classify maps a failed call's error to its kind (ADR-0021).
func classify(ctx context.Context, node string, err error) *Error {
	kind := KindConn
	var rerr goredis.Error
	switch {
	case errors.As(err, &rerr):
		kind = KindScript
		for _, p := range clusterReplies {
			if strings.HasPrefix(rerr.Error(), p+" ") || rerr.Error() == p {
				kind = KindConn
			}
		}
	case errors.Is(err, goredis.ErrPoolTimeout):
	case timedOut(ctx, err):
		kind = KindTimeout
	}
	return &Error{Kind: kind, Node: node, Err: err}
}

// timedOut reports whether err is a timeout or ctx's deadline has passed.
// It reads the deadline itself: a socket deadline taken from ctx can fire
// before ctx's own timer marks ctx done (ADR-0025).
func timedOut(ctx context.Context, err error) bool {
	var nerr net.Error
	deadline, ok := ctx.Deadline()
	return errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil ||
		ok && !time.Now().Before(deadline) || errors.As(err, &nerr) && nerr.Timeout()
}

// checkVersion makes sure the node at addr runs Redis 7 or later (RL-30),
// asking it with INFO server under ctx the first time. An INFO that fails
// fails the call and is tried again next time; a node below 7 has its
// breaker held open, so every later call to it fails without being sent.
func (c *Client) checkVersion(ctx context.Context, node *goredis.Client, addr string) error {
	c.mu.Lock()
	checked := c.nodes[addr]
	c.mu.Unlock()
	if checked {
		return nil
	}
	info, err := node.Info(ctx, "server").Result()
	if err != nil {
		return classify(ctx, addr, err)
	}
	version, major, err := redisVersion(info)
	if err != nil {
		return &Error{Kind: KindScript, Node: addr, Err: err}
	}
	if major < 7 {
		if c.breakers.hold(addr, errOldRedis) {
			c.log.Error("redis limiter: node runs Redis older than 7; every call to it fails", "node", addr, "redis_version", version)
		}
		return &Error{Kind: KindConn, Node: addr, Err: errOldRedis}
	}
	c.mu.Lock()
	c.nodes[addr] = true
	c.mu.Unlock()
	return nil
}

// redisVersion returns redis_version from an INFO server reply and its
// major version.
func redisVersion(info string) (string, int, error) {
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); ok {
			major, _, _ := strings.Cut(v, ".")
			n, err := strconv.Atoi(major)
			if err != nil {
				return "", 0, fmt.Errorf("INFO server: redis_version %q", v)
			}
			return v, n, nil
		}
	}
	return "", 0, errors.New("INFO server: no redis_version")
}

// decode reads the script's reply for a call of n limits.
func decode(v any, n int) (Result, error) {
	reply, ok := v.([]any)
	if !ok || len(reply) != 8+4*n {
		return Result{}, fmt.Errorf("limiter reply: want an array of %d, got %T of %d", 8+4*n, v, len(reply))
	}
	ints := make([]int64, len(reply))
	for i, x := range reply {
		if i == 1 {
			continue
		}
		if ints[i], ok = x.(int64); !ok {
			return Result{}, fmt.Errorf("limiter reply: field %d is %T, want an integer", i+1, x)
		}
	}
	level, ok := reply[1].(string)
	if !ok {
		return Result{}, fmt.Errorf("limiter reply: field 2 is %T, want a string", reply[1])
	}
	if ints[2] < 0 || ints[2] > int64(n) || ints[6] < 0 || ints[6] >= 1<<n {
		return Result{}, fmt.Errorf("limiter reply: denying_limit %d or shadow_would_deny %d out of range", ints[2], ints[6])
	}
	res := Result{
		Allowed:         ints[0] == 1,
		DenyingLevel:    level,
		DenyingLimit:    int(ints[2]) - 1,
		Remaining:       ints[3],
		ResetAfter:      ms(ints[4]),
		RetryAfter:      ms(ints[5]),
		ShadowWouldDeny: uint32(ints[6]), //nolint:gosec // G115: checked < 2^n ≤ 2^32 above
		NowUS:           ints[7],
		Limits:          make([]ratelimit.Decision, n),
	}
	for i := range res.Limits {
		f := ints[8+4*i:]
		res.Limits[i] = ratelimit.Decision{Allowed: f[0] == 1, Remaining: f[1], ResetAfter: ms(f[2]), RetryAfter: ms(f[3])}
	}
	return res, nil
}

func ms(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

// Limiter adapts one limit to ratelimit.Limiter (RL-01) on the global key
// layout: Allow(ctx, key, cost), with key a ratelimit.Key, makes a one-limit
// Call, and a negative cost a Refund from the current window. Script tests
// use it; the gateway uses Call and Refund.
func (c *Client) Limiter(policy, algorithm string, l config.Limit) ratelimit.Limiter {
	return limiter{c: c, policy: policy, algorithm: algorithm, l: l}
}

type limiter struct {
	c         *Client
	policy    string
	algorithm string
	l         config.Limit
}

func (a limiter) Allow(ctx context.Context, key string, cost int64) (ratelimit.Decision, error) {
	limits := []Limit{{
		Key:       GlobalKey(a.policy, a.l.Period, digestOf(key)),
		Level:     "global",
		Enforce:   true,
		Algorithm: a.algorithm,
		Rate:      a.l.Rate,
		Period:    a.l.Period,
		Burst:     a.l.Burst,
	}}
	var res Result
	var err error
	if cost < 0 {
		res, err = a.c.Refund(ctx, -cost, 0, limits)
	} else {
		res, err = a.c.Call(ctx, cost, limits)
	}
	if err != nil {
		return ratelimit.Decision{}, err
	}
	return res.Limits[0], nil
}
