package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ankitbisht23/spillway/internal/hardware"
)

// rules are the constants of ADR-0031's rules.
type rules struct {
	success       time.Duration // (0) and (a): the window that starts at the failure
	maxWindow     time.Duration // (b): a window lies within this
	minWindow     time.Duration // (b): and spans at least this
	minWindows    int           // (b): windows checked, at least
	maxScrapeGap  time.Duration // (b): between consecutive scrapes, at most
	scrapeTimeout time.Duration
	traces        int // (c): retried requests whose traces are checked, at most
	minTraces     int // (c): and at least
	tracePoll     time.Duration
}

var defaultRules = rules{
	success:       10 * time.Second,
	maxWindow:     10 * time.Second,
	minWindow:     8 * time.Second,
	minWindows:    30,
	maxScrapeGap:  2 * time.Second,
	scrapeTimeout: 2 * time.Second,
	traces:        20,
	minTraces:     5,
	tracePoll:     2 * time.Second,
}

// withinBudget is ADR-0030's budget formula with the defaults (ratio 0.2, 3
// retries/s over 10 s) in integers: retries ≤ 0.2 × first attempts + 30.
func withinBudget(retries, firstAttempts float64) bool {
	return 5*retries <= firstAttempts+150
}

// upstreamReport is the JSON "chaos upstream" prints.
type upstreamReport struct {
	Scenario      string        `json:"scenario"`
	Upstream      string        `json:"upstream"`
	FaultEndpoint string        `json:"fault_endpoint"`
	FaultAt       string        `json:"fault_at"` // t_f
	FaultError    string        `json:"fault_error,omitempty"`
	ResetError    string        `json:"reset_error,omitempty"`
	Requests      requestCounts `json:"requests"`
	FaultSeen     faultRule     `json:"rule_0_fault"`
	Success       successRule   `json:"rule_a_success"`
	Budget        budgetRule    `json:"rule_b_budget"`
	Traces        traceRule     `json:"rule_c_traces"`
	Circuit       circuitInfo   `json:"circuit"`
	Scrapes       []scrape      `json:"scrapes"`
	Pass          bool          `json:"pass"`
	Hardware      hardware.Info `json:"hardware"`
}

type faultRule struct {
	Pass bool `json:"pass"`
	// From t_f to the first scrape sent in the window that shows the
	// fault endpoint at 0; -1 if none.
	FirstOutS float64 `json:"first_out_s"`
	// After that scrape, to the end of the run: scrapes showing 0 and all.
	OutScrapes   int `json:"out_scrapes"`
	AfterScrapes int `json:"after_scrapes"`
}

type successRule struct {
	Pass      bool           `json:"pass"`
	Saturated bool           `json:"sender_saturated"`
	Counted   int            `json:"counted"`
	Succeeded int            `json:"succeeded"`
	Failures  map[string]int `json:"failures"` // by status, or timeout, dial, other, dropped
	// 2xx share of the requests due before t_f and due from t_f + 10 s.
	Before float64 `json:"before_2xx"`
	After  float64 `json:"after_2xx"`
}

type budgetRule struct {
	Pass     bool   `json:"pass"`
	Coverage string `json:"coverage_failure,omitempty"`
	Windows  int    `json:"windows"`
	Over     int    `json:"windows_over_budget"`
	// The window whose retries use the largest share of its allowance.
	TightRetries float64 `json:"tightest_retries"`
	TightAllowed float64 `json:"tightest_allowed"`
	TightSpanS   float64 `json:"tightest_span_s"`
	// Over the run.
	Sent            float64 `json:"retries_sent"`
	BudgetExhausted float64 `json:"retries_budget_exhausted"`
	FirstAttempts   float64 `json:"first_attempts"`
}

type traceRule struct {
	Pass    bool `json:"pass"`
	Retried int  `json:"retried_requests"` // sent at or after t_f with Spillway-Attempts >= 2
	// Retried requests sent before t_f (in flight when the fault began):
	// not checked, shown for information.
	RetriedBefore   int           `json:"retried_before_fault"`
	RetriedBeforeMS []float64     `json:"retried_before_fault_sent_ms,omitempty"` // from t_f
	Checked         int           `json:"checked"`
	Matched         int           `json:"matched"`
	AttemptSpans    int           `json:"attempt_spans"`
	Results         []traceResult `json:"results"`
}

type traceResult struct {
	TraceID  string  `json:"trace_id"`
	Attempts int     `json:"attempts"`
	Status   int     `json:"status"`
	SentMS   float64 `json:"sent_ms"` // from t_f
	Spans    int     `json:"attempt_spans"`
	Mismatch string  `json:"mismatch,omitempty"`
}

type circuitInfo struct {
	Max    float64  `json:"max_state"` // -1 without the series
	States []string `json:"states_seen"`
}

var circuitStates = map[float64]string{-1: "no series", 0: "closed", 1: "open", 2: "half-open"}

// analyzeUpstream checks rules (0), (a) and (b) on a run, and takes (c)'s
// results from traces.
func analyzeUpstream(o upstreamOptions, faultAddr string, r scenarioRun, traces []traceResult) upstreamReport {
	rep := upstreamReport{Scenario: o.fault, Upstream: o.upstream, FaultEndpoint: faultAddr, Scrapes: r.scrapes}
	if !r.faultAt.IsZero() {
		rep.FaultAt = r.faultAt.UTC().Format(time.RFC3339Nano)
	}
	tf, end := r.faultAt, r.faultAt.Add(o.rules.success)

	// (0): a scrape sent within [t_f, t_f + 10 s] shows the endpoint out.
	rep.FaultSeen.FirstOutS = -1
	for _, m := range r.scrapes {
		if v, ok := m.Healthy[faultAddr]; rep.FaultSeen.FirstOutS < 0 && !tf.IsZero() && m.OK && ok && v == 0 &&
			!m.Sent.Before(tf) && !m.Sent.After(end) {
			rep.FaultSeen.FirstOutS = m.Sent.Sub(tf).Seconds()
		} else if rep.FaultSeen.FirstOutS >= 0 && m.OK {
			rep.FaultSeen.AfterScrapes++
			if ok && v == 0 {
				rep.FaultSeen.OutScrapes++
			}
		}
	}
	rep.FaultSeen.Pass = rep.FaultSeen.FirstOutS >= 0

	// (a): every request due before t_f + 10 s whose answer had not arrived
	// by t_f; a success is a 2xx read in full.
	a := &rep.Success
	a.Failures = map[string]int{}
	var before, beforeOK, after, afterOK int
	for _, q := range r.out {
		rep.Requests.Scheduled++
		if q.sent {
			rep.Requests.Sent++
		} else {
			rep.Requests.Dropped++
		}
		ok := q.status/100 == 2
		switch {
		case q.due.Before(tf):
			before++
			if ok {
				beforeOK++
			}
		case !q.due.Before(end):
			after++
			if ok {
				afterOK++
			}
		}
		if tf.IsZero() || !q.due.Before(end) || q.answered() && !q.done.After(tf) {
			continue
		}
		a.Counted++
		switch {
		case ok:
			a.Succeeded++
		case !q.sent:
			a.Failures["dropped"]++
		case q.answered():
			a.Failures[strconv.Itoa(q.status)]++
		default:
			a.Failures[q.errKind]++
		}
	}
	a.Before, a.After = share(beforeOK, before), share(afterOK, after)
	a.Saturated = rep.Requests.Sent*100 < rep.Requests.Scheduled*99
	a.Pass = !tf.IsZero() && !a.Saturated && a.Counted > 0 && 100*a.Succeeded >= 99*a.Counted

	rep.Budget = checkBudget(r.scrapes, o.rules)

	// (c)
	c := &rep.Traces
	c.Retried = len(retriedRequests(r.out, tf, len(r.out)))
	for _, q := range r.out {
		if q.sent && q.attempts >= 2 && q.sentAt.Before(tf) {
			c.RetriedBefore++
			c.RetriedBeforeMS = append(c.RetriedBeforeMS, float64(q.sentAt.Sub(tf).Microseconds())/1000)
		}
	}
	c.Results, c.Checked = traces, len(traces)
	for _, t := range traces {
		c.AttemptSpans += t.Spans
		if t.Mismatch == "" {
			c.Matched++
		}
	}
	c.Pass = c.Checked >= o.rules.minTraces && c.Matched == c.Checked

	rep.Circuit.Max = -1
	seen := map[float64]bool{}
	for _, m := range r.scrapes {
		if m.OK {
			seen[m.Circuit] = true
			rep.Circuit.Max = max(rep.Circuit.Max, m.Circuit)
		}
	}
	for _, v := range slices.Sorted(maps.Keys(seen)) {
		rep.Circuit.States = append(rep.Circuit.States, cmp.Or(circuitStates[v], strconv.FormatFloat(v, 'g', -1, 64)))
	}
	rep.Pass = rep.FaultSeen.Pass && a.Pass && rep.Budget.Pass && c.Pass
	return rep
}

func share(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// checkBudget is rule (b): for each scrape j, the earliest scrape i before
// it with recv_j − send_i ≤ 10 s and send_j − recv_i ≥ 8 s makes a window,
// whose retries must be within the budget formula. Every scrape must be
// answered, consecutive scrapes at most 2 s apart, and at least 30 windows
// checked.
func checkBudget(scrapes []scrape, ru rules) budgetRule {
	var b budgetRule
	for k, m := range scrapes {
		switch {
		case !m.OK && b.Coverage == "":
			b.Coverage = fmt.Sprintf("scrape %d failed: %s", k, m.Err)
		case k > 0 && m.Sent.Sub(scrapes[k-1].Sent) > ru.maxScrapeGap && b.Coverage == "":
			b.Coverage = fmt.Sprintf("scrapes %d and %d sent %.2f s apart", k-1, k, m.Sent.Sub(scrapes[k-1].Sent).Seconds())
		}
	}
	tightest := -1.0
	for j, mj := range scrapes {
		for _, mi := range scrapes[:j] {
			if !mi.OK || !mj.OK || mj.Recv.Sub(mi.Sent) > ru.maxWindow || mj.Sent.Sub(mi.Recv) < ru.minWindow {
				continue
			}
			retries, firsts := mj.Retries-mi.Retries, mj.FirstAttempts-mi.FirstAttempts
			b.Windows++
			if !withinBudget(retries, firsts) {
				b.Over++
			}
			if used := retries / (firsts/5 + 30); used > tightest {
				tightest = used
				b.TightRetries, b.TightAllowed, b.TightSpanS = retries, firsts/5+30, mj.Recv.Sub(mi.Sent).Seconds()
			}
			break
		}
	}
	if len(scrapes) > 0 {
		first, last := scrapes[0], scrapes[len(scrapes)-1]
		b.Sent, b.BudgetExhausted, b.FirstAttempts = last.Retries-first.Retries, last.BudgetExhausted-first.BudgetExhausted, last.FirstAttempts-first.FirstAttempts
	}
	if b.Coverage == "" && b.Windows < ru.minWindows {
		b.Coverage = fmt.Sprintf("%d windows checked, want at least %d", b.Windows, ru.minWindows)
	}
	b.Pass = b.Coverage == "" && b.Over == 0
	return b
}

// retriedRequests are the requests sent at or after tf whose answer carried
// Spillway-Attempts ≥ 2, in send order, at most n.
func retriedRequests(out []request, tf time.Time, n int) []request {
	var rs []request
	for _, q := range out {
		if q.sent && !tf.IsZero() && !q.sentAt.Before(tf) && q.attempts >= 2 {
			rs = append(rs, q)
		}
	}
	slices.SortStableFunc(rs, func(x, y request) int { return x.sentAt.Compare(y.sentAt) })
	return rs[:min(n, len(rs))]
}

// checkTraces reads each request's trace from Tempo, again every
// tracePoll while it does not match, until it matches or deadline.
func (s *upstreamSender) checkTraces(ctx context.Context, qs []request, deadline time.Time) []traceResult {
	res := make([]traceResult, len(qs))
	for i, q := range qs {
		res[i] = traceResult{TraceID: q.traceID, Attempts: q.attempts, Status: q.status, Mismatch: "not read"}
	}
	for {
		pending := 0
		for i, q := range qs {
			if res[i].Mismatch == "" {
				continue
			}
			body, err := s.get(ctx, s.control, s.tempo+"/api/v2/traces/"+q.traceID)
			if err != nil {
				res[i].Mismatch = "tempo: " + err.Error()
			} else {
				res[i].Spans, res[i].Mismatch = matchTrace(body, q)
			}
			if res[i].Mismatch != "" {
				pending++
			}
		}
		if pending == 0 || ctx.Err() != nil || !time.Now().Add(s.rules.tracePoll).Before(deadline) {
			return res
		}
		sleepUntil(ctx, time.Now().Add(s.rules.tracePoll))
	}
}

// tempoTrace is the part of Tempo's /api/v2/traces/<id> answer the check
// reads: OTLP JSON, with IDs in base64 and integers as strings.
type tempoTrace struct {
	Trace struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []tempoAttr `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []tempoSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	} `json:"trace"`
}

type tempoSpan struct {
	SpanID       string      `json:"spanId"`
	ParentSpanID string      `json:"parentSpanId"`
	Name         string      `json:"name"`
	Kind         string      `json:"kind"`
	Attributes   []tempoAttr `json:"attributes"`
	Status       struct {
		Code json.RawMessage `json:"code"`
	} `json:"status"`
}

type tempoAttr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string          `json:"stringValue"`
		IntValue    *json.RawMessage `json:"intValue"`
	} `json:"value"`
}

// isError: OTLP JSON's status code, by name or number.
func (s tempoSpan) isError() bool {
	c := strings.Trim(string(s.Status.Code), `"`)
	return c == "STATUS_CODE_ERROR" || c == "2"
}

func attr(as []tempoAttr, key string) (string, bool) {
	for _, a := range as {
		if a.Key != key {
			continue
		}
		switch {
		case a.Value.StringValue != nil:
			return *a.Value.StringValue, true
		case a.Value.IntValue != nil:
			return strings.Trim(string(*a.Value.IntValue), `"`), true
		}
	}
	return "", false
}

// spanID returns an OTLP JSON span ID as hex: Tempo writes base64, other
// writers hex.
func spanID(s string) string {
	if len(s) == 16 {
		if _, err := hex.DecodeString(s); err == nil {
			return strings.ToLower(s)
		}
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return hex.EncodeToString(b)
	}
	return s
}

// matchTrace checks one retried request's trace (ADR-0031, rule c): exactly
// one spillway-gateway server span, the child of the span the sender sent;
// exactly n "upstream attempt" children of it, n the request's
// Spillway-Attempts, numbered 1…n; attempts before the last failed; the
// last has the status the sender got or, without a response, failed while
// the sender got 502 or 504. It returns the attempt spans found and "" on a
// match, or what does not match.
func matchTrace(body []byte, q request) (attempts int, mismatch string) {
	var t tempoTrace
	if err := json.Unmarshal(body, &t); err != nil {
		return 0, "tempo: " + err.Error()
	}
	var servers, all []tempoSpan
	for _, rs := range t.Trace.ResourceSpans {
		service, _ := attr(rs.Resource.Attributes, "service.name")
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				all = append(all, sp)
				if service == "spillway-gateway" && sp.Kind == "SPAN_KIND_SERVER" && spanID(sp.ParentSpanID) == q.spanID {
					servers = append(servers, sp)
				}
			}
		}
	}
	if len(all) == 0 {
		return 0, "no trace yet"
	}
	if len(servers) != 1 {
		return 0, fmt.Sprintf("%d gateway server spans with parent %s, want 1", len(servers), q.spanID)
	}
	server := spanID(servers[0].SpanID)
	byN := map[int]tempoSpan{}
	for _, sp := range all {
		if sp.Name != "upstream attempt" || spanID(sp.ParentSpanID) != server {
			continue
		}
		attempts++
		v, _ := attr(sp.Attributes, "spillway.attempt")
		n, err := strconv.Atoi(v)
		if err != nil {
			return attempts, fmt.Sprintf("attempt span without spillway.attempt (%q)", v)
		}
		if _, dup := byN[n]; dup {
			return attempts, fmt.Sprintf("two spans of attempt %d", n)
		}
		byN[n] = sp
	}
	if attempts != q.attempts {
		return attempts, fmt.Sprintf("%d attempt spans, Spillway-Attempts %d", attempts, q.attempts)
	}
	for n := 1; n <= q.attempts; n++ {
		sp, ok := byN[n]
		switch {
		case !ok:
			return attempts, fmt.Sprintf("no span of attempt %d", n)
		case n < q.attempts && !sp.isError():
			return attempts, fmt.Sprintf("attempt %d of %d is not an error", n, q.attempts)
		case n < q.attempts:
			continue
		}
		code, has := attr(sp.Attributes, "http.response.status_code")
		switch {
		case has && code != strconv.Itoa(q.status):
			return attempts, fmt.Sprintf("last attempt has status %s, the client got %d", code, q.status)
		case !has && (!sp.isError() || q.status != 502 && q.status != 504):
			return attempts, fmt.Sprintf("last attempt has no response (error %v), the client got %d", sp.isError(), q.status)
		}
	}
	return attempts, ""
}

// line is one check in the verify scripts' format.
func line(ok bool, check, measured string) string {
	verdict := "FAIL"
	if ok {
		verdict = "PASS"
	}
	return fmt.Sprintf("%-4s  %-44s %s", verdict, check, measured)
}

// lines are the report's rule lines, then its information lines (indented
// six spaces, as the verify scripts print details).
func (r upstreamReport) lines(scenario string, ru rules) []string {
	f := r.FaultSeen
	fault := fmt.Sprintf("no scrape within %s of the fault shows %s at 0", ru.success, r.FaultEndpoint)
	if f.Pass {
		fault = fmt.Sprintf("%s at 0 from +%.2f s; %d/%d later scrapes show it out", r.FaultEndpoint, f.FirstOutS, f.OutScrapes, f.AfterScrapes)
	}
	if r.FaultError != "" {
		fault += "; fault request: " + r.FaultError
	}

	a := r.Success
	succ := fmt.Sprintf("%d/%d (%.2f%%) succeeded; failures: %s; 2xx before the fault %.2f%%, after +%s %.2f%%",
		a.Succeeded, a.Counted, 100*share(a.Succeeded, a.Counted), failures(a.Failures), 100*a.Before, ru.success, 100*a.After)
	if a.Saturated {
		succ = fmt.Sprintf("sender saturated: %d/%d sent; ", r.Requests.Sent, r.Requests.Scheduled) + succ
	}

	b := r.Budget
	budget := fmt.Sprintf("%d windows, %d over; tightest %.0f retries vs %.0f allowed over %.1f s; run: %.0f sent, %.0f budget_exhausted, %.0f first attempts",
		b.Windows, b.Over, b.TightRetries, b.TightAllowed, b.TightSpanS, b.Sent, b.BudgetExhausted, b.FirstAttempts)
	if b.Coverage != "" {
		budget = "coverage: " + b.Coverage + "; " + budget
	}

	c := r.Traces
	traces := fmt.Sprintf("%d/%d traces match (of %d retried requests, %d more sent before the fault), %d attempt spans", c.Matched, c.Checked, c.Retried, c.RetriedBefore, c.AttemptSpans)
	if c.Checked < ru.minTraces {
		traces = fmt.Sprintf("too few retried requests: %d, want at least %d; ", c.Checked, ru.minTraces) + traces
	}
	for _, t := range c.Results {
		if t.Mismatch != "" {
			traces += fmt.Sprintf("; first mismatch %s: %s", t.TraceID, t.Mismatch)
			break
		}
	}

	ls := []string{
		line(f.Pass, scenario+": (0) fault endpoint out within 10 s", fault),
		line(a.Pass, scenario+": (a) >= 99% succeed within 10 s", succ),
		line(b.Pass, scenario+": (b) retries within budget", budget),
		line(c.Pass, scenario+": (c) traces show each attempt", traces),
	}
	if r.ResetError != "" {
		ls = append(ls, fmt.Sprintf("      %s: setting the mock back to none failed: %s", scenario, r.ResetError))
	}
	return append(ls,
		fmt.Sprintf("      %s: breaker (gw_circuit_state) highest %s; states seen: %s", scenario, cmp.Or(circuitStates[r.Circuit.Max], "?"), strings.Join(r.Circuit.States, ", ")),
		fmt.Sprintf("      %s: requests %d scheduled, %d sent, %d dropped; fault at %s", scenario, r.Requests.Scheduled, r.Requests.Sent, r.Requests.Dropped, cmp.Or(r.FaultAt, "never")),
	)
}

func failures(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
