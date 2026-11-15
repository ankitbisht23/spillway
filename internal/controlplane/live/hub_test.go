package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// The hub's tests run in synctest bubbles: time is a fake clock that
// moves only when every goroutine of the test is blocked, so "every 5 s"
// is exact, and the bubble fails the test if a goroutine is left blocked
// when it ends.

const requests = "sum by (code_class) (rate(gw_requests_total"

// settle advances the fake clock by d and waits until every goroutine is
// blocked again.
func settle(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

// checkClean checks that nothing of the streams is left: no connection,
// no running key, and, one interval later, no kept key.
func checkClean(t *testing.T, th *testHub) {
	t.Helper()
	synctest.Wait()
	if c, k := th.metric(t, "cp_stream_connections"), th.metric(t, "cp_stream_keys"); c != 0 || k != 0 {
		t.Errorf("after the streams: %v connections, %v keys", c, k)
	}
	settle(th.cfg.Interval + time.Second)
	if keys := th.keys(); len(keys) != 0 {
		t.Errorf("keys kept after an interval: %v", keys)
	}
}

// TestFanOut: N streams on one key make one Prometheus query per tick,
// and each stream gets every result (D6).
func TestFanOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		var streams []*served
		for i := range 20 {
			streams = append(streams, th.serve(t, "user-"+string(rune('a'+i%5)), "sub=overview.requests&range=15m", time.Time{}))
		}
		synctest.Wait()
		if n := th.prom.count(requests); n != 1 {
			t.Fatalf("after the first subscribers: %d queries, want 1", n)
		}
		settle(15 * time.Second) // ticks at 5, 10 and 15 s
		if n := th.prom.count(requests); n != 4 {
			t.Errorf("after 15 s: %d queries for 20 streams, want 4 (one per tick)", n)
		}
		for i, s := range streams {
			events, _ := s.w.events(t)
			if len(events) == 0 || events[0].name != "ready" {
				t.Fatalf("stream %d: first event %v, want ready", i, events)
			}
			if got := len(named(events, "result", "overview.requests")); got != 4 {
				t.Errorf("stream %d: %d results, want 4", i, got)
			}
		}
		if got := th.metric(t, "cp_stream_evaluations_total", "query", "overview.requests", "outcome", "ok"); got != 4 {
			t.Errorf("evaluations ok = %v, want 4", got)
		}
		for _, s := range streams {
			s.stop()
		}
		if got := th.metric(t, "cp_stream_closed_total", "reason", "client"); got != 20 {
			t.Errorf("closed by client = %v, want 20", got)
		}
		checkClean(t, th)
	})
}

// TestKeys: a key is the query plus the parameters it uses, so streams
// that differ only in a parameter a query ignores share its key, and
// others evaluate separately.
func TestKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		a := th.serve(t, "u", "sub=overview.requests&sub=overview.routes&route=r1", time.Time{})
		b := th.serve(t, "u", "sub=overview.requests&sub=overview.routes&route=orders", time.Time{})
		c := th.serve(t, "u", "sub=overview.routes&tenant=acme", time.Time{})
		synctest.Wait()
		want := []string{
			"overview.requests&route=orders&tenant=%2A&range=15m",
			"overview.requests&route=r1&tenant=%2A&range=15m",
			"overview.routes&tenant=%2A&range=15m",
			"overview.routes&tenant=acme&range=15m",
		}
		if got := th.keys(); !slices.Equal(got, want) {
			t.Errorf("keys = %v, want %v", got, want)
		}
		if n := th.prom.count(requests); n != 2 {
			t.Errorf("overview.requests: %d queries, want 2 (two routes)", n)
		}
		if n := th.prom.count(`sum by (route) (rate(gw_requests_total{}[15m]))`); n != 1 {
			t.Errorf("overview.routes for all tenants: %d queries, want 1 (shared)", n)
		}
		a.stop()
		b.stop()
		c.stop()
		checkClean(t, th)
	})
}

// TestStopAndResume: the last subscriber leaving stops the key's loop at
// once; one coming back within the interval gets the kept result at once
// and no evaluation runs until the interval since the last one has passed.
// After the interval without subscribers the key is dropped.
func TestStopAndResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		s := th.serve(t, "u", "sub=overview.requests", time.Time{})
		settle(2 * time.Second)
		s.stop()
		synctest.Wait()
		if k := th.metric(t, "cp_stream_keys"); k != 0 {
			t.Errorf("keys after the last subscriber left: %v", k)
		}
		settle(2 * time.Second) // t = 4 s
		s = th.serve(t, "u", "sub=overview.requests", time.Time{})
		synctest.Wait()
		events, _ := s.w.events(t)
		if got := named(events, "result"); len(got) != 1 || got[0].data["at"] != "2000-01-01T00:00:00Z" {
			t.Errorf("a subscriber back within 5 s got %v, want the kept result of t=0", got)
		}
		if n := th.prom.count(requests); n != 1 {
			t.Errorf("at 4 s: %d queries, want 1: no evaluation within 5 s of the last", n)
		}
		settle(time.Second) // t = 5 s
		if n := th.prom.count(requests); n != 2 {
			t.Errorf("at 5 s: %d queries, want 2", n)
		}
		s.stop()
		settle(6 * time.Second) // dropped 5 s after the leave
		if keys := th.keys(); len(keys) != 0 {
			t.Fatalf("keys = %v, want none", keys)
		}
		s = th.serve(t, "u", "sub=overview.requests", time.Time{})
		synctest.Wait()
		if n := th.prom.count(requests); n != 3 {
			t.Errorf("a new subscriber after the key was dropped: %d queries, want 3 (evaluated at once)", n)
		}
		s.stop()
		checkClean(t, th)
	})
}

// TestCoalescing: while a client is not taking events, a new result
// replaces the one it has not taken, so at most one is pending per
// subscription, and the client gets the latest when it reads again.
func TestCoalescing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, func(c *Config) { c.WriteTimeout = time.Minute })
		s := th.serve(t, "u", "sub=overview.requests", time.Time{})
		synctest.Wait() // ready and the t=0 result written
		s.w.setBlocked(true)
		settle(5 * time.Second)  // t=5 result: its write blocks
		settle(10 * time.Second) // t=10 and t=15 results: the t=15 one replaces t=10's
		s.w.setBlocked(false)
		synctest.Wait()
		events, _ := s.w.events(t)
		var at []string
		for _, e := range named(events, "result") {
			at = append(at, e.data["at"].(string)[17:19])
		}
		if want := []string{"00", "05", "15"}; !slices.Equal(at, want) {
			t.Errorf("results at seconds %v, want %v", at, want)
		}
		if got := th.metric(t, "cp_stream_coalesced_total"); got != 1 {
			t.Errorf("coalesced = %v, want 1", got)
		}
		s.stop()
		checkClean(t, th)
	})
}

// TestSlowClient: a client that does not take an event within the write
// timeout is disconnected, and its subscriptions end.
func TestSlowClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		s := th.serve(t, "u", "sub=overview.requests&sub=replicas", time.Time{})
		synctest.Wait()
		if !s.w.readCleared || !s.w.writesCleared {
			t.Errorf("the server's read and write deadlines were not cleared at the start")
		}
		s.w.setBlocked(true)
		start := time.Now()
		settle(5 * time.Second) // the t=5 results block
		select {
		case <-s.done:
			t.Fatal("disconnected before the write timeout")
		default:
		}
		settle(10 * time.Second)
		select {
		case <-s.done:
		default:
			t.Fatal("not disconnected after the write timeout")
		}
		if got := th.metric(t, "cp_stream_closed_total", "reason", "slow"); got != 1 {
			t.Errorf("closed slow = %v, want 1", got)
		}
		if waited := time.Since(start); waited < 14*time.Second {
			t.Errorf("disconnected after %v", waited)
		}
		s.cancel()
		checkClean(t, th)
	})
}

// TestLimits: at most PerSubject open streams per subject and Total in
// all; a closed stream frees its place.
func TestLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, false, func(c *Config) { c.PerSubject, c.Total = 2, 3 })
		a1 := th.serve(t, "a", "sub=replicas", time.Time{})
		a2 := th.serve(t, "a", "sub=replicas", time.Time{})
		synctest.Wait()
		q := url.Values{"sub": {"replicas"}}
		if _, err := th.Open(context.Background(), "a", q); !isLimit(err, "for this caller") {
			t.Errorf("a third stream for a: %v", err)
		}
		b := th.serve(t, "b", "sub=replicas", time.Time{})
		synctest.Wait()
		if _, err := th.Open(context.Background(), "c", q); !isLimit(err, "on this control plane") {
			t.Errorf("a fourth stream: %v", err)
		}
		if got := th.metric(t, "cp_stream_rejected_total", "reason", "limit"); got != 2 {
			t.Errorf("rejected for a limit = %v", got)
		}
		a1.stop()
		synctest.Wait()
		a3 := th.serve(t, "a", "sub=replicas", time.Time{}) // a1's place, for a and in all
		synctest.Wait()
		for _, s := range []*served{a2, b, a3} {
			s.stop()
		}
		checkClean(t, th)
	})
}

func isLimit(err error, msg string) bool {
	l, ok := errors.AsType[*LimitError](err)
	return ok && strings.Contains(l.Msg, msg)
}

// TestErrors: a failed evaluation is an error event with a fixed message,
// the stream stays open and recovers by itself; Prometheus not configured
// still serves the Postgres-backed subscriptions.
func TestErrors(t *testing.T) {
	for _, tt := range []struct {
		mode, code string
	}{
		{"refused", "prometheus_unavailable"},
		{"down", "prometheus_unavailable"},
		{"bad", "query_failed"},
		{"html", "query_failed"},
		{"huge", "query_failed"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				th := newTestHub(t, true, nil)
				th.prom.setMode(tt.mode)
				s := th.serve(t, "u", "sub=overview.routes&sub=replicas", time.Time{})
				settle(5 * time.Second)
				th.prom.setMode("")
				settle(5 * time.Second)
				events, _ := s.w.events(t)
				errs := named(events, "error", "overview.routes")
				if len(errs) != 2 || errs[0].data["code"] != tt.code {
					t.Fatalf("error events %v, want two with code %s", errs, tt.code)
				}
				if msg := errs[0].data["message"].(string); strings.Contains(msg, "prometheus.test") || strings.Contains(msg, "sum by") {
					t.Errorf("the message shows internals: %q", msg)
				}
				if got := named(events, "result", "overview.routes"); len(got) != 1 {
					t.Errorf("after recovery: %d results, want 1", len(got))
				}
				if got := named(events, "result", "replicas"); len(got) != 3 {
					t.Errorf("replicas: %d results, want 3: unaffected", len(got))
				}
				if c := strings.Count(th.log.String(), "live query failing"); c != 1 {
					t.Errorf("logged %d failures, want 1 per failure run:\n%s", c, th.log.String())
				}
				if got := th.metric(t, "cp_stream_evaluations_total", "query", "overview.routes", "outcome", "error"); got != 2 {
					t.Errorf("evaluations error = %v, want 2", got)
				}
				s.stop()
				checkClean(t, th)
			})
		})
	}
	t.Run("not configured", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			th := newTestHub(t, false, nil)
			s := th.serve(t, "u", "sub=overview.requests&sub=replicas&sub=throttling.heavy_hitters", time.Time{})
			synctest.Wait()
			events, _ := s.w.events(t)
			if e := named(events, "error", "overview.requests"); len(e) != 1 || e[0].data["code"] != "prometheus_not_configured" {
				t.Errorf("overview.requests: %v", e)
			}
			if len(named(events, "result", "replicas")) != 1 || len(named(events, "result", "throttling.heavy_hitters")) != 1 {
				t.Errorf("the Postgres-backed subscriptions: %v", events)
			}
			s.stop()
			checkClean(t, th)
		})
	})
	t.Run("postgres down", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			th := newTestHub(t, true, nil)
			s := th.serve(t, "u", "sub=replicas&sub=throttling.heavy_hitters&sub=overview.requests", time.Time{})
			synctest.Wait()
			th.pg.setDown(true)
			settle(5 * time.Second)
			th.pg.setDown(false)
			settle(5 * time.Second)
			events, _ := s.w.events(t)
			for _, sub := range []string{"replicas", "throttling.heavy_hitters"} {
				if e := named(events, "error", sub); len(e) != 1 || e[0].data["code"] != "unavailable" {
					t.Errorf("%s: %v", sub, e)
				}
				if r := named(events, "result", sub); len(r) != 2 {
					t.Errorf("%s: %d results, want 2", sub, len(r))
				}
			}
			if r := named(events, "result", "overview.requests"); len(r) != 3 {
				t.Errorf("overview.requests: %d results, want 3", len(r))
			}
			s.stop()
			checkClean(t, th)
		})
	})
}

// TestTokenExpiry: the stream ends at its token's exp with an end event,
// so it cannot outlive the token (ADR-0059).
func TestTokenExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		s := th.serve(t, "u", "sub=overview.requests", time.Now().Add(37*time.Second))
		settle(36 * time.Second)
		select {
		case <-s.done:
			t.Fatal("ended before the token's exp")
		default:
		}
		settle(time.Second)
		<-s.done
		events, keepalives := s.w.events(t)
		last := events[len(events)-1]
		if last.name != "end" || last.data["reason"] != "token_expired" {
			t.Errorf("last event %v, want end token_expired", last)
		}
		if keepalives != 2 {
			t.Errorf("%d keepalives in 37 s, want 2 (every 15 s)", keepalives)
		}
		if got := th.metric(t, "cp_stream_closed_total", "reason", "token_expired"); got != 1 {
			t.Errorf("closed token_expired = %v", got)
		}
		s.cancel()
		checkClean(t, th)
	})
}

// TestShutdown: Shutdown ends every stream with an end event and refuses
// new ones.
func TestShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		a := th.serve(t, "u", "sub=overview.requests", time.Time{})
		b := th.serve(t, "v", "sub=replicas", time.Time{})
		synctest.Wait()
		th.Shutdown()
		th.Shutdown() // twice is harmless
		<-a.done
		<-b.done
		for _, s := range []*served{a, b} {
			events, _ := s.w.events(t)
			if last := events[len(events)-1]; last.name != "end" || last.data["reason"] != "shutdown" {
				t.Errorf("last event %v, want end shutdown", last)
			}
		}
		if _, err := th.Open(context.Background(), "u", url.Values{"sub": {"replicas"}}); !isLimit(err, "shutting down") {
			t.Errorf("open after shutdown: %v", err)
		}
		if got := th.metric(t, "cp_stream_closed_total", "reason", "shutdown"); got != 2 {
			t.Errorf("closed shutdown = %v", got)
		}
		checkClean(t, th)
	})
}

// TestQueries checks the PromQL each catalogue query sends, with its
// parameters escaped into label matchers, and the range's start, end and
// step.
func TestQueries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		s := th.serve(t, "u", "sub=overview.requests&sub=overview.ratelimited&sub=overview.latency&sub=overview.routes&sub=overview.tenants"+
			"&sub=throttling.policies&sub=throttling.heavy_hitters&sub=replicas&route=r1&tenant=acme&range=1h&policy_id=7", time.Time{})
		synctest.Wait()
		want := []string{
			`sum by (code_class) (rate(gw_requests_total{route="r1",tenant="acme"}[2m]))`,
			`sum(rate(gw_requests_total{route="r1",tenant="acme",reason="ratelimited"}[2m])) / sum(rate(gw_requests_total{route="r1",tenant="acme"}[2m]))`,
			`histogram_quantile(0.5, sum by (le) (rate(gw_request_duration_seconds_bucket{route="r1",tenant="acme"}[2m])))`,
			`histogram_quantile(0.99, sum by (le) (rate(gw_request_duration_seconds_bucket{route="r1",tenant="acme"}[2m])))`,
			`sum by (route) (rate(gw_requests_total{tenant="acme"}[1h]))`,
			`sum by (route) (rate(gw_requests_total{tenant="acme",reason="ratelimited"}[1h]))`,
			`histogram_quantile(0.99, sum by (route, le) (rate(gw_request_duration_seconds_bucket{tenant="acme"}[1h])))`,
			`sum by (tenant) (rate(gw_requests_total{route="r1"}[1h]))`,
			`sum by (tenant) (rate(gw_requests_total{route="r1",reason="ratelimited"}[1h]))`,
			`histogram_quantile(0.99, sum by (tenant, le) (rate(gw_request_duration_seconds_bucket{route="r1"}[1h])))`,
			`sum by (policy, mode) (rate(gw_ratelimit_decisions_total{decision="deny"}[2m]))`,
		}
		got := th.prom.queries()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("queries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		for _, c := range th.prom.calls {
			if c.Get("endpoint") == "query_range" && (c.Get("step") != "60" || c.Get("start") != "946681200.000" || c.Get("end") != "946684800.000") {
				t.Errorf("range %v, want 1h back from 2000-01-01T00:00:00Z with 60 s steps", c)
			}
			if c.Get("endpoint") == "query" && c.Get("time") != "946684800.000" {
				t.Errorf("instant at %v", c.Get("time"))
			}
		}
		if th.pg.since != time.Now().Add(-time.Hour) || th.pg.policyID == nil || *th.pg.policyID != 7 {
			t.Errorf("heavy hitters since %v policy %v", th.pg.since, th.pg.policyID)
		}
		s.stop()
		checkClean(t, th)
	})
}

// TestData checks the data shapes against the spec's schemas: matrix,
// vector (top 10, merged by route), null for values that are not finite
// numbers, and every event (ADR-0059).
func TestData(t *testing.T) {
	// The embedded spec lacks the event schemas, which no operation
	// references (oapi-codegen prunes them), so read the source.
	spec, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	validate := func(t *testing.T, schema string, raw string) {
		t.Helper()
		ref := spec.Components.Schemas[schema]
		if ref == nil {
			t.Fatalf("no schema %s", schema)
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if err := ref.Value.VisitJSON(v); err != nil {
			t.Errorf("%s does not match the spec: %v\n%s", schema, err, raw)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		th := newTestHub(t, true, nil)
		s := th.serve(t, "u", "sub=overview.requests&sub=overview.latency&sub=overview.routes&sub=throttling.heavy_hitters&sub=replicas", time.Now().Add(time.Second))
		settle(time.Second)
		th.prom.setMode("down")
		s2 := th.serve(t, "u", "sub=overview.tenants", time.Time{})
		synctest.Wait()
		events, _ := s.w.events(t)
		e2, _ := s2.w.events(t)
		events = append(events, e2...)
		schemas := map[string]string{"ready": "StreamReady", "result": "StreamResult", "error": "StreamError", "end": "StreamEnd"}
		seen := map[string]bool{}
		for _, e := range events {
			validate(t, schemas[e.name], e.raw)
			seen[e.name] = true
		}
		if len(seen) != 4 {
			t.Errorf("event kinds seen: %v", seen)
		}
		req := named(events, "result", "overview.requests")[0].data["data"].(map[string]any)
		points := req["series"].([]any)[0].(map[string]any)["points"].([]any)
		if req["kind"] != "matrix" || req["step_s"] != 15.0 || points[0].([]any)[1] != 1.5 || points[1].([]any)[1] != nil {
			t.Errorf("overview.requests data %v", req)
		}
		lat := named(events, "result", "overview.latency")[0].data["data"].(map[string]any)["series"].([]any)
		if len(lat) != 2 || lat[0].(map[string]any)["labels"].(map[string]any)["quantile"] != "0.5" ||
			lat[1].(map[string]any)["labels"].(map[string]any)["quantile"] != "0.99" {
			t.Errorf("overview.latency series %v", lat)
		}
		routes := named(events, "result", "overview.routes")[0].data["data"].(map[string]any)
		series := routes["series"].([]any)
		if routes["kind"] != "vector" || len(series) != 30 {
			t.Fatalf("overview.routes: %d series, want 10 routes × 3 stats", len(series))
		}
		var order []string
		for _, s := range series {
			l := s.(map[string]any)["labels"].(map[string]any)
			if l["stat"] == "rps" {
				order = append(order, l["route"].(string))
			}
		}
		// route-03's rate is +Inf, which is no number: it sorts last and
		// falls out of the top 10.
		if want := []string{"route-00", "route-01", "route-02", "route-04", "route-05", "route-06", "route-07", "route-08", "route-09", "route-10"}; !slices.Equal(order, want) {
			t.Errorf("top routes %v, want %v", order, want)
		}
		s2.stop()
		s.cancel()
		checkClean(t, th)
	})
}
