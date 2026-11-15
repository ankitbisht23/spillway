package live

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// roundTrip answers every Prometheus call with body and status, and
// records each query.
type roundTrip struct {
	status  int
	body    string
	queries []string
}

func (f *roundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.ParseForm(); err != nil {
		return nil, err
	}
	f.queries = append(f.queries, req.PostForm.Get("query"))
	rec := httptest.NewRecorder()
	rec.WriteHeader(f.status)
	_, _ = rec.WriteString(f.body)
	return rec.Result(), nil
}

func promOf(t *testing.T, f *roundTrip) *Prometheus {
	t.Helper()
	u, err := url.Parse("http://prometheus.test:9090")
	if err != nil {
		t.Fatal(err)
	}
	return NewPrometheus(u, &http.Client{Transport: f})
}

// TestDecisions: one instant query of the policy's decisions by mode and
// decision over the window, the name escaped; shadow and enforce summed,
// would-denies and denies picked out, off and NaN values ignored.
func TestDecisions(t *testing.T) {
	f := &roundTrip{status: 200, body: `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"mode":"shadow","decision":"allow"},"value":[1,"90"]},
		{"metric":{"mode":"shadow","decision":"deny"},"value":[1,"10.5"]},
		{"metric":{"mode":"shadow","decision":"fail_open"},"value":[1,"NaN"]},
		{"metric":{"mode":"enforce","decision":"allow"},"value":[1,"30"]},
		{"metric":{"mode":"enforce","decision":"deny"},"value":[1,"10"]},
		{"metric":{"mode":"off","decision":"allow"},"value":[1,"1000"]}]}}`}
	d, err := promOf(t, f).Decisions(t.Context(), `a"b\c`, 7*24*time.Hour, time.Unix(1_789_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if d != (Decisions{Shadow: 100.5, ShadowDenied: 10.5, Enforce: 40, Denied: 10}) {
		t.Errorf("decisions %+v", d)
	}
	want := `sum by (mode, decision) (increase(gw_ratelimit_decisions_total{policy="a\"b\\c"}[604800s]))`
	if len(f.queries) != 1 || f.queries[0] != want {
		t.Errorf("queries %q, want %q", f.queries, want)
	}

	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{503, `{"status":"error","errorType":"unavailable"}`, ErrPrometheusUnavailable},
		{400, `{"status":"error","errorType":"bad_data"}`, errQueryFailed},
		{200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, errQueryFailed},
	} {
		if _, err := promOf(t, &roundTrip{status: tc.status, body: tc.body}).Decisions(t.Context(), "p", time.Hour, time.Now()); !errors.Is(err, tc.want) {
			t.Errorf("%d %s: %v, want %v", tc.status, tc.body, err, tc.want)
		}
	}
}

// TestOldestSample: Prometheus's own lowest timestamp, or none when the
// series is missing.
func TestOldestSample(t *testing.T) {
	f := &roundTrip{status: 200, body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1789000000.25"]}]}}`}
	at, ok, err := promOf(t, f).OldestSample(t.Context(), time.Now())
	if err != nil || !ok || !at.Equal(time.UnixMilli(1_789_000_000_250)) {
		t.Errorf("oldest %v %v %v", at, ok, err)
	}
	if !strings.Contains(f.queries[0], "prometheus_tsdb_lowest_timestamp_seconds") {
		t.Errorf("query %q", f.queries[0])
	}
	_, ok, err = promOf(t, &roundTrip{status: 200, body: `{"status":"success","data":{"resultType":"vector","result":[]}}`}).OldestSample(t.Context(), time.Now())
	if ok || err != nil {
		t.Errorf("no series: %v %v", ok, err)
	}
	_, ok, err = promOf(t, &roundTrip{status: 502, body: "bad gateway"}).OldestSample(t.Context(), time.Now())
	if ok || !errors.Is(err, ErrPrometheusUnavailable) {
		t.Errorf("down: %v %v", ok, err)
	}
}
