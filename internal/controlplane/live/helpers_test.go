package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeProm is Prometheus as an http.RoundTripper, so it runs inside a
// synctest bubble: it records each query and answers by mode.
type fakeProm struct {
	mu    sync.Mutex
	calls []url.Values // each call's form, with "endpoint" added
	mode  string       // "" answers; refused, down (503), bad (400), huge (over 1 MiB), html
}

func (f *fakeProm) setMode(m string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = m
}

func (f *fakeProm) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	mode := f.mode
	f.mu.Unlock()
	if mode == "refused" {
		return nil, errors.New("dial tcp: connection refused")
	}
	if err := req.ParseForm(); err != nil {
		return nil, err
	}
	form := req.PostForm
	endpoint := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
	form.Set("endpoint", endpoint)
	f.mu.Lock()
	f.calls = append(f.calls, form)
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	switch mode {
	case "down":
		rec.WriteHeader(http.StatusServiceUnavailable)
		_, _ = rec.WriteString(`{"status":"error","errorType":"unavailable","error":"overloaded"}`)
	case "bad":
		rec.WriteHeader(http.StatusBadRequest)
		_, _ = rec.WriteString(`{"status":"error","errorType":"bad_data","error":"parse error"}`)
	case "html":
		_, _ = rec.WriteString(`<html>not prometheus</html>`)
	case "huge":
		_, _ = rec.WriteString(`{"status":"success","data":{"resultType":"vector","result":[],"pad":"` + strings.Repeat("x", maxAnswer) + `"}}`)
	default:
		q := form.Get("query")
		if endpoint == "query_range" {
			//nolint:gosec // G705: a test double's JSON answer, not HTML
			_, _ = fmt.Fprintf(rec, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"code_class":"2xx"},"values":[[%s,"1.5"],[%s,"NaN"]]}]}}`,
				form.Get("start"), form.Get("end"))
			break
		}
		// routes.*: two replicas, listed out of order, with one value
		// that is no number.
		if strings.Contains(q, "max by (replica") {
			_, _ = rec.WriteString(`{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"replica":"gateway-2","upstream":"u","endpoint":"10.0.0.1:80"},"value":[1,"1"]},` +
				`{"metric":{"replica":"gateway-1","upstream":"u","endpoint":"10.0.0.1:80"},"value":[1,"NaN"]}]}}`)
			break
		}
		// Instant: by route or tenant, 12 values with decreasing rates.
		by := "route"
		if strings.Contains(q, "sum by (tenant") {
			by = "tenant"
		}
		var series []string
		for i := range 12 {
			v := fmt.Sprint(12 - i)
			if i == 3 {
				v = "+Inf"
			}
			series = append(series, fmt.Sprintf(`{"metric":{%q:"%s-%02d"},"value":[1,%q]}`, by, by, i, v))
		}
		_, _ = fmt.Fprintf(rec, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(series, ","))
	}
	return rec.Result(), nil
}

// count is how many calls had a query containing substr.
func (f *fakeProm) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c.Get("query"), substr) {
			n++
		}
	}
	return n
}

func (f *fakeProm) queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var q []string
	for _, c := range f.calls {
		q = append(q, c.Get("query"))
	}
	return q
}

var errPGDown = errors.New("postgres: connection refused")

// fakePG is the store: routes and tenants by name, replicas and heavy
// hitters as fixed answers, and a switch for an unavailable database.
type fakePG struct {
	mu       sync.Mutex
	routes   []string
	tenants  []string
	down     bool
	broken   bool // answers an error that is not unavailability
	calls    map[string]int
	since    time.Time
	policyID *int64
}

func newFakePG() *fakePG {
	return &fakePG{routes: []string{"r1", "orders"}, tenants: []string{"acme"}, calls: map[string]int{}}
}

func (f *fakePG) call(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	if f.broken {
		return errors.New("a bug")
	}
	if f.down {
		return errPGDown
	}
	return nil
}

func (f *fakePG) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakePG) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakePG) RouteExists(_ context.Context, name string) (bool, error) {
	if err := f.call("RouteExists"); err != nil {
		return false, err
	}
	return contains(f.routes, name), nil
}

func (f *fakePG) TenantExists(_ context.Context, name string) (bool, error) {
	if err := f.call("TenantExists"); err != nil {
		return false, err
	}
	return contains(f.tenants, name), nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (f *fakePG) Replicas(context.Context) (any, error) {
	if err := f.call("Replicas"); err != nil {
		return nil, err
	}
	return map[string]any{"items": []any{}}, nil
}

func (f *fakePG) HeavyHitters(_ context.Context, since time.Time, policyID *int64) (any, error) {
	if err := f.call("HeavyHitters"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.since, f.policyID = since, policyID
	f.mu.Unlock()
	return map[string]any{"items": []any{}}, nil
}

func (f *fakePG) Unavailable(err error) bool { return errors.Is(err, errPGDown) }

// testHub is a hub over the fakes, with its own registry and log.
type testHub struct {
	*Hub
	prom *fakeProm
	pg   *fakePG
	reg  *prometheus.Registry
	log  *syncBuffer
}

func newTestHub(t *testing.T, withProm bool, change func(*Config)) *testHub {
	t.Helper()
	th := &testHub{prom: &fakeProm{}, pg: newFakePG(), reg: prometheus.NewRegistry(), log: &syncBuffer{}}
	cfg := Config{Postgres: th.pg, Registerer: th.reg, Log: slog.New(slog.NewJSONHandler(th.log, nil))}
	if withProm {
		u, err := ParsePrometheusURL("http://prometheus.test:9090")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Prometheus = NewPrometheus(u, &http.Client{Transport: th.prom})
	}
	if change != nil {
		change(&cfg)
	}
	th.Hub = NewHub(cfg)
	return th
}

// metric is the value of the counter or gauge name with labels
// (name=value pairs).
func (th *testHub) metric(t *testing.T, name string, labels ...string) float64 {
	t.Helper()
	mfs, err := th.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			for i := 0; i+1 < len(labels); i += 2 {
				found := false
				for _, l := range m.GetLabel() {
					found = found || l.GetName() == labels[i] && l.GetValue() == labels[i+1]
				}
				if !found {
					continue next
				}
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("no metric %s %v", name, labels)
	return 0
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeWriter is a ResponseWriter for Serve inside a synctest bubble: it
// keeps what is written, the deadlines Serve sets and, while blocked,
// holds writes until unblocked or past the write deadline, as a client
// that does not read would.
type fakeWriter struct {
	mu            sync.Mutex
	header        http.Header
	status        int
	out           bytes.Buffer
	deadline      time.Time
	block         chan struct{}
	readCleared   bool
	writesCleared bool
}

func newFakeWriter() *fakeWriter { return &fakeWriter{header: http.Header{}} }

func (w *fakeWriter) Header() http.Header { return w.header }

func (w *fakeWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = code
}

func (w *fakeWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	block, deadline := w.block, w.deadline
	w.mu.Unlock()
	if block != nil {
		var expired <-chan time.Time
		if !deadline.IsZero() {
			t := time.NewTimer(time.Until(deadline))
			defer t.Stop()
			expired = t.C
		}
		select {
		case <-block:
		case <-expired:
			return 0, fmt.Errorf("write: %w", os.ErrDeadlineExceeded)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(b)
}

func (w *fakeWriter) FlushError() error { return nil }

func (w *fakeWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = t
	w.writesCleared = w.writesCleared || t.IsZero()
	return nil
}

func (w *fakeWriter) SetReadDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.readCleared = w.readCleared || t.IsZero()
	return nil
}

// setBlocked makes later writes wait (true) or lets them through.
func (w *fakeWriter) setBlocked(on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on && w.block == nil {
		w.block = make(chan struct{})
	}
	if !on && w.block != nil {
		close(w.block)
		w.block = nil
	}
}

// sse is one parsed event.
type sse struct {
	name string
	data map[string]any
	raw  string
}

// events parses what was written; keepalives count as comments.
func (w *fakeWriter) events(t *testing.T) (events []sse, keepalives int) {
	t.Helper()
	w.mu.Lock()
	text := w.out.String()
	w.mu.Unlock()
	for frame := range strings.SplitSeq(text, "\n\n") {
		if frame == "" {
			continue
		}
		if frame == ": keepalive" {
			keepalives++
			continue
		}
		name, rest, ok := strings.Cut(frame, "\n")
		data, ok2 := strings.CutPrefix(rest, "data: ")
		name, ok3 := strings.CutPrefix(name, "event: ")
		if !ok || !ok2 || !ok3 || strings.Contains(data, "\n") {
			t.Fatalf("not an SSE frame: %q", frame)
		}
		e := sse{name: name, raw: data}
		if err := json.Unmarshal([]byte(data), &e.data); err != nil {
			t.Fatalf("event data is not JSON: %q", data)
		}
		events = append(events, e)
	}
	return events, keepalives
}

// named are the events called name (and, if sub is given, for that sub).
func named(events []sse, name string, sub ...string) []sse {
	var out []sse
	for _, e := range events {
		if e.name == name && (len(sub) == 0 || e.data["sub"] == sub[0]) {
			out = append(out, e)
		}
	}
	return out
}

// served is a stream being served to a fakeWriter.
type served struct {
	w      *fakeWriter
	cancel context.CancelFunc
	done   chan struct{}
}

// serve opens a stream for subject with query and serves it in a
// goroutine until cancel; expiry zero means the token never expires.
func (th *testHub) serve(t *testing.T, subject, query string, expiry time.Time) *served {
	t.Helper()
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	st, err := th.Open(context.Background(), subject, q)
	if err != nil {
		t.Fatalf("open %s: %v", query, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{w: newFakeWriter(), cancel: cancel, done: make(chan struct{})}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/stream?"+query, nil)
	go func() {
		defer close(s.done)
		st.Serve(s.w, req, expiry)
	}()
	return s
}

// stop ends the stream as a client that goes away and waits for Serve.
func (s *served) stop() {
	s.cancel()
	<-s.done
}
