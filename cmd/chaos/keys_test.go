package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedis is a cluster of three primaries with a replica each, for the
// sweep: every created key costs perKey bytes on its primary and replica.
type fakeRedis struct {
	mu       sync.Mutex
	perKey   int64
	keys     map[string]int64 // node → test keys
	extra    int64            // other keys that appear on redis-0 per created key, ×1000
	deny     int              // tenant whose call is denied; 0 none
	fail     int              // tenant whose call errors; 0 none
	created  int64
	cleanups int
	calls    map[string]int // kind → calls
}

func newFakeRedis(perKey int64) *fakeRedis {
	return &fakeRedis{perKey: perKey, keys: map[string]int64{}, calls: map[string]int{}}
}

var fakePrimaries = []string{"redis-0:6379", "redis-1:6379", "redis-2:6379"}

func (f *fakeRedis) env() keysEnv {
	return keysEnv{
		readings: func(context.Context) (map[string]nodeReading, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			out := map[string]nodeReading{}
			for i, p := range fakePrimaries {
				n := f.keys[p]
				if i == 0 {
					n += f.created * f.extra / 1000
				}
				// 1 MB of other data on each node, and 100 other keys.
				out[p] = nodeReading{Primary: true, UsedMemory: 1<<20 + n*f.perKey, DBSize: 100 + n}
				r := strings.Replace(p, "redis-", "redis-r", 1)
				out[r] = nodeReading{UsedMemory: 1<<20 + f.keys[p]*f.perKey, DBSize: 100 + f.keys[p]}
			}
			return out, nil
		},
		create: func(_ context.Context, k keyKind, n int) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls[k.Name]++
			switch n {
			case f.fail:
				return false, errors.New("timeout")
			case f.deny:
				return false, nil
			}
			f.keys[fakePrimaries[n%3]]++
			f.created++
			return true, nil
		},
		cleanup: func(context.Context) (int64, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.cleanups++
			var n int64
			for k, v := range f.keys {
				n += v
				delete(f.keys, k)
			}
			f.created = 0
			return n, nil
		},
		sleep: func(context.Context, time.Duration) {},
	}
}

func keysRun(t *testing.T, f *fakeRedis, args ...string) (int, keysReport, string) {
	t.Helper()
	o, err := parseKeysFlags(append([]string{"-steps", "2500,5000,10000", "-in-flight", "8"}, args...), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runKeysWith(t.Context(), o, f.env(), &stdout, &stderr)
	var rep keysReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("report: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return code, rep, stderr.String()
}

func TestKeysPass(t *testing.T) {
	f := newFakeRedis(150)
	code, rep, lines := keysRun(t, f)
	if code != 0 || !rep.Pass || len(rep.Sweeps) != 2 {
		t.Fatalf("code %d, report %+v\n%s", code, rep, lines)
	}
	for i, kind := range []string{"gcra", "sliding_window"} {
		s := rep.Sweeps[i]
		if s.Name != kind || !s.Pass || len(s.Steps) != 3 || s.Removed != 10000 || f.calls[kind] != 10000 {
			t.Errorf("%s sweep: %+v, %d calls", kind, s, f.calls[kind])
		}
		last := s.Steps[2]
		if last.Keys != 10000 || last.Created != 10000 || last.DBSizeDelta != 10000 || last.UsedMemDelta != 1_500_000 ||
			last.BytesPerKey != 150 || last.ReplicaDBSize != 10000 || !last.OK {
			t.Errorf("%s last step: %+v", kind, last)
		}
		if s.Base["redis-0:6379"].DBSize != 100 || !s.Base["redis-0:6379"].Primary || s.Base["redis-r0:6379"].Primary {
			t.Errorf("%s base readings: %+v", kind, s.Base)
		}
	}
	// Before the sweeps, then after each.
	if f.cleanups != 3 {
		t.Errorf("%d cleanups, want 3", f.cleanups)
	}
	for _, want := range []string{
		"PASS  NFR-10 gcra keys: <= 200 B per key at 10000",
		"PASS  NFR-10 sliding_window keys: <= 200 B per key at 10000",
		"150.0 B per key at 10000 keys (Σ Δused_memory 1500000 B ÷ Σ Δ DBSIZE 10000); created 10000, denied 0, errors 0, DBSIZE off by 0.00% (<= 1%)",
		"gcra at 2500 keys: 150.0 B per key",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("output lacks %q:\n%s", want, lines)
		}
	}
}

func TestKeysFail(t *testing.T) {
	for _, tt := range []struct {
		name   string
		f      func() *fakeRedis
		want   string
		gcraOK bool
	}{
		{"too much memory per key", func() *fakeRedis { return newFakeRedis(201) }, "FAIL  NFR-10 gcra keys", false},
		{"a deny", func() *fakeRedis { f := newFakeRedis(100); f.deny = 3000; return f }, "denied 1", false},
		{"an error", func() *fakeRedis { f := newFakeRedis(100); f.fail = 7; return f }, "first error: timeout", false},
		// 2% more keys than created appear, so the per-key figure would
		// blend in other keys.
		{"other keys blend in", func() *fakeRedis { f := newFakeRedis(100); f.extra = 20; return f }, "DBSIZE off by 2.00%", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.f()
			code, rep, lines := keysRun(t, f)
			if code != 1 || rep.Pass || !strings.Contains(lines, tt.want) {
				t.Errorf("code %d, pass %v; want 1, FAIL with %q:\n%s", code, rep.Pass, tt.want, lines)
			}
			if f.cleanups != 3 || len(f.keys) != 0 {
				t.Errorf("%d cleanups, %d nodes with keys left; want 3 and none", f.cleanups, len(f.keys))
			}
		})
	}
	t.Run("a failing step stops its sweep", func(t *testing.T) {
		f := newFakeRedis(100)
		f.deny = 3000
		_, rep, _ := keysRun(t, f)
		if len(rep.Sweeps[0].Steps) != 2 || rep.Sweeps[0].Steps[1].OK || !rep.Sweeps[0].Steps[0].OK {
			t.Errorf("gcra steps: %+v, want the second to fail and the sweep to stop", rep.Sweeps[0].Steps)
		}
	})
}

func TestKeysCleanupOnly(t *testing.T) {
	f := newFakeRedis(100)
	f.keys["redis-1:6379"] = 42
	o, err := parseKeysFlags([]string{"-cleanup"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runKeysWith(t.Context(), o, f.env(), &stdout, &stderr); code != 0 || f.cleanups != 1 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "removed 42 test keys") {
		t.Errorf("code %d, %d cleanups, stdout %q, stderr %q", code, f.cleanups, stdout.String(), stderr.String())
	}
}

func TestKeysFlags(t *testing.T) {
	o, err := parseKeysFlags(nil, io.Discard)
	if err != nil || len(o.steps) != 3 || o.steps[2] != 1_000_000 || o.inFlight != 256 || o.timeout != 5*time.Second ||
		o.settle != 2*time.Second || o.maxBytes != 200 || o.maxCountOff != 0.01 || len(o.seeds) != 6 {
		t.Errorf("defaults %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{"-steps", "5,4"}, {"-steps", "0"}, {"-steps", "a"}, {"-in-flight", "0"}, {"-timeout", "0s"},
		{"-max-bytes", "0"}, {"-max-count-off", "-1"}, {"extra"},
	} {
		if _, err := parseKeysFlags(bad, io.Discard); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestTestKeyAndInfo(t *testing.T) {
	k := testKey(keyKinds[0], 7)
	if !strings.HasPrefix(k, "rl:v1:{t:7}:9001:300000:") || len(k) != len("rl:v1:{t:7}:9001:300000:")+22 {
		t.Errorf("testKey = %q", k)
	}
	if !strings.HasPrefix(testKey(keyKinds[1], 1000000), "rl:v1:{t:1000000}:9002:300000:") {
		t.Errorf("sliding-window key = %q", testKey(keyKinds[1], 1000000))
	}
	r, err := parseNodeInfo("# Memory\r\nused_memory:123456\r\nused_memory_human:120K\r\n# Replication\r\nrole:master\r\n")
	if err != nil || r.UsedMemory != 123456 || !r.Primary {
		t.Errorf("parseNodeInfo = %+v, %v", r, err)
	}
	if r, err := parseNodeInfo("used_memory:1\nrole:slave\n"); err != nil || r.Primary {
		t.Errorf("replica: %+v, %v", r, err)
	}
	for _, bad := range []string{"role:master\n", "used_memory:1\n", "used_memory:x\nrole:master\n"} {
		if _, err := parseNodeInfo(bad); err == nil {
			t.Errorf("parseNodeInfo(%q): no error", bad)
		}
	}
}
