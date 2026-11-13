package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

func TestParseFlags(t *testing.T) {
	o, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.url != "http://gateway-1:8000/anything/chaos" || o.metrics != "http://gateway-1:8001/metrics" || o.keys != 30 || o.rate != 20 ||
		o.duration != 40*time.Second || o.killAfter != 10*time.Second || o.maxWindow != 10*time.Second || o.maxBurst != time.Second || o.configVersion != 42 {
		t.Errorf("options = %+v", o)
	}
	for _, tt := range []struct {
		args []string
		want string // in the error
	}{
		{[]string{"-keys", "0"}, "-keys"},
		{[]string{"-rate", "0"}, "-rate"},
		{[]string{"-kill-after", "0s"}, "-kill-after"},
		{[]string{"-kill-after", "40s"}, "-kill-after"},
		{[]string{"-max-window", "0s"}, "-max-window"},
		{[]string{"-max-burst", "0s"}, "-max-burst"},
		{[]string{"-timeout", "0s"}, "-timeout"},
		{[]string{"extra"}, "unexpected arguments"},
	} {
		if _, err := parseFlags(tt.args, io.Discard); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: error = %v, want one about %s", tt.args, err, tt.want)
		}
	}
	if code := run(t.Context(), []string{"-keys", "0"}, nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("bad flags: exit code %d, want 2", code)
	}
	if code := run(t.Context(), []string{"-h"}, nil, io.Discard, io.Discard); code != 0 {
		t.Errorf("-h: exit code %d, want 0", code)
	}
}

// TestKeySlot: the slots Redis 8.10.3's CLUSTER KEYSLOT gives, hash tags
// included, and the CRC16 check value.
func TestKeySlot(t *testing.T) {
	if got := crc16("123456789"); got != 0x31c3 {
		t.Errorf("crc16(123456789) = %#x, want 0x31c3", got)
	}
	for key, want := range map[string]int{
		"foo":           12182,
		"bar":           5061,
		"foo{}{bar}":    8363, // an empty tag: the whole key
		"foo{{bar}}zap": 4015, // the tag is "{bar"
		"rl:v1:{k:ypeBEsobvcr6wjGzmiPcTQ}:chaos-primary:1000": 13822,
		"rl:v1:{k:vlmSqMa-bOw8WQnG8K6ziA}:chaos-primary:1000": 15125,
	} {
		if got := keySlot(key); got != want {
			t.Errorf("keySlot(%q) = %d, want %d", key, got, want)
		}
	}
}

// threeShards is the compose cluster's layout after redis-init.
var threeShards = []shard{
	{"redis-0", []goredis.SlotRange{{Start: 0, End: 5460}}},
	{"redis-1", []goredis.SlotRange{{Start: 5461, End: 10922}}},
	{"redis-2", []goredis.SlotRange{{Start: 10923, End: 16383}}},
}

func TestPickKeys(t *testing.T) {
	keys := pickKeys("run", threeShards, 0, 31)
	if len(keys) != 30 {
		t.Fatalf("%d keys, want 30", len(keys))
	}
	count := map[string]int{}
	for _, k := range keys {
		slot := keySlot(fmt.Sprintf("rl:v1:{k:%s}:chaos-primary:1000", rlredis.Digest(k.name)))
		s := slices.IndexFunc(threeShards, func(s shard) bool { return s.owns(slot) })
		count[threeShards[s].primary]++
		if k.killed != (s == 0) {
			t.Errorf("key %s in slot %d: killed = %v", k.name, slot, k.killed)
		}
	}
	if count["redis-0"] != 10 || count["redis-1"] != 10 || count["redis-2"] != 10 {
		t.Errorf("keys per primary = %v, want 10 each", count)
	}
}

func TestSchedule(t *testing.T) {
	a := schedule(3, 20, 2*time.Second)
	if len(a) != 3*20*2 {
		t.Fatalf("%d arrivals, want 120", len(a))
	}
	if !slices.IsSortedFunc(a, func(x, y arrival) int { return int(x.at - y.at) }) {
		t.Error("arrivals not sorted by due time")
	}
	// Key k starts at k·50/3 ms and sends every 50 ms.
	if a[0] != (arrival{0, 0}) || a[1].key != 1 || a[1].at != 50*time.Millisecond/3 || a[3] != (arrival{50 * time.Millisecond, 0}) {
		t.Errorf("first arrivals = %v", a[:4])
	}
}

func TestAnalyze(t *testing.T) {
	kill := time.UnixMicro(10_000_000)
	us := func(ms int64) int64 { return kill.UnixMicro() + ms*1000 }
	at := func(ms int64) time.Time { return time.UnixMicro(us(ms)) }
	keys := []key{{"survivor", false}, {"victim", true}}
	// step is one request: r is a Redis decision at ms from the kill, f a
	// failure-mode decision sent at ms and answered 5 ms later.
	type step struct {
		a arrival
		o outcome
	}
	r := func(k int, ms int64) step {
		return step{arrival{key: k}, outcome{sent: true, due: at(ms), done: at(ms + 1), status: 200, decided: true, decisionUS: us(ms)}}
	}
	f := func(k int, ms int64) step {
		return step{arrival{key: k}, outcome{sent: true, due: at(ms), done: at(ms + 5), status: 200}}
	}
	// ft is f with the gateway's Spillway-Redis-Error, kinds.
	ft := func(k int, ms int64, kinds string) step {
		s := f(k, ms)
		s.o.redisError = kinds
		return s
	}
	for _, tt := range []struct {
		name   string
		steps  []step
		pass   bool
		window float64 // max, ms
	}{
		{"pass", []step{r(0, -100), r(1, -100), r(0, 100), f(1, 100), f(1, 4000), r(0, 4200), r(1, 4200), r(1, 5000)}, true, 4200},
		{"surviving key fails over", []step{r(0, -100), r(1, -100), f(0, 100), f(1, 100), r(0, 4200), r(1, 4200)}, false, 4200},
		{"failure mode after the window", []step{r(0, -100), r(1, -100), f(1, 100), r(1, 4200), f(1, 4300)}, false, 4200},
		{"failure mode before the kill request", []step{r(0, -100), f(1, -310), r(1, 4200)}, false, 4200},
		{"failure mode before the recorded exit", []step{r(0, -100), f(1, -8), r(1, 4200)}, true, 4200}, // answered at -3 ms: Docker records the exit late
		{"never recovers", []step{r(0, -100), r(1, -100), f(1, 100), f(1, 20000)}, false, 0},
		{"window too long", []step{r(0, -100), r(1, -100), f(1, 100), r(1, 10500)}, false, 10500},
		// One burst of Redis timeouts outside the window is excused (ADR-0034).
		{"timeout before the kill request", []step{r(0, -100), ft(1, -5000, "timeout"), r(1, 4200)}, true, 4200},
		{"burst after the window", []step{r(0, -100), f(1, 100), r(1, 4200), ft(1, 9000, "timeout"), ft(1, 9500, "timeout,timeout"), ft(1, 9995, "timeout")}, true, 4200},
		{"two bursts", []step{r(0, -100), ft(1, -5000, "timeout"), r(1, 4200), ft(1, 9000, "timeout")}, false, 4200},
		{"burst too long", []step{r(0, -100), r(1, 4200), ft(1, 9000, "timeout"), ft(1, 9996, "timeout")}, false, 4200},
		{"conn error outside the window", []step{r(0, -100), r(1, 4200), ft(1, 9000, "conn")}, false, 4200},
		{"timeout and conn", []step{r(0, -100), r(1, 4200), ft(1, 9000, "timeout,conn")}, false, 4200},
		{"burst on a surviving key", []step{r(0, -100), ft(0, -5000, "timeout"), r(1, 4200)}, false, 4200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var a []arrival
			var o []outcome
			for _, s := range tt.steps {
				a, o = append(a, s.a), append(o, s.o)
			}
			rep := analyze(keys, a, o, at(-300), kill, 10*time.Second, time.Second)
			if rep.Pass != tt.pass || rep.WindowMS.Max != tt.window {
				t.Errorf("pass %v, window %v ms; want %v, %v ms; report %+v", rep.Pass, rep.WindowMS.Max, tt.pass, tt.window, rep)
			}
			if int64(len(rep.SurvivingSamples)) != rep.Surviving.FailureMode {
				t.Errorf("%d surviving samples for %d failure-mode decisions: %q", len(rep.SurvivingSamples), rep.Surviving.FailureMode, rep.SurvivingSamples)
			}
			lines := rep.lines(10*time.Second, time.Second)
			if failed := slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "FAIL") }); failed == tt.pass {
				t.Errorf("lines disagree with pass %v:\n%s", tt.pass, strings.Join(lines, "\n"))
			}
			if excused := strings.Contains(lines[2], ", excused (ADR-0034)"); excused != (tt.pass && rep.OutsideWindow > 0) {
				t.Errorf("killed-primary line says excused %v, want %v: %s", excused, tt.pass && rep.OutsideWindow > 0, lines[2])
			}
		})
	}
	// A request answered with neither a decision nor a 2xx.
	rep := analyze(keys, []arrival{{key: 1}, {key: 1}}, []outcome{{sent: true, status: 502}, {sent: true, done: at(4200), status: 200, decided: true, decisionUS: us(4200)}}, at(-300), kill, 10*time.Second, time.Second)
	if rep.Pass || rep.Killed.Other != 1 {
		t.Errorf("an unanswered request passed: %+v", rep)
	}
	// Too many dropped.
	rep = analyze(keys, []arrival{{key: 0}, {key: 1}}, []outcome{{}, {sent: true, status: 200, decided: true, decisionUS: us(100)}}, at(-300), kill, 10*time.Second, time.Second)
	if rep.Pass || rep.Requests.Dropped != 1 {
		t.Errorf("a run that sent half its schedule passed: %+v", rep)
	}
}

// fakeGateway answers as gateway-1 running test/chaos/primary.yaml in dev
// mode: every request is a Redis decision, except that a key on the killed
// primary gets fail_open decisions (2xx without the header) for down after
// the kill, and so do surviving keys when breakAll is set. Its admin port
// reports version 42.
type fakeGateway struct {
	down     time.Duration
	breakAll bool
	kill     atomic.Int64 // Unix µs, 0 before the kill
}

func (f *fakeGateway) start(t *testing.T) (url, metrics string) {
	traffic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UnixMicro()
		slot := keySlot(fmt.Sprintf("rl:v1:{k:%s}:chaos-primary:1000", rlredis.Digest(r.Header.Get(keyHeader))))
		k := f.kill.Load()
		if k == 0 || now < k || now >= k+f.down.Microseconds() || !threeShards[0].owns(slot) && !f.breakAll {
			w.Header().Set(decisionTimeHeader, strconv.FormatInt(now, 10))
		}
		_, _ = io.WriteString(w, "{}")
	}))
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "# TYPE gw_config_version gauge\ngw_config_version 42\n")
	}))
	t.Cleanup(traffic.Close)
	t.Cleanup(admin.Close)
	return traffic.URL, admin.URL
}

// killer is the script's side: when the sender's stderr asks for the kill,
// it notes the kill time and writes it to the sender's stdin.
type killer struct {
	mu     sync.Mutex
	stderr bytes.Buffer
	gw     *fakeGateway
	stdin  *io.PipeWriter
}

func (k *killer) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if bytes.HasPrefix(p, []byte("kill redis-0")) {
		now := time.Now()
		k.gw.kill.Store(now.UnixMicro())
		_, _ = fmt.Fprintln(k.stdin, now.UTC().Format(time.RFC3339Nano))
		_ = k.stdin.Close()
	}
	return k.stderr.Write(p)
}

func TestRun(t *testing.T) {
	for _, tt := range []struct {
		name     string
		down     time.Duration
		breakAll bool
		code     int
	}{
		{"pass", 300 * time.Millisecond, false, 0},
		{"window too long", 1200 * time.Millisecond, false, 1},
		{"surviving keys fail too", 300 * time.Millisecond, true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gw := &fakeGateway{down: tt.down, breakAll: tt.breakAll}
			url, metrics := gw.start(t)
			stdin, w := io.Pipe()
			k := &killer{gw: gw, stdin: w}
			o, err := parseFlags([]string{"-url", url, "-metrics", metrics, "-keys", "6", "-duration", "2s", "-kill-after", "500ms", "-max-window", "1s"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			o.shards = func(context.Context) ([]shard, error) { return threeShards, nil }
			var stdout bytes.Buffer
			code := runWith(t.Context(), o, stdin, &stdout, k)
			if code != tt.code {
				t.Errorf("exit code %d, want %d; stderr:\n%s", code, tt.code, k.stderr.String())
			}
			var r report
			if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
				t.Fatalf("report: %v\n%s", err, stdout.String())
			}
			if r.KilledNode != "redis-0" || r.Keys.Killed != 2 || r.Keys.Surviving != 4 || r.Requests.Scheduled != 6*20*2 || r.Recovered != 2 {
				t.Errorf("report = %+v", r)
			}
			// The window is the down time, give or take a request interval.
			if w := time.Duration(r.WindowMS.Max * float64(time.Millisecond)); w < tt.down || w > tt.down+100*time.Millisecond {
				t.Errorf("failover window %v, want about %v", w, tt.down)
			}
		})
	}
}

func TestReadKillTime(t *testing.T) {
	want := time.Date(2026, 11, 11, 10, 0, 0, 123456789, time.UTC)
	if got, err := readKillTime(strings.NewReader("2026-11-11T10:00:00.123456789Z\n")); err != nil || !got.Equal(want) {
		t.Errorf("readKillTime = %v, %v", got, err)
	}
	for _, in := range []string{"", "soon\n"} {
		if _, err := readKillTime(strings.NewReader(in)); err == nil {
			t.Errorf("readKillTime(%q) succeeded", in)
		}
	}
}
