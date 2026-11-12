//go:build integration

package integration

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
)

// tempoSpan is a span as Tempo's /api/v2/traces answers it: OTLP JSON,
// with IDs in base64.
type tempoSpan struct {
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Attributes   []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
			IntValue    string `json:"intValue"`
		} `json:"value"`
	} `json:"attributes"`
	Status struct {
		Code string `json:"code"`
	} `json:"status"`
}

func (s tempoSpan) attr(key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value.StringValue + a.Value.IntValue
		}
	}
	return ""
}

// hexID is a base64 span ID in hex.
func hexID(t *testing.T, b64 string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("span ID %q: %v", b64, err)
	}
	return hex.EncodeToString(b)
}

// TestTraceInTempo sends a sampled request through gateway-1 on the dev
// config's retry route to a path every endpoint answers 503, and finds its
// trace in Tempo, through the OTel Collector's tail sampling (its errors
// policy keeps it): the gateway's server span under the client's span, and
// one upstream attempt span per attempt under it, numbered 1 to 3, each
// failed with the 503 (OB-02, GW-22).
func TestTraceInTempo(t *testing.T) {
	var traceID [16]byte
	var parent [8]byte
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(parent[:])
	tid, pid := hex.EncodeToString(traceID[:]), hex.EncodeToString(parent[:])
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8000/retry/status/503", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Traceparent", "00-"+tid+"-"+pid+"-01")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Spillway-Attempts") != "3" {
		t.Fatalf("GET /retry/status/503 = %d after %q attempts, want 503 after 3", resp.StatusCode, resp.Header.Get("Spillway-Attempts"))
	}

	// The Collector decides 10 s after the trace's first span; the gateway
	// and the Collector each batch for about a second; Tempo then ingests.
	var spans []tempoSpan
	var body string
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(2 * time.Second) {
		var code int
		code, body = get(t, "http://127.0.0.1:3200/api/v2/traces/"+tid)
		var r struct {
			Trace struct {
				ResourceSpans []struct {
					ScopeSpans []struct {
						Spans []tempoSpan `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			} `json:"trace"`
		}
		spans = nil
		if code == http.StatusOK && json.Unmarshal([]byte(body), &r) == nil {
			for _, rs := range r.Trace.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					spans = append(spans, ss.Spans...)
				}
			}
		}
		// The server span ends last.
		if slices.ContainsFunc(spans, func(s tempoSpan) bool { return s.Kind == "SPAN_KIND_SERVER" }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Tempo has no server span for trace %s within 60 s: %s", tid, body)
		}
	}

	var server tempoSpan
	var attempts []tempoSpan
	for _, s := range spans {
		switch {
		case s.Kind == "SPAN_KIND_SERVER":
			server = s
		case s.Name == "upstream attempt":
			attempts = append(attempts, s)
		}
	}
	if server.Name != "GET retry" || hexID(t, server.ParentSpanID) != pid || server.attr("spillway.attempts") != "3" || server.Status.Code != "STATUS_CODE_ERROR" {
		t.Errorf("server span %q, parent %s, attempts %q, status %q; want GET retry under %s, 3 attempts, error",
			server.Name, hexID(t, server.ParentSpanID), server.attr("spillway.attempts"), server.Status.Code, pid)
	}
	if len(attempts) != 3 {
		t.Fatalf("%d upstream attempt spans, want 3: %s", len(attempts), body)
	}
	slices.SortFunc(attempts, func(a, b tempoSpan) int {
		x, _ := strconv.Atoi(a.attr("spillway.attempt"))
		y, _ := strconv.Atoi(b.attr("spillway.attempt"))
		return x - y
	})
	for i, a := range attempts {
		if a.ParentSpanID != server.SpanID || a.attr("spillway.attempt") != strconv.Itoa(i+1) || a.attr("http.response.status_code") != "503" ||
			a.Status.Code != "STATUS_CODE_ERROR" || a.Kind != "SPAN_KIND_CLIENT" || a.attr("spillway.upstream") != "httpbin-retry" {
			t.Errorf("attempt span %d: %+v", i+1, a)
		}
		if reason := a.attr("spillway.retry_reason"); i > 0 && reason != "503" {
			t.Errorf("attempt %d: retry_reason %q, want 503", i+1, reason)
		}
	}
}
