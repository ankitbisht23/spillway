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
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

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

// fakeNode is a one-node "cluster" that speaks enough RESP for go-redis:
// it owns every slot and answers the limiter script with script.
type fakeNode struct {
	addr    string
	version string
	script  func(name string) string // the reply; "" drops the connection

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
		var reply string
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
			s := "# Server\r\nredis_version:" + f.version + "\r\n"
			reply = "$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n"
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
	c := NewClient([]string{f.addr}, nil, slog.New(slog.DiscardHandler))
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
	c := NewClient([]string{f.addr}, nil, slog.New(slog.DiscardHandler))
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
}

// TestOldRedis checks RL-30: a node below Redis 7 fails every call as conn
// without being sent the script, and is logged once.
func TestOldRedis(t *testing.T) {
	f := newFakeNode(t, "6.2.14", func(string) string { return allowedReply })
	var logs bytes.Buffer
	c := NewClient([]string{f.addr}, nil, slog.New(slog.NewTextHandler(&logs, nil)))
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
	c := NewClient([]string{"127.0.0.1:1"}, nil, slog.New(slog.DiscardHandler))
	defer func() { _ = c.Close() }()
	for _, n := range []int{0, MaxLimits + 1} {
		_, err := c.Call(context.Background(), 1, make([]Limit, n))
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindScript {
			t.Errorf("Call with %d limits: error %v, want a script error", n, err)
		}
	}
}
