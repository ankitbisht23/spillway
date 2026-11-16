package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankitbisht23/spillway/internal/controlplane/live"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
)

// TestShadowReportWindow: a window other than 15m, 1h, 24h and 7d is 400,
// before the store is read (UI-01, D8).
func TestShadowReportWindow(t *testing.T) {
	h := newHandler(t, io.Discard)
	for _, w := range []string{"2h", "", "7D"} {
		a := do(t, h, http.MethodGet, BasePath+"/policies/1/shadow-report?window="+w, "", "Authorization", "Bearer "+token("viewer"))
		if a.status != http.StatusBadRequest || a.err.Code != "invalid_request" {
			t.Errorf("window %q: %d %s", w, a.status, a.err.Code)
		}
	}
}

func TestRatioAndLater(t *testing.T) {
	if ratio(1, 0) != nil || *ratio(1, 4) != 0.25 {
		t.Error("ratio")
	}
	a, b := time.Unix(10, 0), time.Unix(20, 0)
	if !later(a, b).Equal(b) || !later(b, a).Equal(b) {
		t.Error("later")
	}
}

// TestShadowReportConcurrency: however many reports are asked for at
// once, at most shadowConcurrency query Prometheus at a time; the rest
// wait for a slot and still answer (ADR-0060).
func TestShadowReportConcurrency(t *testing.T) {
	var now, most atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := now.Add(1)
		defer now.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(prom.Close)
	u, err := live.ParsePrometheusURL(prom.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := (&Server{log: slog.New(slog.DiscardHandler)}).WithPrometheus(live.NewPrometheus(u, prom.Client()))
	var wg sync.WaitGroup
	errs := make(chan *apiv1.ShadowReportMetricsError, 16)
	for range 16 {
		wg.Go(func() {
			// 16 reports of 2 queries of 20 ms, 4 at a time: about 160 ms,
			// far inside shadowQueryTimeout even with a host stall
			// (ADR-0034).
			r := apiv1.ShadowReport{Until: time.Now()}
			if e := s.shadowCounts(context.Background(), &r, "p", time.Hour); e != nil {
				errs <- e
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("a report failed: %s", *e)
	}
	if m := most.Load(); m > shadowConcurrency || m < 2 {
		t.Errorf("at most %d queries at once, want 2 to %d", m, shadowConcurrency)
	}
}
