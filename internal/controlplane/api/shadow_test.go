package api

import (
	"io"
	"net/http"
	"testing"
	"time"
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
