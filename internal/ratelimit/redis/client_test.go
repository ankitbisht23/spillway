package redis

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
)

func TestKeys(t *testing.T) {
	// The examples in ADR-0021.
	for filled, want := range map[string]string{
		"acme":  "giszrYfBSKCiClunzV68qg",
		"alice": "K9gGyX8OAK8aH8Myj6djqQ",
		"carol": "TCbZB0wn2J7eWScMCsFLcQ",
		"bytes": "J3CJ2RwL308uaGK6fkoHYA",
	} {
		if got := Digest(filled); got != want {
			t.Errorf("Digest(%q) = %q, want %q", filled, got, want)
		}
	}
	if got := Digest("a much longer filled template:with:parts"); len(got) != 22 {
		t.Errorf("Digest is %d characters, want 22", len(got))
	}
	tests := []struct{ got, want string }{
		{TenantKey("acme", "tenant-default", time.Second, Digest("acme")), "rl:v1:{t:acme}:tenant-default:1000:giszrYfBSKCiClunzV68qg"},
		{TenantKey("acme", "consumer-minute", time.Minute, Digest("alice")), "rl:v1:{t:acme}:consumer-minute:60000:K9gGyX8OAK8aH8Myj6djqQ"},
		{NoTenantKey(Digest("carol"), "consumer-minute", time.Minute), "rl:v1:{k:TCbZB0wn2J7eWScMCsFLcQ}:consumer-minute:60000"},
		{GlobalKey("global-bytes", time.Second, Digest("bytes")), "rl:v1:{g:global-bytes}:1000:J3CJ2RwL308uaGK6fkoHYA"},
		{GlobalKey("p", time.Hour, Digest("x")), "rl:v1:{g:p}:3600000:" + Digest("x")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("key = %q, want %q", tt.got, tt.want)
		}
	}
	if got, want := digestOf(ratelimit.Key("acme")), Digest("acme"); got != want {
		t.Errorf("digestOf(Key) = %q, want %q", got, want)
	}
}

func TestEncode(t *testing.T) {
	limits := []Limit{
		{Key: "a", Level: "tenant", Enforce: true, Algorithm: config.GCRA, Rate: 100, Period: time.Second, Burst: 200},
		{Key: "b", Level: "consumer", Algorithm: config.SlidingWindow, Rate: 1000, Period: time.Minute, Burst: 7},
	}
	want := []any{int64(5),
		"tenant", "enforce", "gcra", int64(100), int64(1000), int64(200),
		"consumer", "shadow", "sliding_window", int64(1000), int64(60000), int64(0),
	}
	if got := encode(5, limits); !reflect.DeepEqual(got, want) {
		t.Errorf("encode = %v, want %v", got, want)
	}
	cmd := scriptCmd(context.Background(), "evalsha", scriptSHA, []string{"a", "b"}, encode(5, limits))
	if got := cmd.Args()[:5]; !reflect.DeepEqual(got, []any{"evalsha", scriptSHA, 2, "a", "b"}) {
		t.Errorf("command starts %v", got)
	}
	if !cmd.NoRetry() {
		t.Error("script command can be retried")
	}
	if len(scriptSHA) != 40 || !strings.HasPrefix(script, "#!lua\n") {
		t.Errorf("script SHA %q, script starts %q", scriptSHA, script[:10])
	}
}

func TestDecode(t *testing.T) {
	reply := []any{int64(0), "consumer", int64(2), int64(0), int64(1500), int64(250), int64(4), int64(1700000000123456),
		int64(1), int64(7), int64(100), int64(0),
		int64(0), int64(0), int64(1500), int64(250),
		int64(0), int64(0), int64(3), int64(2)}
	got, err := decode(reply, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := Result{
		Allowed:         false,
		DenyingLevel:    "consumer",
		DenyingLimit:    1,
		Remaining:       0,
		ResetAfter:      1500 * time.Millisecond,
		RetryAfter:      250 * time.Millisecond,
		ShadowWouldDeny: 4,
		NowUS:           1700000000123456,
		Limits: []ratelimit.Decision{
			{Allowed: true, Remaining: 7, ResetAfter: 100 * time.Millisecond},
			{ResetAfter: 1500 * time.Millisecond, RetryAfter: 250 * time.Millisecond},
			{ResetAfter: 3 * time.Millisecond, RetryAfter: 2 * time.Millisecond},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decode =\n%+v, want\n%+v", got, want)
	}

	allowed := []any{int64(1), "", int64(0), int64(9), int64(10), int64(0), int64(0), int64(1), int64(1), int64(9), int64(10), int64(0)}
	if got, err := decode(allowed, 1); err != nil || !got.Allowed || got.DenyingLimit != -1 || got.DenyingLevel != "" {
		t.Errorf("decode(allowed) = %+v, %v", got, err)
	}

	bad := map[string]any{
		"not an array":       "OK",
		"short":              allowed[:11],
		"string field":       replace(allowed, 3, "9"),
		"level not a string": replace(allowed, 1, int64(0)),
		"denying past n":     replace(allowed, 2, int64(2)),
		"mask past n":        replace(allowed, 6, int64(2)),
	}
	for name, v := range bad {
		if _, err := decode(v, 1); err == nil {
			t.Errorf("decode(%s) succeeded", name)
		}
	}
}

func replace(s []any, i int, v any) []any {
	s = append([]any(nil), s...)
	s[i] = v
	return s
}

func TestRedisVersion(t *testing.T) {
	v, major, err := redisVersion("# Server\r\nredis_git_sha1:00000000\r\nredis_version:8.10.3\r\nredis_mode:cluster\r\n")
	if err != nil || v != "8.10.3" || major != 8 {
		t.Errorf("redisVersion = %q, %d, %v", v, major, err)
	}
	for _, info := range []string{"# Server\r\n", "redis_version:x.1\r\n"} {
		if _, _, err := redisVersion(info); err == nil {
			t.Errorf("redisVersion(%q) succeeded", info)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// lateCtx is a context whose deadline has passed but whose timer has not
// yet marked it done, as when a socket deadline taken from it fires first.
type lateCtx struct{ context.Context }

func (lateCtx) Deadline() (time.Time, bool) { return time.Now().Add(-time.Microsecond), true }

type replyErr string

func (e replyErr) Error() string { return string(e) }
func (replyErr) RedisError()     {}

func TestClassify(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	tests := []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{context.Background(), context.DeadlineExceeded, KindTimeout},
		{context.Background(), fmt.Errorf("read: %w", timeoutErr{}), KindTimeout},
		{expired, io.EOF, KindTimeout},
		{lateCtx{context.Background()}, io.EOF, KindTimeout},
		{context.Background(), io.EOF, KindConn},
		{context.Background(), goredis.ErrPoolTimeout, KindConn},
		{context.Background(), goredis.ErrClosed, KindConn},
		{context.Background(), &net.OpError{Op: "dial", Err: errors.New("connection refused")}, KindConn},
		{context.Background(), replyErr("CLUSTERDOWN The cluster is down"), KindConn},
		{context.Background(), replyErr("TRYAGAIN Multiple keys request during rehashing of slot"), KindConn},
		{context.Background(), replyErr("MASTERDOWN Link with MASTER is down"), KindConn},
		{context.Background(), replyErr("LOADING Redis is loading the dataset in memory"), KindConn},
		{context.Background(), replyErr("READONLY You can't write against a read only replica."), KindConn},
		{context.Background(), replyErr("MOVED 3999 127.0.0.1:6381"), KindConn},
		{expired, replyErr("ERR Error running script: spillway: cost must be a non-zero integer"), KindScript},
		{context.Background(), replyErr("OOM command not allowed when used memory > 'maxmemory'."), KindScript},
		{context.Background(), replyErr("CROSSSLOT Keys in request don't hash to the same slot"), KindScript},
		{context.Background(), replyErr("MOVEDX something"), KindScript},
	}
	for _, tt := range tests {
		e := classify(tt.ctx, "redis-1:6379", tt.err)
		if e.Kind != tt.want || e.Node != "redis-1:6379" || !errors.Is(e, tt.err) {
			t.Errorf("classify(%v) = %+v, want kind %s", tt.err, e, tt.want)
		}
	}
}

// TestOutcome: which failed calls count towards a breaker (ADR-0025).
func TestOutcome(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	tests := []struct {
		err             error
		replied, broken bool
	}{
		{nil, true, false},
		{classify(context.Background(), "n", replyErr("ERR Error running script: boom")), true, false},
		{classify(context.Background(), "n", replyErr("OOM command not allowed")), true, false},
		{classify(context.Background(), "n", replyErr("CLUSTERDOWN The cluster is down")), true, false},
		{classify(context.Background(), "n", io.EOF), false, true},
		{classify(context.Background(), "n", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), false, true},
		{classify(context.Background(), "n", fmt.Errorf("write: %w", syscall.EPIPE)), false, true},
		{classify(context.Background(), "n", fmt.Errorf("read: %w", syscall.ECONNRESET)), false, true},
		{classify(expired, "n", io.EOF), false, false},
		{classify(context.Background(), "n", context.DeadlineExceeded), false, false},
		{classify(context.Background(), "n", fmt.Errorf("read: %w", timeoutErr{})), false, false},
		{classify(context.Background(), "n", goredis.ErrPoolTimeout), false, false},
		{classify(context.Background(), "n", goredis.ErrClosed), false, false},
		{&Error{Kind: KindConn, Node: "n", Err: errOldRedis}, false, false},
		{&Error{Kind: KindScript, Node: "n", Err: errors.New("limiter reply: short")}, false, false},
	}
	for _, tt := range tests {
		if replied, broken := outcome(tt.err); replied != tt.replied || broken != tt.broken {
			t.Errorf("outcome(%v) = %v, %v; want %v, %v", tt.err, replied, broken, tt.replied, tt.broken)
		}
	}
}

// fakeNode is a one-node "cluster" that speaks enough RESP for go-redis:
// it owns every slot and answers the limiter script with script.
type fakeNode struct {
	addr    string
	version string
	script  func(name string) string // the reply; "" drops the connection

	setup        atomic.Int64 // ns each HELLO and CLUSTER reply is held back: connection and slot-map cost
	infoFailures atomic.Int32 // INFO replies still to fail with LOADING
	paused       atomic.Bool  // commands get no reply; the connection drops once it is unpaused

	mu     sync.Mutex
	counts map[string]int
}

func newFakeNode(t *testing.T, version string, script func(name string) string) *fakeNode {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeNode{addr: ln.Addr().String(), version: version, script: script, counts: make(map[string]int)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeNode) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[name]
}

func (f *fakeNode) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	_, port, _ := net.SplitHostPort(f.addr)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		name := strings.ToLower(args[0])
		f.mu.Lock()
		f.counts[name]++
		f.mu.Unlock()
		if f.paused.Load() {
			for f.paused.Load() {
				time.Sleep(time.Millisecond)
			}
			return
		}
		var reply string
		if name == "hello" || name == "cluster" {
			time.Sleep(time.Duration(f.setup.Load()))
		}
		switch name {
		case "hello":
			reply = "-ERR unknown command 'HELLO'\r\n"
		case "client", "readonly":
			reply = "+OK\r\n"
		case "ping":
			reply = "+PONG\r\n"
		case "cluster":
			reply = "*1\r\n*3\r\n:0\r\n:16383\r\n*2\r\n$9\r\n127.0.0.1\r\n:" + port + "\r\n"
		case "command":
			reply = "*0\r\n"
		case "info":
			if f.infoFailures.Add(-1) >= 0 {
				reply = "-LOADING Redis is loading the dataset in memory\r\n"
				break
			}
			s := "# Server\r\nredis_version:" + f.version + "\r\n"
			reply = "$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n"
		case "script": // SCRIPT LOAD
			reply = "$40\r\n" + scriptSHA + "\r\n"
		case "evalsha", "eval":
			if reply = f.script(name); reply == "" {
				return
			}
		default:
			reply = "-ERR unknown command\r\n"
		}
		if _, err := io.WriteString(c, reply); err != nil {
			return
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil || n < 1 {
		return nil, fmt.Errorf("bad command header %q", line)
	}
	args := make([]string, n)
	for i := range args {
		if line, err = r.ReadString('\n'); err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		if err != nil {
			return nil, fmt.Errorf("bad bulk header %q", line)
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

var oneLimit = []Limit{{Key: GlobalKey("p", time.Second, Digest("k")), Level: "global", Enforce: true, Algorithm: config.GCRA, Rate: 10, Period: time.Second, Burst: 10}}

// allowedReply is the script's reply for one limit that allowed.
const allowedReply = "*12\r\n:1\r\n$0\r\n\r\n:0\r\n:9\r\n:100\r\n:0\r\n:0\r\n:1700000000000000\r\n:1\r\n:9\r\n:100\r\n:0\r\n"

func callFake(t *testing.T, c *Client) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Call(ctx, 1, oneLimit)
}

// TestNoResend guards RL-16: a node that drops the connection after reading
// the script call (it may have run) sees exactly one send.
func TestNoResend(t *testing.T) {
	f := newFakeNode(t, "7.2.4", func(string) string { return "" })
	c := NewClient([]string{f.addr}, nil, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()

	_, err := callFake(t, c)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindConn || e.Node != f.addr {
		t.Fatalf("Call error = %v, want a conn error on %s", err, f.addr)
	}
	if n := f.count("evalsha") + f.count("eval"); n != 1 {
		t.Errorf("script sent %d times, want 1", n)
	}
}

func TestNoScriptFallback(t *testing.T) {
	f := newFakeNode(t, "8.0.0", func(name string) string {
		if name == "evalsha" {
			return "-NOSCRIPT No matching script. Please use EVAL.\r\n"
		}
		return allowedReply
	})
	c := NewClient([]string{f.addr}, nil, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()

	for range 2 {
		res, err := callFake(t, c)
		if err != nil || !res.Allowed || res.NowUS != 1700000000000000 || len(res.Limits) != 1 || res.Limits[0].Remaining != 9 {
			t.Fatalf("Call = %+v, %v", res, err)
		}
	}
	if f.count("evalsha") != 2 || f.count("eval") != 2 || f.count("info") != 1 {
		t.Errorf("evalsha %d, eval %d, info %d; want 2, 2, 1", f.count("evalsha"), f.count("eval"), f.count("info"))
	}
	// Routing policies off: no COMMAND fetch inside a call's budget
	// (ADR-0025).
	if n := f.count("command"); n != 0 {
		t.Errorf("COMMAND sent %d times, want 0", n)
	}
}

// TestOldRedis checks RL-30: a node below Redis 7 fails every call as conn
// without being sent the script, and is logged once.
func TestOldRedis(t *testing.T) {
	f := newFakeNode(t, "6.2.14", func(string) string { return allowedReply })
	var logs bytes.Buffer
	c := NewClient([]string{f.addr}, nil, ratelimit.SystemClock, slog.New(slog.NewTextHandler(&logs, nil)))
	defer func() { _ = c.Close() }()

	for range 3 {
		_, err := callFake(t, c)
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindConn || !errors.Is(err, errOldRedis) {
			t.Fatalf("Call error = %v, want conn: %v", err, errOldRedis)
		}
	}
	if f.count("evalsha")+f.count("eval") != 0 || f.count("info") != 1 {
		t.Errorf("script sent %d times, info %d; want 0 and 1", f.count("evalsha")+f.count("eval"), f.count("info"))
	}
	if n := strings.Count(logs.String(), "older than 7"); n != 1 || !strings.Contains(logs.String(), "redis_version=6.2.14") {
		t.Errorf("logged %d times: %s", n, logs.String())
	}
}

func TestCallLimitCount(t *testing.T) {
	c := NewClient([]string{"127.0.0.1:1"}, nil, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()
	for _, n := range []int{0, MaxLimits + 1} {
		_, err := c.Call(context.Background(), 1, make([]Limit, n))
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindScript {
			t.Errorf("Call with %d limits: error %v, want a script error", n, err)
		}
	}
}

// TestClientBreaker: request calls whose connection breaks open the node's
// breaker after 3 in a row, and opening it reloads the slot map; script
// errors and timeouts never open it; the first answered PING closes it and
// refills the request pool, and a node that stops answering PINGs is opened
// by them (RL-22, ADR-0025).
func TestClientBreaker(t *testing.T) {
	const (
		allowed = iota
		scriptError
		drop
		slow
	)
	var mode atomic.Int32
	f := newFakeNode(t, "8.0.0", func(string) string {
		switch mode.Load() {
		case scriptError:
			return "-ERR Error running script: boom\r\n"
		case drop:
			return ""
		case slow:
			time.Sleep(20 * time.Millisecond)
		}
		return allowedReply
	})
	clock := ratelimit.NewFakeClock(time.Unix(1_700_000_000, 0))
	c := NewClient([]string{f.addr}, nil, clock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()
	c.stop() // the test runs the health check rounds itself
	budget := func() (Result, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		return c.Call(ctx, 1, oneLimit)
	}
	mustOpen := func(step string) {
		t.Helper()
		sent := f.count("evalsha")
		for range 3 {
			if _, err := callFake(t, c); !isKind(err, KindConn) || !errors.Is(err, errBreakerOpen) {
				t.Fatalf("%s: %v, want conn: %v", step, err, errBreakerOpen)
			}
		}
		if n := f.count("evalsha") - sent; n != 0 {
			t.Errorf("%s: script sent %d times while the breaker is open", step, n)
		}
	}
	mustClose := func(step string) {
		t.Helper()
		mode.Store(allowed)
		pings := f.count("ping")
		c.ping(t.Context())
		// Closing also opens a request connection in the background: a
		// second PING, through the request pool.
		for deadline := time.Now().Add(5 * time.Second); f.count("ping") < pings+2; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: no request connection opened on closing", step)
			}
		}
		if res, err := callFake(t, c); err != nil || !res.Allowed {
			t.Fatalf("%s: %+v, %v", step, res, err)
		}
	}

	mode.Store(scriptError)
	for i := range 10 {
		if _, err := callFake(t, c); !isKind(err, KindScript) {
			t.Fatalf("script error %d: %v", i+1, err)
		}
	}
	mode.Store(slow)
	for i := range 10 {
		if _, err := budget(); !isKind(err, KindTimeout) {
			t.Fatalf("timeout %d: %v", i+1, err)
		}
	}
	mode.Store(drop)
	for i := range 3 {
		if _, err := callFake(t, c); !isKind(err, KindConn) || errors.Is(err, errBreakerOpen) {
			t.Fatalf("broken connection %d: %v, want a conn error from the node", i+1, err)
		}
	}
	if n := f.count("evalsha"); n != 23 {
		t.Errorf("script sent %d times, want 23: script errors and timeouts never open the breaker", n)
	}
	slotLoads := f.count("cluster")
	mustOpen("after 3 broken connections")
	for deadline := time.Now().Add(5 * time.Second); f.count("cluster") == slotLoads; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("opening the breaker did not reload the slot map")
		}
	}
	clock.Advance(time.Hour)
	mustOpen("time alone")
	mustClose("after an answered PING")

	// The node stops answering: three health check rounds open it, and the
	// first answered PING closes it.
	f.paused.Store(true)
	for range 2 {
		c.ping(t.Context())
	}
	if _, err := budget(); !isKind(err, KindTimeout) {
		t.Fatalf("paused, before the third missed PING: %v, want a timeout", err)
	}
	c.ping(t.Context())
	mustOpen("after 3 missed PINGs")
	f.paused.Store(false)
	mustClose("after the node is back")
}

// TestPingerOptions: the health checks' clients have maintenance
// notifications off, so building one never resolves the node's host name
// (ADR-0025).
func TestPingerOptions(t *testing.T) {
	c := NewClient([]string{"redis-0:6379"}, nil, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()
	c.stop()
	if mode := c.pinger("redis-0:6379").Options().MaintNotificationsConfig.Mode; mode != maintnotifications.ModeDisabled {
		t.Errorf("maintenance notifications %q, want %q", mode, maintnotifications.ModeDisabled)
	}
}

// TestHealthChecks runs the background health checks on the system clock: a
// node that stops answering has its breaker opened within a few rounds, and
// closed within a round of answering again (ADR-0025).
func TestHealthChecks(t *testing.T) {
	f := newFakeNode(t, "8.0.0", func(string) string { return allowedReply })
	c := NewClient([]string{f.addr}, nil, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()
	budget := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		_, err := c.Call(ctx, 1, oneLimit)
		return err
	}
	if err := budget(); err != nil { // the client meets the node
		t.Fatal(err)
	}
	wait := func(step string, done func(error) bool) time.Duration {
		t.Helper()
		start := time.Now()
		for deadline := start.Add(5 * time.Second); !done(budget()); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not after 5s", step)
			}
		}
		return time.Since(start)
	}
	f.paused.Store(true)
	opened := wait("breaker open", func(err error) bool { return errors.Is(err, errBreakerOpen) })
	f.paused.Store(false)
	closed := wait("breaker closed", func(err error) bool { return err == nil })
	t.Logf("measure: breaker open %v after the node paused, closed %v after it came back", opened, closed)
	if opened > 2*time.Second || closed > time.Second {
		t.Errorf("open after %v, closed after %v; want about 300 ms and 100 ms", opened, closed)
	}
}

func isKind(err error, kind string) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// TestWarm: a warmed client's first call to a node, under a 5 ms budget, is
// one EVALSHA, decided by Redis, even when a connection and the slot map
// take far longer than the budget; an unwarmed client's first call times
// out (ADR-0024).
func TestWarm(t *testing.T) {
	const setup = 50 * time.Millisecond
	budget := func(c *Client) (Result, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		return c.Call(ctx, 1, oneLimit)
	}

	cold := newFakeNode(t, "8.0.0", func(string) string { return allowedReply })
	cold.setup.Store(int64(setup))
	c := NewClient([]string{cold.addr}, nil, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()
	if _, err := budget(c); !isKind(err, KindTimeout) {
		t.Errorf("unwarmed first call: %v, want a timeout", err)
	}

	f := newFakeNode(t, "8.0.0", func(string) string { return allowedReply })
	f.setup.Store(int64(setup))
	f.infoFailures.Store(1) // the first pass fails, the second succeeds
	var logs bytes.Buffer
	c = NewClient([]string{f.addr}, nil, ratelimit.SystemClock, slog.New(slog.NewTextHandler(&logs, nil)))
	defer func() { _ = c.Close() }()
	c.stop() // no health checks: the node's slow connection setup would miss them
	c.Warm(t.Context(), 10*time.Millisecond)
	if f.count("info") != 2 || f.count("script") != 1 || f.count("evalsha") != 0 {
		t.Errorf("warm-up: info %d, script %d, evalsha %d; want 2, 1, 0", f.count("info"), f.count("script"), f.count("evalsha"))
	}
	if strings.Count(logs.String(), "warm-up failed") != 1 || !strings.Contains(logs.String(), `msg="redis limiter warmed" nodes=1`) {
		t.Errorf("logs:\n%s", logs.String())
	}
	res, err := budget(c)
	if err != nil || !res.Allowed || res.NowUS != 1700000000000000 {
		t.Fatalf("warmed first call: %+v, %v; want Redis's decision", res, err)
	}
	if f.count("info") != 2 || f.count("evalsha") != 1 || f.count("eval") != 0 {
		t.Errorf("first call: info %d, evalsha %d, eval %d; want 2, 1, 0", f.count("info"), f.count("evalsha"), f.count("eval"))
	}
}

// TestWarmStops: warm-up against a cluster that never answers returns when
// its context ends, and never holds a node open (GW-24).
func TestWarmStops(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c := NewClient([]string{addr}, nil, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	c.Warm(ctx, 20*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Warm returned %v after its context ended", d-200*time.Millisecond)
	}
}
