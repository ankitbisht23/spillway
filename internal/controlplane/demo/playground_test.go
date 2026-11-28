package demo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// fakeKeys is a KeyStore: a Playground consumer per tenant, and keys
// whose secrets name the tenant and a sequence number.
type fakeKeys struct {
	mu      sync.Mutex
	missing bool
	issued  []issued
}

type issued struct {
	consumer int64
	expires  *time.Time
	key      string
}

func (f *fakeKeys) PlaygroundConsumer(_ context.Context, tenant string) (int64, error) {
	if f.missing {
		return 0, ErrNoConsumer
	}
	for i, t := range PlaygroundTenants {
		if t == tenant {
			return int64(100 + i), nil
		}
	}
	return 0, ErrNoConsumer
}

func (f *fakeKeys) IssueKey(_ context.Context, consumer int64, expires *time.Time) (int64, secret.Value, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprintf("spw_testkey%d_%d", consumer, len(f.issued))
	f.issued = append(f.issued, issued{consumer, expires, k})
	return int64(len(f.issued)), secret.New(k), nil
}

func (f *fakeKeys) keys() []issued {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]issued(nil), f.issued...)
}

// fakeGateway counts requests by key and answers 200 for the first ok of
// each key, then 429, with RateLimit headers.
type fakeGateway struct {
	*httptest.Server
	n        atomic.Int64
	ok       int64
	mu       sync.Mutex
	keys     map[string]int64
	ids      []string
	paths    map[string]int
	unauthed int // 401 answers before a key is accepted
	block    chan struct{}
}

func newGateway(t *testing.T, ok int64) *fakeGateway {
	g := &fakeGateway{ok: ok, keys: map[string]int64{}, paths: map[string]int{}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		if g.unauthed > 0 {
			g.unauthed--
			g.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := r.Header.Get("X-API-Key")
		id := r.Header.Get("X-Request-Id")
		g.keys[key]++
		n := g.keys[key]
		g.paths[r.Method+" "+r.URL.Path]++
		if !strings.HasPrefix(id, "pg-probe-") {
			g.ids = append(g.ids, id)
		}
		block := g.block
		g.mu.Unlock()
		if block != nil && !strings.HasPrefix(id, "pg-probe-") {
			<-block
		}
		g.n.Add(1)
		if n <= g.ok {
			w.Header().Set("RateLimit", `"consumer-plan";r=5;t=1`)
			w.Header().Set("RateLimit-Policy", `"consumer-plan";q=10;w=1`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not echoed"))
			return
		}
		w.Header().Set("RateLimit", `"consumer-plan";r=0;t=1`)
		w.Header().Set("RateLimit-Policy", `"consumer-plan";q=10;w=1`)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(g.Close)
	return g
}

func newPG(t *testing.T, st KeyStore, log *bytes.Buffer, gws ...*fakeGateway) *Playground {
	t.Helper()
	urls := make([]string, len(gws))
	for i, g := range gws {
		urls[i] = g.URL
	}
	var w safeBuffer
	if log != nil {
		w.b = log
	} else {
		w.b = &bytes.Buffer{}
	}
	p := NewPlayground(PlaygroundConfig{Gateways: urls, Keys: Keys{On: true}, Store: st, Log: slog.New(slog.NewJSONHandler(&w, nil))})
	t.Cleanup(p.Shutdown)
	return p
}

// safeBuffer is a bytes.Buffer safe for concurrent writes.
type safeBuffer struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// event is one SSE event of a stream.
type event struct {
	name string
	data string
}

// streamOf serves sub's stream to a test client and returns its events
// until the stream ends.
func streamOf(t *testing.T, sub *Subscription, expiry time.Time) []event {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sub.Serve(w, r, expiry) }))
	defer srv.Close()
	res, err := http.Get(srv.URL) //nolint:noctx // a test server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("stream headers: %v", res.Header)
	}
	var out []event
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(nil, 1<<20)
	var e event
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			e.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			e.data = strings.TrimPrefix(line, "data: ")
		case line == "" && e.name != "":
			out = append(out, e)
			e = event{}
		}
	}
	return out
}

// strict checks data against api/openapi.yaml's schema (the events'
// schemas are documentation only, so neither generated nor embedded) and
// decodes it into v.
func strict(t *testing.T, schema, data string, v any) {
	t.Helper()
	spec, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := spec.Components.Schemas[schema]
	if !ok {
		t.Fatalf("no schema %s", schema)
	}
	var doc any
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatal(err)
	}
	if err := ref.Value.VisitJSON(doc); err != nil {
		t.Fatalf("%s does not match %s: %v", data, schema, err)
	}
	if err := json.Unmarshal([]byte(data), v); err != nil {
		t.Fatal(err)
	}
}

var alice = Caller{Sub: "alice", IP: "192.0.2.1"}

// TestPlaygroundRun: a custom run sends at its rate, round robin over the
// gateways, with its tenant's Playground key and pg-<run>-<n> request IDs;
// its stream has ready, a tick per second with counts by status, the
// latest RateLimit headers verbatim and the per-replica split, then end
// with the totals; events match the spec's schemas; no event and no log
// line carries a key, and the logs name no client IP.
func TestPlaygroundRun(t *testing.T) {
	gws := []*fakeGateway{newGateway(t, 4), newGateway(t, 4), newGateway(t, 4)}
	var logs bytes.Buffer
	st := &fakeKeys{}
	p := newPG(t, st, &logs, gws...)
	r, err := p.Start(t.Context(), alice, CustomShape("free-co", "payments", 30, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := p.Subscribe("alice", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	events := streamOf(t, sub, time.Time{})
	if len(events) < 3 || events[0].name != "ready" || events[len(events)-1].name != "end" {
		t.Fatalf("events %v", events)
	}
	var ready apiv1.PlaygroundRun
	strict(t, "PlaygroundRun", events[0].data, &ready)
	if ready.Id != r.ID || ready.Kind != "custom" || *ready.Tenant != "free-co" || *ready.Route != "payments" || *ready.Rate != 30 || ready.DurationS != 2 || ready.State != "running" {
		t.Errorf("ready %+v", ready)
	}
	var sent, ok, limited int64
	replicas := map[string]int64{}
	var last *Last
	for _, e := range events[1 : len(events)-1] {
		if e.name != "tick" {
			t.Fatalf("event %q", e.name)
		}
		var tk Tick
		strict(t, "PlaygroundTick", e.data, &tk)
		c := tk.Tenants["free-co"]
		sent, ok, limited = sent+c.Sent, ok+c.OK, limited+c.Limited
		for k, v := range tk.Replicas {
			replicas[k] += v
		}
		if l, has := tk.Last["free-co"]; has {
			last = &l
		}
		if len(tk.Replicas) != 3 {
			t.Errorf("tick replicas %v, want all three", tk.Replicas)
		}
	}
	var end End
	strict(t, "PlaygroundEnd", events[len(events)-1].data, &end)
	tot := end.Totals["free-co"]
	if end.Reason != "done" || tot.Sent != 60 || tot.OK+tot.Limited != 60 || tot.OK != 9 /* 4 per gateway, one the start's probe */ || tot.Dropped != 0 || tot.P99Ms == nil {
		t.Errorf("end %+v, totals %+v", end, tot)
	}
	if sent != tot.Sent || ok != tot.OK || limited != tot.Limited {
		t.Errorf("ticks sum to sent %d ok %d limited %d; totals %+v", sent, ok, limited, tot)
	}
	for _, g := range gws {
		name := strings.TrimPrefix(g.URL, "http://")
		if replicas[name] != 20 {
			t.Errorf("replica %s got %d of 60, want 20 (round robin)", name, replicas[name])
		}
		g.mu.Lock()
		if g.paths["POST /v1/payments"] != 21 { // 20, and the start's probe
			t.Errorf("%s: %v", name, g.paths)
		}
		for _, id := range g.ids {
			if !strings.HasPrefix(id, "pg-"+r.ID+"-") {
				t.Errorf("request ID %q", id)
			}
		}
		g.mu.Unlock()
	}
	if last == nil || last.Status != 429 || *last.RateLimit != `"consumer-plan";r=0;t=1` || *last.RateLimitPolicy != `"consumer-plan";q=10;w=1` || *last.RetryAfter != "1" {
		t.Errorf("last %+v", last)
	}
	keys := st.keys()
	if len(keys) != 1 || keys[0].consumer != 100 {
		t.Fatalf("issued %v", keys)
	}
	all := logs.String()
	for _, e := range events {
		if strings.Contains(e.data, keys[0].key) || strings.Contains(e.data, "spw_") {
			t.Errorf("an event carries a key: %s", e.data)
		}
	}
	for _, bad := range []string{keys[0].key, "spw_", alice.IP} {
		if strings.Contains(all, bad) {
			t.Errorf("the log has %q:\n%s", bad, all)
		}
	}
	for _, want := range []string{`"msg":"playground run started"`, `"msg":"playground run ended"`, `"run":"` + r.ID + `"`, `"sub":"alice"`} {
		if !strings.Contains(all, want) {
			t.Errorf("the log lacks %s:\n%s", want, all)
		}
	}
	// A late stream: ready, then end with the totals.
	late, err := p.Subscribe("alice", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev := streamOf(t, late, time.Time{}); len(ev) != 2 || ev[0].name != "ready" || ev[1].name != "end" || !strings.Contains(ev[0].data, `"state":"ended"`) {
		t.Errorf("late stream %v", ev)
	}
}

// TestPlaygroundKeysThroughExpiry: the generator issues its keys through
// Keys.Expiry, the 30-minute rule (ADR-0095 §3): one key per tenant, now
// + 30 minutes on the control plane's clock, reused while it has 5
// minutes left and issued anew after; a scenario run gets each of its
// tenants' keys; with demo mode off no key is issued.
func TestPlaygroundKeysThroughExpiry(t *testing.T) {
	g := newGateway(t, 1000)
	st := &fakeKeys{}
	p := newPG(t, st, nil, g)
	clock := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	p.keys.Now = func() time.Time { return clock }
	run := func(s Shape) {
		t.Helper()
		r, err := p.Start(t.Context(), alice, s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Stop("alice", r.ID); err != nil {
			t.Fatal(err)
		}
		for !r.Ended() {
			time.Sleep(10 * time.Millisecond)
		}
	}
	run(CustomShape("pro-co", "orders", 5, 5*time.Second))
	k := st.keys()
	if len(k) != 1 || k[0].consumer != 101 || k[0].expires == nil || !k[0].expires.Equal(clock.Add(30*time.Minute)) {
		t.Fatalf("first key %+v, want pro-co's consumer and %v", k, clock.Add(30*time.Minute))
	}
	clock = clock.Add(25 * time.Minute) // exactly 5 minutes left: reused
	run(CustomShape("pro-co", "orders", 5, 5*time.Second))
	if n := len(st.keys()); n != 1 {
		t.Errorf("%d keys after a run with 5 minutes left, want 1", n)
	}
	clock = clock.Add(time.Second) // under 5 minutes left: a new key
	run(CustomShape("pro-co", "orders", 5, 5*time.Second))
	if k := st.keys(); len(k) != 2 || !k[1].expires.Equal(clock.Add(30*time.Minute)) {
		t.Errorf("keys %+v, want a second one expiring %v", k, clock.Add(30*time.Minute))
	}
	s, _ := ScenarioShape("d2")
	run(s)
	if k := st.keys(); len(k) != 4 {
		t.Errorf("after d2: %d keys, want 4 (free-co and enterprise-co issued)", len(k))
	}

	off := NewPlayground(PlaygroundConfig{Gateways: []string{g.URL}, Store: st})
	defer off.Shutdown()
	if _, err := off.Start(t.Context(), alice, CustomShape("free-co", "orders", 5, 5*time.Second)); err == nil {
		t.Error("demo mode off: a run started")
	}
	if n := len(st.keys()); n != 4 {
		t.Errorf("demo mode off issued a key (%d)", n)
	}
}

// TestPlaygroundWaitsForNewKey: a new key's first requests wait until
// every gateway accepts it (here after two 401 answers), so no counted
// request is a 401 of a key not yet applied.
func TestPlaygroundWaitsForNewKey(t *testing.T) {
	g := newGateway(t, 1000)
	g.unauthed = 2
	p := newPG(t, &fakeKeys{}, nil, g)
	r, err := p.Start(t.Context(), alice, CustomShape("free-co", "orders", 20, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for !r.Ended() {
		time.Sleep(10 * time.Millisecond)
	}
	if tot := r.Totals()["free-co"]; tot.Forbidden != 0 || tot.OK != 20 {
		t.Errorf("totals %+v, want 20 ok and no 401", tot)
	}
}

// TestPlaygroundCaps: per client IP 1 active run and 30 starts an hour,
// per caller 3 active, in all 3 active, 1 scenario run and 700 rps; a
// refused start sends nothing and says when to retry.
func TestPlaygroundCaps(t *testing.T) {
	g := newGateway(t, 1<<30)
	p := newPG(t, &fakeKeys{}, nil, g)
	long := CustomShape("free-co", "orders", 1, MaxDuration)
	start := func(c Caller, s Shape) (*Run, error) { return p.Start(t.Context(), c, s) }
	capErr := func(err error, msg string) {
		t.Helper()
		ce, ok := errors.AsType[*CapError](err)
		if !ok || !strings.Contains(ce.Msg, msg) || ce.RetryAfter < 1 || ce.RetryAfter > 61 {
			t.Errorf("%v, want a cap error with %q and Retry-After 1–61 s", err, msg)
		}
	}
	r1, err := start(Caller{"alice", "192.0.2.1"}, long)
	if err != nil {
		t.Fatal(err)
	}
	_, err = start(Caller{"bob", "192.0.2.1"}, long)
	capErr(err, "from your address")
	// Per caller: 3 active, from 3 addresses.
	_, err = start(Caller{"alice", "192.0.2.2"}, long)
	if err != nil {
		t.Fatal(err)
	}
	_, err = start(Caller{"alice", "192.0.2.3"}, long)
	if err != nil {
		t.Fatal(err)
	}
	_, err = start(Caller{"alice", "192.0.2.4"}, long)
	capErr(err, "you have 3 runs running")
	// In all: 3 active.
	_, err = start(Caller{"carol", "192.0.2.5"}, long)
	capErr(err, "at most 3 in all")
	if u, left := p.Usage(Caller{"alice", "192.0.2.1"}); u != 3 || left != perIPHour-1 {
		t.Errorf("usage %d active, %d left", u, left)
	}
	if _, err := p.Stop("bob", r1.ID); !errors.Is(err, ErrNoRun) {
		t.Errorf("another caller's stop: %v", err)
	}
	if _, err := p.Subscribe("bob", r1.ID); !errors.Is(err, ErrNoRun) {
		t.Errorf("another caller's stream: %v", err)
	}
	p.Shutdown()

	// Scenario and rate caps on a fresh Playground.
	p = newPG(t, &fakeKeys{}, nil, g)
	d2, _ := ScenarioShape("d2") // 650 rps
	sc, err := p.Start(t.Context(), Caller{"alice", "192.0.2.1"}, d2)
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := ScenarioShape("d1")
	_, err = p.Start(t.Context(), Caller{"bob", "192.0.2.2"}, d1)
	capErr(err, "another scenario run")
	_, err = p.Start(t.Context(), Caller{"bob", "192.0.2.2"}, CustomShape("pro-co", "orders", 51, MaxDuration))
	capErr(err, "over 700 requests per second")
	if _, err := p.Start(t.Context(), Caller{"bob", "192.0.2.2"}, CustomShape("pro-co", "orders", 50, MaxDuration)); err != nil {
		t.Errorf("650 + 50 rps: %v", err)
	}
	if _, err := p.Stop("alice", sc.ID); err != nil {
		t.Fatal(err)
	}
	p.Shutdown()

	// Starts per IP per hour: 30, stopped at once; the 31st is refused
	// until the first start is an hour old.
	p = newPG(t, &fakeKeys{}, nil, g)
	short := CustomShape("free-co", "catalog", 1, MaxDuration)
	for i := range perIPHour {
		r, err := p.Start(t.Context(), Caller{fmt.Sprint("user", i), "192.0.2.9"}, short)
		if err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
		if _, err := p.Stop(r.Sub, r.ID); err != nil {
			t.Fatal(err)
		}
		for !r.Ended() {
			time.Sleep(5 * time.Millisecond)
		}
	}
	_, err = p.Start(t.Context(), Caller{"another", "192.0.2.9"}, short)
	ce, ok := errors.AsType[*CapError](err)
	if !ok || !strings.Contains(ce.Msg, "started 30 runs in the last hour") || ce.RetryAfter < 3500 || ce.RetryAfter > 3600 {
		t.Errorf("31st start: %v", err)
	}
	if _, left := p.Usage(Caller{"user0", "192.0.2.9"}); left != 0 {
		t.Errorf("runs left %d, want 0", left)
	}
}

// TestPlaygroundStreams: at most 2 streams per run; a stop ends them with
// reason stopped; an expired token ends one with token_expired while the
// run goes on; a shutdown ends runs with reason shutdown and refuses new
// ones.
func TestPlaygroundStreams(t *testing.T) {
	g := newGateway(t, 1<<30)
	p := newPG(t, &fakeKeys{}, nil, g)
	r, err := p.Start(t.Context(), alice, CustomShape("enterprise-co", "catalog", 10, MaxDuration))
	if err != nil {
		t.Fatal(err)
	}
	s1, err1 := p.Subscribe("alice", r.ID)
	s2, err2 := p.Subscribe("alice", r.ID)
	_, err3 := p.Subscribe("alice", r.ID)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if _, ok := errors.AsType[*CapError](err3); !ok {
		t.Errorf("a third stream: %v", err3)
	}
	expiring := streamOf(t, s1, time.Now().Add(1500*time.Millisecond))
	if last := expiring[len(expiring)-1]; last.name != "end" || !strings.Contains(last.data, `"reason":"token_expired"`) || r.Ended() {
		t.Errorf("token expiry: %v, run ended %v", last, r.Ended())
	}
	done := make(chan []event)
	go func() { done <- streamOf(t, s2, time.Time{}) }()
	time.Sleep(1200 * time.Millisecond)
	if _, err := p.Stop("alice", r.ID); err != nil {
		t.Fatal(err)
	}
	ev := <-done
	var end End
	strict(t, "PlaygroundEnd", ev[len(ev)-1].data, &end)
	if end.Reason != "stopped" || end.Totals["enterprise-co"].Sent < 10 {
		t.Errorf("stopped: %+v", end)
	}

	r2, err := p.Start(t.Context(), Caller{"bob", "192.0.2.7"}, CustomShape("pro-co", "catalog", 10, MaxDuration))
	if err != nil {
		t.Fatal(err)
	}
	s3, _ := p.Subscribe("bob", r2.ID)
	go func() { done <- streamOf(t, s3, time.Time{}) }()
	time.Sleep(300 * time.Millisecond)
	p.Shutdown()
	ev = <-done
	if last := ev[len(ev)-1]; !strings.Contains(last.data, `"reason":"shutdown"`) {
		t.Errorf("shutdown: %v", last)
	}
	if _, err := p.Start(t.Context(), Caller{"carol", "192.0.2.8"}, CustomShape("pro-co", "catalog", 10, MinDuration)); err == nil {
		t.Error("a start after shutdown")
	}
}

// TestPlaygroundInFlight: at most 256 requests in flight per run; one due
// past them is dropped and counted.
func TestPlaygroundInFlight(t *testing.T) {
	g := newGateway(t, 1<<30)
	p := newPG(t, &fakeKeys{}, nil, g)
	// The probe passes; every request of the run waits until released.
	g.block = make(chan struct{})
	r, err := p.Start(t.Context(), alice, CustomShape("free-co", "orders", 400, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	g.mu.Lock()
	close(g.block)
	g.block = nil
	g.mu.Unlock()
	for !r.Ended() {
		time.Sleep(10 * time.Millisecond)
	}
	tot := r.Totals()["free-co"]
	if tot.Sent+tot.Dropped != 400 || tot.Sent != maxInFlight || tot.OK != maxInFlight {
		t.Errorf("totals %+v, want 400 due, 256 sent and answered, the rest dropped", tot)
	}
}

// TestPlaygroundUnavailable: no Playground consumer, or no gateway
// answering, refuses the start and takes back its reservation.
func TestPlaygroundUnavailable(t *testing.T) {
	g := newGateway(t, 1)
	p := newPG(t, &fakeKeys{missing: true}, nil, g)
	if _, err := p.Start(t.Context(), alice, CustomShape("free-co", "orders", 1, MinDuration)); !errors.Is(err, ErrNoConsumer) {
		t.Errorf("no consumer: %v", err)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	p2 := NewPlayground(PlaygroundConfig{Gateways: []string{down.URL}, Keys: Keys{On: true}, Store: &fakeKeys{}})
	defer p2.Shutdown()
	if _, err := p2.Start(t.Context(), alice, CustomShape("free-co", "orders", 1, MinDuration)); !errors.Is(err, ErrGatewaysDown) {
		t.Errorf("gateways down: %v", err)
	}
	if a, left := p2.Usage(alice); a != 0 || left != perIPHour {
		t.Errorf("a refused start counted: %d active, %d left", a, left)
	}
}

// TestPlaygroundStopFreesItsSlot: a stopped run stops counting against
// the caps at once, so its caller's next run from the same address
// starts while the stopped one's last requests finish.
func TestPlaygroundStopFreesItsSlot(t *testing.T) {
	g := newGateway(t, 1<<30)
	p := newPG(t, &fakeKeys{}, nil, g)
	r, err := p.Start(t.Context(), alice, CustomShape("free-co", "orders", 50, MaxDuration))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stop("alice", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(t.Context(), alice, CustomShape("pro-co", "orders", 50, MaxDuration)); err != nil {
		t.Errorf("a start right after a stop: %v", err)
	}
}
