package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/ratelimit"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// "chaos keys" is NFR-10's key-count sweep (ADR-0074 §D9): it creates
// limiter keys with the real limiter client and script, as tenant-level keys
// rl:v1:{t:<n>}:<policy>:300000:<digest> with numeric tenants 1 … N, first
// GCRA keys (policy 9001, {rate 1, period 5m, burst 1}), then, after they
// are removed, sliding-window keys (policy 9002, {rate 1, period 5m}). After
// each step it reads used_memory (INFO memory) and DBSIZE of every primary,
// and per-key memory is Σ Δused_memory ÷ Σ Δ DBSIZE from the sweep's first
// reading. A step fails on any error or deny (a fresh key always allows, so
// the sender's count is the keys created) or when Σ Δ DBSIZE differs from
// the keys created by more than 1%; the sweep passes when every step does
// and the last step's per-key memory is at most -max-bytes. The test's keys
// are removed with SCAN and UNLINK (never KEYS) after each sweep and on
// every exit. It prints its report as JSON on stdout and progress and one
// PASS/FAIL line per kind on stderr, and exits 0 on PASS, 1 on FAIL and 2
// on bad flags.
//
//	chaos keys [-steps 250000,500000,1000000] [-max-bytes 200] [-cleanup]

// keyKind is one sweep's key kind.
type keyKind struct {
	Name      string `json:"kind"`
	Policy    string `json:"policy"`
	algorithm string
	burst     int64
}

var keyKinds = []keyKind{
	{Name: "gcra", Policy: "9001", algorithm: config.GCRA, burst: 1},
	{Name: "sliding_window", Policy: "9002", algorithm: config.SlidingWindow},
}

const (
	keysPeriod = 5 * time.Minute
	// keysPattern matches the test's keys of both kinds, and only those
	// (policies 9001 and 9002 are reserved for it, ADR-0074).
	keysPattern = "rl:v1:*:900[12]:*"
	unlinkBatch = 1000
)

// testKey is the Redis key of tenant n for kind k.
func testKey(k keyKind, n int) string {
	t := strconv.Itoa(n)
	return rlredis.TenantKey(t, k.Policy, keysPeriod, rlredis.Digest(t))
}

type keysOptions struct {
	seeds       []string
	steps       []int
	inFlight    int
	timeout     time.Duration
	settle      time.Duration
	maxBytes    float64
	maxCountOff float64 // largest |Σ Δ DBSIZE − created| ÷ created
	cleanupOnly bool
}

func parseKeysFlags(args []string, stderr io.Writer) (keysOptions, error) {
	o := keysOptions{}
	var seeds, steps string
	fs := flag.NewFlagSet("chaos keys", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&seeds, "redis", "redis-0:6379,redis-1:6379,redis-2:6379,redis-3:6379,redis-4:6379,redis-5:6379", "Redis Cluster seed nodes")
	fs.StringVar(&steps, "steps", "250000,500000,1000000", "key counts to read memory at, increasing; the last is judged")
	fs.IntVar(&o.inFlight, "in-flight", 256, "most script calls in flight")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-call timeout")
	fs.DurationVar(&o.settle, "settle", 2*time.Second, "wait after a step's last call before reading")
	fs.Float64Var(&o.maxBytes, "max-bytes", 200, "most Redis memory per key at the last step (NFR-10)")
	fs.Float64Var(&o.maxCountOff, "max-count-off", 0.01, "most relative difference between Σ Δ DBSIZE and the keys created")
	fs.BoolVar(&o.cleanupOnly, "cleanup", false, "only remove the test's keys, then exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.seeds = strings.Split(seeds, ",")
	var err error
	prev := 0
	for s := range strings.SplitSeq(steps, ",") {
		n, perr := strconv.Atoi(strings.TrimSpace(s))
		if perr != nil || n <= prev {
			err = fmt.Errorf("-steps: %q is not a list of increasing positive counts", steps)
			break
		}
		o.steps = append(o.steps, n)
		prev = n
	}
	switch {
	case err != nil:
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case o.inFlight < 1:
		err = errors.New("-in-flight: must be positive")
	case o.timeout <= 0 || o.settle < 0:
		err = errors.New("-timeout must be positive and -settle not negative")
	case o.maxBytes <= 0 || o.maxCountOff < 0:
		err = errors.New("-max-bytes must be positive and -max-count-off not negative")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos keys:", err)
	}
	return o, err
}

// nodeReading is one node's memory and key count.
type nodeReading struct {
	Primary    bool  `json:"primary"`
	UsedMemory int64 `json:"used_memory"`
	DBSize     int64 `json:"dbsize"`
}

// keysEnv is what the sweep needs from Redis; tests replace it.
type keysEnv struct {
	// readings reads every node, by address.
	readings func(ctx context.Context) (map[string]nodeReading, error)
	// create makes tenant n's key of kind k with one script call.
	create func(ctx context.Context, k keyKind, n int) (allowed bool, err error)
	// cleanup removes the test's keys from every primary.
	cleanup func(ctx context.Context) (int64, error)
	sleep   func(ctx context.Context, d time.Duration)
}

// keysStep is one step of a sweep.
type keysStep struct {
	Keys           int     `json:"keys"`    // the step's target
	Created        int64   `json:"created"` // allowed answers so far
	Denied         int64   `json:"denied"`
	Errors         int64   `json:"errors"`
	FirstError     string  `json:"first_error,omitempty"`
	DBSizeDelta    int64   `json:"dbsize_delta"`      // Σ primaries
	UsedMemDelta   int64   `json:"used_memory_delta"` // Σ primaries, bytes
	BytesPerKey    float64 `json:"bytes_per_key"`
	CountOff       float64 `json:"count_off"` // |Σ Δ DBSIZE − created| ÷ created
	ReplicaDBSize  int64   `json:"replicas_dbsize_delta"`
	ReplicaUsedMem int64   `json:"replicas_used_memory_delta"`
	Seconds        float64 `json:"seconds"` // the step's calls
	OK             bool    `json:"ok"`
}

// keysSweep is one kind's sweep.
type keysSweep struct {
	keyKind
	Base    map[string]nodeReading `json:"base"`
	Steps   []keysStep             `json:"steps"`
	Removed int64                  `json:"removed"`
	Pass    bool                   `json:"pass"`
}

type keysReport struct {
	MaxBytes float64     `json:"max_bytes"`
	Sweeps   []keysSweep `json:"sweeps"`
	Pass     bool        `json:"pass"`
}

func runKeys(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseKeysFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	env, closeEnv, err := redisKeysEnv(ctx, o)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos keys:", err)
		return 1
	}
	defer closeEnv()
	return runKeysWith(ctx, o, env, stdout, stderr)
}

func runKeysWith(ctx context.Context, o keysOptions, env keysEnv, stdout, stderr io.Writer) int {
	logf := func(format string, a ...any) { _, _ = fmt.Fprintf(stderr, format+"\n", a...) }
	// The keys go on every exit, also when ctx ends.
	clean := func() int64 {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		n, err := env.cleanup(cctx)
		if err != nil {
			logf("chaos keys: removing the test's keys: %v", err)
		}
		return n
	}
	if o.cleanupOnly {
		logf("chaos keys: removed %d test keys", clean())
		return 0
	}
	if n := clean(); n > 0 {
		logf("chaos keys: removed %d test keys left from an earlier run", n)
	}
	rep := keysReport{MaxBytes: o.maxBytes, Pass: true}
	for _, k := range keyKinds {
		s, err := sweep(ctx, o, env, k, logf)
		s.Removed = clean()
		rep.Sweeps = append(rep.Sweeps, s)
		if err != nil {
			logf("chaos keys: %s: %v", k.Name, err)
			rep.Pass = false
			break
		}
		rep.Pass = rep.Pass && s.Pass
		if ctx.Err() != nil {
			rep.Pass = false
			break
		}
	}
	for _, l := range rep.lines(o) {
		logf("%s", l)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	if !rep.Pass {
		return 1
	}
	return 0
}

// sweep creates k's keys step by step and reads the nodes after each step.
func sweep(ctx context.Context, o keysOptions, env keysEnv, k keyKind, logf func(string, ...any)) (keysSweep, error) {
	s := keysSweep{keyKind: k}
	base, err := env.readings(ctx)
	if err != nil {
		return s, fmt.Errorf("reading the nodes: %w", err)
	}
	s.Base = base
	var created, denied, errs atomic.Int64
	var firstErr atomic.Value
	next := 1
	for _, target := range o.steps {
		start := time.Now()
		ids := make(chan int)
		var wg sync.WaitGroup
		for range o.inFlight {
			wg.Go(func() {
				for n := range ids {
					cctx, cancel := context.WithTimeout(ctx, o.timeout)
					allowed, err := env.create(cctx, k, n)
					cancel()
					switch {
					case err != nil:
						errs.Add(1)
						firstErr.CompareAndSwap(nil, err.Error())
					case !allowed:
						denied.Add(1)
					default:
						created.Add(1)
					}
				}
			})
		}
	send:
		for ; next <= target; next++ {
			select {
			case ids <- next:
			case <-ctx.Done():
				break send
			}
		}
		close(ids)
		wg.Wait()
		st := keysStep{Keys: target, Seconds: time.Since(start).Seconds()}
		env.sleep(ctx, o.settle)
		now, err := env.readings(ctx)
		if err != nil {
			return s, fmt.Errorf("reading the nodes after %d keys: %w", target, err)
		}
		for addr, b := range base {
			n, ok := now[addr]
			if !ok || n.Primary != b.Primary {
				return s, fmt.Errorf("node %s left or changed role during the sweep", addr)
			}
			if b.Primary {
				st.DBSizeDelta += n.DBSize - b.DBSize
				st.UsedMemDelta += n.UsedMemory - b.UsedMemory
			} else {
				st.ReplicaDBSize += n.DBSize - b.DBSize
				st.ReplicaUsedMem += n.UsedMemory - b.UsedMemory
			}
		}
		st.Created, st.Denied, st.Errors = created.Load(), denied.Load(), errs.Load()
		if e, ok := firstErr.Load().(string); ok {
			st.FirstError = e
		}
		if st.DBSizeDelta > 0 {
			st.BytesPerKey = float64(st.UsedMemDelta) / float64(st.DBSizeDelta)
		}
		if st.Created > 0 {
			st.CountOff = math.Abs(float64(st.DBSizeDelta-st.Created)) / float64(st.Created)
		}
		st.OK = st.Denied == 0 && st.Errors == 0 && st.Created == int64(target) && st.DBSizeDelta > 0 && st.CountOff <= o.maxCountOff
		s.Steps = append(s.Steps, st)
		logf("      %s at %d keys: %.1f B per key (Σ Δused_memory %d B over Σ Δ DBSIZE %d; created %d, denied %d, errors %d; replicas Δ DBSIZE %d, Δused_memory %d B); calls took %.1f s",
			k.Name, target, st.BytesPerKey, st.UsedMemDelta, st.DBSizeDelta, st.Created, st.Denied, st.Errors, st.ReplicaDBSize, st.ReplicaUsedMem, st.Seconds)
		if !st.OK || ctx.Err() != nil {
			return s, nil
		}
	}
	last := s.Steps[len(s.Steps)-1]
	s.Pass = len(s.Steps) == len(o.steps) && last.BytesPerKey <= o.maxBytes
	return s, nil
}

// lines are the report's checks, one PASS/FAIL line per kind.
func (r keysReport) lines(o keysOptions) []string {
	target := o.steps[len(o.steps)-1]
	var out []string
	for _, k := range keyKinds {
		check := fmt.Sprintf("NFR-10 %s keys: <= %.0f B per key at %d", k.Name, o.maxBytes, target)
		i := -1
		for j, s := range r.Sweeps {
			if s.Name == k.Name {
				i = j
			}
		}
		if i < 0 || len(r.Sweeps[i].Steps) == 0 {
			out = append(out, fmt.Sprintf("%-4s  %-44s %s", "FAIL", check, "not measured"))
			continue
		}
		s := r.Sweeps[i]
		st := s.Steps[len(s.Steps)-1]
		measured := fmt.Sprintf("%.1f B per key at %d keys (Σ Δused_memory %d B ÷ Σ Δ DBSIZE %d); created %d, denied %d, errors %d, DBSIZE off by %.2f%% (<= %.0f%%)",
			st.BytesPerKey, st.Keys, st.UsedMemDelta, st.DBSizeDelta, st.Created, st.Denied, st.Errors, st.CountOff*100, o.maxCountOff*100)
		if st.FirstError != "" {
			measured += "; first error: " + st.FirstError
		}
		if !st.OK {
			measured += fmt.Sprintf("; step %d failed", st.Keys)
		}
		verdict := "FAIL"
		if s.Pass {
			verdict = "PASS"
		}
		out = append(out, fmt.Sprintf("%-4s  %-44s %s", verdict, check, measured))
	}
	return out
}

// redisKeysEnv is keysEnv on the compose Redis Cluster: the limiter client
// for the script calls, a client per node for the readings, and the cluster
// client's primaries for the cleanup.
func redisKeysEnv(ctx context.Context, o keysOptions) (keysEnv, func(), error) {
	shards, err := allNodes(ctx, o.seeds)
	if err != nil {
		return keysEnv{}, nil, fmt.Errorf("reading CLUSTER SHARDS: %w", err)
	}
	nodes := map[string]*goredis.Client{}
	for addr := range shards {
		nodes[addr] = goredis.NewClient(&goredis.Options{Addr: addr, MaxRetries: -1})
	}
	cluster := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: o.seeds})
	limiter := rlredis.NewClient(o.seeds, nil, rlredis.Auth{}, ratelimit.SystemClock, slog.New(slog.DiscardHandler))
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	limiter.Warm(wctx, time.Second)
	cancel()
	closeAll := func() {
		_ = limiter.Close()
		_ = cluster.Close()
		for _, c := range nodes {
			_ = c.Close()
		}
	}
	env := keysEnv{
		readings: func(ctx context.Context) (map[string]nodeReading, error) {
			out := map[string]nodeReading{}
			for addr, c := range nodes {
				info, err := c.Info(ctx, "memory", "replication").Result()
				if err != nil {
					return nil, fmt.Errorf("%s: INFO: %w", addr, err)
				}
				size, err := c.DBSize(ctx).Result()
				if err != nil {
					return nil, fmt.Errorf("%s: DBSIZE: %w", addr, err)
				}
				r, err := parseNodeInfo(info)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", addr, err)
				}
				r.DBSize = size
				out[addr] = r
			}
			return out, nil
		},
		create: func(ctx context.Context, k keyKind, n int) (bool, error) {
			res, err := limiter.Call(ctx, 1, []rlredis.Limit{{
				Key: testKey(k, n), Level: "tenant", Enforce: true, Algorithm: k.algorithm,
				Rate: 1, Period: keysPeriod, Burst: k.burst,
			}})
			return res.Allowed, err
		},
		cleanup: func(ctx context.Context) (int64, error) {
			var removed atomic.Int64
			err := cluster.ForEachMaster(ctx, func(ctx context.Context, c *goredis.Client) error {
				n, err := unlinkMatching(ctx, c, keysPattern)
				removed.Add(n)
				return err
			})
			return removed.Load(), err
		},
		sleep: func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
			}
		},
	}
	return env, closeAll, nil
}

// allNodes returns every node of the cluster, primaries and replicas, by
// host:port, and whether it is a primary.
func allNodes(ctx context.Context, seeds []string) (map[string]bool, error) {
	var err error
	for _, addr := range seeds {
		rdb := goredis.NewClient(&goredis.Options{Addr: addr, MaxRetries: -1})
		var cs []goredis.ClusterShard
		cs, err = rdb.ClusterShards(ctx).Result()
		_ = rdb.Close()
		if err != nil {
			continue
		}
		out := map[string]bool{}
		for _, s := range cs {
			for _, n := range s.Nodes {
				if n.Health == "online" {
					out[fmt.Sprintf("%s:%d", n.Hostname, n.Port)] = n.Role == "master"
				}
			}
		}
		return out, nil
	}
	return nil, err
}

// parseNodeInfo reads used_memory and the role from INFO memory and
// replication.
func parseNodeInfo(info string) (nodeReading, error) {
	var r nodeReading
	var seenMem, seenRole bool
	for line := range strings.SplitSeq(info, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch k {
		case "used_memory":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return r, fmt.Errorf("used_memory %q: %w", v, err)
			}
			r.UsedMemory, seenMem = n, true
		case "role":
			r.Primary, seenRole = v == "master", true
		}
	}
	if !seenMem || !seenRole {
		return r, errors.New("INFO has no used_memory or role")
	}
	return r, nil
}

// unlinkMatching removes the keys matching pattern on one node with SCAN
// and UNLINK in batches of unlinkBatch, never KEYS. A batch is a pipeline of
// one-key UNLINKs, since its keys are in many slots (CROSSSLOT).
func unlinkMatching(ctx context.Context, c *goredis.Client, pattern string) (int64, error) {
	var removed int64
	var cursor uint64
	batch := make([]string, 0, unlinkBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		cmds, err := c.Pipelined(ctx, func(p goredis.Pipeliner) error {
			for _, k := range batch {
				p.Unlink(ctx, k)
			}
			return nil
		})
		for _, cmd := range cmds {
			if ic, ok := cmd.(*goredis.IntCmd); ok {
				removed += ic.Val()
			}
		}
		batch = batch[:0]
		return err
	}
	for {
		keys, next, err := c.Scan(ctx, cursor, pattern, unlinkBatch).Result()
		if err != nil {
			return removed, err
		}
		for _, k := range keys {
			batch = append(batch, k)
			if len(batch) == unlinkBatch {
				if err := flush(); err != nil {
					return removed, err
				}
			}
		}
		if cursor = next; cursor == 0 {
			return removed, flush()
		}
	}
}
