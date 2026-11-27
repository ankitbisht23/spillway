package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The base load and the scenarios D1–D7 (SC-02, ADR-0096 §2), each in its
// demo form (make demo-dN) and its verify-m8 form (--check, ADR-0098 §2,
// with ADR-0101 §13's D6 budget rule).

// scenario is one "run --scenario" value.
type scenario struct {
	name, title string
	ids         []checkDef // its checks, in order
	length      func(check bool) time.Duration
	streams     func(check bool) []streamSpec // beyond the base load
	every       time.Duration                 // tick's period; zero: one second
	tick        func(r *runner, n int)        // the viewer's line(s), every period
	run         func(r *runner) error         // actions; returns when the load is over
	end         func(r *runner)               // the viewer's last lines
	checks      func(r *runner)               // --check: judge every check of ids
}

// checkDef names a check and what it checks.
type checkDef struct{ id, what string }

// baseConsumers are the base load's consumers, one per tenant.
var baseConsumers = []string{"free-co-web", "pro-co-web", "enterprise-co-web"}

// baseLoad is each tenant's base load (ADR-0096 §2), 258.5 rps in all,
// under every limit: no 429 and no heavy hitter from it alone.
func baseLoad() []streamSpec {
	return []streamSpec{
		{consumer: "free-co-web", route: "orders", rate: 4},
		{consumer: "free-co-web", route: "catalog", rate: 2},
		{consumer: "free-co-web", route: "payments", rate: 1, per: 2 * time.Second},
		{consumer: "pro-co-web", route: "orders", rate: 35},
		{consumer: "pro-co-web", route: "catalog", rate: 15},
		{consumer: "pro-co-web", route: "payments", rate: 2},
		{consumer: "enterprise-co-web", route: "orders", rate: 140},
		{consumer: "enterprise-co-web", route: "catalog", rate: 50},
		{consumer: "enterprise-co-web", route: "payments", rate: 10},
	}
}

func fixed(demo, check time.Duration) func(bool) time.Duration {
	return func(c bool) time.Duration {
		if c {
			return check
		}
		return demo
	}
}

// scenarios returns the scenarios by name, each with fresh state.
func scenarios() map[string]*scenario {
	m := map[string]*scenario{}
	for _, s := range []*scenario{scenarioBase(), scenarioD1(), scenarioD2(), scenarioD3(), scenarioD4(), scenarioD5(), scenarioD6(), scenarioD7()} {
		m[s.name] = s
	}
	return m
}

// scenarioBase is the base load alone, the demo-traffic service's
// (ADR-0096 §1): it runs until stopped, prints each tenant's numbers every
// 10 s and replaces keys before they expire.
func scenarioBase() *scenario {
	return &scenario{
		name: "base", title: "base load per tenant",
		length:  fixed(0, 0),
		streams: func(bool) []streamSpec { return nil },
		every:   10 * time.Second,
		tick: func(r *runner, _ int) {
			var parts []string
			for _, c := range baseConsumers {
				t := r.live(c, "")
				parts = append(parts, fmt.Sprintf("%s %.0f/s allowed, %.0f/s denied, p99 %s", tenantOf(c), float64(t.ok)/10, float64(t.limited+t.forbidden)/10, ms(t.p99())))
			}
			r.say("%s", strings.Join(parts, " | "))
		},
		run: func(r *runner) error {
			end := time.NewTimer(time.Until(r.e.t0.Add(r.e.end)))
			defer end.Stop()
			renew := time.NewTicker(30 * time.Second)
			defer renew.Stop()
			for {
				select {
				case <-r.ctx.Done():
					return nil
				case <-end.C:
					return nil
				case <-renew.C:
				}
				for _, c := range r.e.keys.due(time.Now()) {
					k, err := r.e.keys.newKey(r.ctx, c)
					if err == nil {
						err = waitApplied(r.ctx, r.e.gws, k)
					}
					if err != nil {
						r.say("could not replace %s's key, trying again in 30 s: %v", c, err)
						continue
					}
					r.e.keys.use(k)
					r.say("%s now sends %s", c, k)
				}
			}
		},
	}
}

// waitEnd waits until the run's end, or SIGINT or SIGTERM.
func waitEnd(r *runner) error {
	r.e.sleepUntil(r.ctx, r.e.end)
	return nil
}

// scenarioD1 is D1, fair limits by plan (RL-03, RL-08, RL-25):
// free-co-mobile at 3× its 10/s, pro-co-mobile at half its 100/s, on
// orders.
func scenarioD1() *scenario {
	const free, pro = "free-co-mobile", "pro-co-mobile"
	from, to := 10*time.Second, 40*time.Second // verify's measured window
	return &scenario{
		name: "d1", title: "fair limits by plan (RL-03, RL-08, RL-25)",
		ids: []checkDef{
			{"d1.1", "every answer 2xx or (free only) 429"},
			{"d1.2", "pro keeps flowing: no 429 for pro-co-mobile"},
			{"d1.3", "free held at its limit: 2xx of free-co-mobile due in [10 s, 40 s)"},
			{"d1.4", "RateLimit, RateLimit-Policy and Retry-After headers"},
		},
		length: fixed(60*time.Second, 45*time.Second),
		streams: func(bool) []streamSpec {
			return []streamSpec{
				{consumer: free, route: "orders", rate: 30, headers: true},
				{consumer: pro, route: "orders", rate: 50, headers: true},
			}
		},
		tick: func(r *runner, n int) {
			f, p := r.live(free, "orders"), r.live(pro, "orders")
			r.say("%s orders: 2xx %3d  429 %3d  |  %s orders: 2xx %3d  429 %3d", free, f.ok, f.limited, pro, p.ok, p.limited)
			if n%5 == 0 {
				for _, c := range []string{free, pro} {
					for _, s := range r.streamsOf(c, "orders") {
						a := s.latest()
						ra := a.retryAfter
						if ra == "" {
							ra = "-"
						}
						r.say("  %s latest %d: RateLimit: %s | RateLimit-Policy: %s | Retry-After: %s", c, a.status, a.rateLimit, a.policy, ra)
					}
				}
			}
		},
		run: waitEnd,
		end: func(r *runner) {
			f, p := r.answersOf(free, "orders"), r.answersOf(pro, "orders")
			l := r.e.since()
			r.say("D1: %s admitted %d of %d (%.1f/s against its 10/s, burst 20), %d × 429; %s admitted %d of %d, %d × 429",
				free, count(f, is2xx), len(f), float64(count(f, is2xx))/l.Seconds(), count(f, is429), pro, count(p, is2xx), len(p), count(p, is429))
		},
		checks: func(r *runner) {
			f, p := r.answersOf(free, "orders"), r.answersOf(pro, "orders")
			badF := count(f, func(a answer) bool { return !is2xx(a) && !is429(a) })
			badP := count(p, func(a answer) bool { return !is2xx(a) && !is429(a) })
			r.judge("d1.1", "every answer 2xx or (free only) 429", badF == 0 && badP == 0 && count(p, is429) == 0, "only 2xx, and 429 for free-co-mobile",
				"%s: %s; %s: %s", free, statusMix(f), pro, statusMix(p))
			r.judge("d1.2", "pro keeps flowing: no 429 for pro-co-mobile", count(p, is429) == 0 && len(p) > 0, "0 × 429",
				"%d × 429 of %d answers", count(p, is429), len(p))
			w := within(f, from, to)
			admitted := count(w, is2xx)
			lo, hi := 270, 384 // 0.9 × 10/s × 30 s; 1.2 × (20 + 300)
			r.judge("d1.3", "free held at its limit: 2xx of free-co-mobile due in [10 s, 40 s)", count(w, is429) > 0 && admitted >= lo && admitted <= hi,
				fmt.Sprintf("at least one 429 and %d ≤ 2xx ≤ %d", lo, hi), "%d × 2xx (%.1f/s), %d × 429 of %d answers", admitted, float64(admitted)/30, count(w, is429), len(w))
			bad, first := 0, ""
			for _, c := range []struct {
				as             []answer
				plan, degraded int64
			}{{f, 10, 3}, {p, 100, 33}} {
				for _, a := range c.as {
					if why := headerProblem(a, c.plan, c.degraded); why != "" {
						bad++
						if first == "" {
							first = fmt.Sprintf("status %d due %.3f s: %s", a.status, a.due.Seconds(), why)
						}
					}
				}
			}
			if first == "" {
				first = "none"
			}
			r.judge("d1.4", "RateLimit, RateLimit-Policy and Retry-After headers", bad == 0, "every 2xx and 429 with ADR-0018's fields and the plan's q",
				"%d of %d answers wrong; first: %s", bad, len(f)+len(p), first)
		},
	}
}

// headerProblem is what is wrong with a 2xx or 429's rate-limit fields
// (ADR-0098 d1.4), "" if nothing. A Redis decision (no
// Spillway-Redis-Error) carries consumer-plan with q = the plan's rate and
// tenant-ceiling with q = 1500, both w = 1; a failure-mode decision
// carries consumer-plan with q = the per-replica degraded value (ADR-0021),
// and tenant-ceiling, which fails open, only if it was evaluated.
func headerProblem(a answer, plan, degraded int64) string {
	if !is2xx(a) && !is429(a) {
		return ""
	}
	if is429(a) && !retryAfterOK(a.retryAfter) {
		return fmt.Sprintf("Retry-After %q", a.retryAfter)
	}
	pol, ok := parseRL(a.policy)
	if !ok {
		return fmt.Sprintf("RateLimit-Policy %q does not parse", a.policy)
	}
	lim, ok := parseRL(a.rateLimit)
	if !ok {
		return fmt.Sprintf("RateLimit %q does not parse", a.rateLimit)
	}
	failure := a.redisErr != ""
	q := plan
	if failure {
		q = degraded
	}
	cp, ok := item(pol, "consumer-plan")
	if !ok || cp.params["q"] != q || cp.params["w"] != 1 {
		return fmt.Sprintf("RateLimit-Policy %q: want \"consumer-plan\";q=%d;w=1", a.policy, q)
	}
	if it, ok := item(lim, "consumer-plan"); !ok || !hasParams(it, "r", "t") {
		return fmt.Sprintf("RateLimit %q: no consumer-plan item with r and t", a.rateLimit)
	}
	tc, ok := item(pol, "tenant-ceiling")
	switch {
	case !ok && failure:
	case !ok || tc.params["q"] != 1500 || tc.params["w"] != 1:
		return fmt.Sprintf("RateLimit-Policy %q: want \"tenant-ceiling\";q=1500;w=1", a.policy)
	default:
		if it, ok := item(lim, "tenant-ceiling"); !ok || !hasParams(it, "r", "t") {
			return fmt.Sprintf("RateLimit %q: no tenant-ceiling item with r and t", a.rateLimit)
		}
	}
	return ""
}

// retryAfterOK reports whether v is a whole number of seconds ≥ 1.
func retryAfterOK(v string) bool {
	if v == "" || len(v) > 9 {
		return false
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1
}

func hasParams(it rlItem, names ...string) bool {
	for _, n := range names {
		if _, ok := it.params[n]; !ok {
			return false
		}
	}
	return true
}

// scenarioD2 is D2, the noisy neighbour (RL-05, NFR-15): free-co-mobile
// floods orders at 50× its limit while pro-co-mobile and
// enterprise-co-mobile send quietly. The demo form floods from 15 s to
// 45 s, so the quiet tenants' p99 shows with and without it; the check
// form has ADR-0098's 20 (off, on) pairs of 10 s slots after a 10 s
// warm-up.
func scenarioD2() *scenario {
	const noisy = "free-co-mobile"
	quiet := []string{"pro-co-mobile", "enterprise-co-mobile"}
	const slot, warmup, pairs = 10 * time.Second, 10 * time.Second, 20
	floodFrom, floodTo := 15*time.Second, 45*time.Second
	return &scenario{
		name: "d2", title: "noisy neighbour (RL-05, NFR-15)",
		ids: []checkDef{
			{"d2.1", "quiet tenants' p99 flat: median over 20 pairs of p99(on) ÷ p99(off)"},
			{"d2.2", "quiet tenants: every answer 2xx"},
			{"d2.3", "noisy tenant: at least 90% 429 in every on slot"},
			{"d2.4", "every stream sent at least 99% of its schedule"},
		},
		length: fixed(60*time.Second, warmup+2*pairs*slot),
		streams: func(check bool) []streamSpec {
			if check {
				return []streamSpec{
					{consumer: noisy, route: "orders", rate: 500, warmup: warmup, slot: slot, toggle: true},
					{consumer: quiet[0], route: "orders", rate: 50, warmup: warmup, slot: slot},
					{consumer: quiet[1], route: "orders", rate: 100, warmup: warmup, slot: slot},
				}
			}
			return []streamSpec{
				{consumer: noisy, route: "orders", rate: 500, from: floodFrom, to: floodTo},
				{consumer: quiet[0], route: "orders", rate: 50},
				{consumer: quiet[1], route: "orders", rate: 100},
			}
		},
		every: 5 * time.Second,
		tick: func(r *runner, _ int) {
			var parts []string
			for _, c := range append([]string{noisy}, quiet...) {
				t := r.live(c, "orders")
				sh := 0.0
				if t.n > 0 {
					sh = float64(t.limited) / float64(t.n)
				}
				parts = append(parts, fmt.Sprintf("%s 2xx %4d 429 %5.1f%% p99 %s", c, t.ok, 100*sh, ms(t.p99())))
			}
			r.say("%s", strings.Join(parts, " | "))
		},
		run: waitEnd,
		end: func(r *runner) {
			if r.check {
				return
			}
			for _, c := range quiet {
				as := r.answersOf(c, "orders")
				on := within(as, floodFrom+time.Second, floodTo)
				off := append(within(as, 5*time.Second, floodFrom), within(as, floodTo+time.Second, r.e.end)...)
				r.say("D2: %s p99 %s while %s floods, %s while it does not; %d × 429", c, ms(quantile(latencies(on), 0.99)), noisy, ms(quantile(latencies(off), 0.99)), count(as, is429))
			}
			n := r.answersOf(noisy, "orders")
			r.say("D2: %s: %d of %d answers 429 (%s)", noisy, count(n, is429), len(n), pct(share(n, is429)))
		},
		checks: func(r *runner) {
			var ratios []string
			ok1 := true
			for _, c := range quiet {
				ss := r.streamsOf(c, "orders")
				if len(ss) != 1 {
					ok1 = false
					continue
				}
				_, _, res := ss[0].snapshot()
				var rs []float64
				full := len(res.Slots) == 2*pairs
				for k := 0; full && k < pairs; k++ {
					off, on := res.Slots[2*k], res.Slots[2*k+1]
					if off.Requests == 0 || on.Requests == 0 || off.P99 == 0 {
						full = false
						break
					}
					rs = append(rs, float64(on.P99)/float64(off.P99))
				}
				m := median(rs)
				ok1 = ok1 && full && m < 1.10
				pooled := "none"
				if res.Pooled != nil {
					pooled = fmt.Sprintf("pooled p99 off %.1f ms, on %.1f ms", float64(res.Pooled.Even)/1000, float64(res.Pooled.Odd)/1000)
				}
				r.measure("d2_ratios_"+c, rs)
				ratios = append(ratios, fmt.Sprintf("%s median %.3f over %d pairs (%s)", c, m, len(rs), pooled))
			}
			r.judge("d2.1", "quiet tenants' p99 flat: median over 20 pairs of p99(on) ÷ p99(off)", ok1, "all 20 pairs answered and the median < 1.10 for each", "%s", strings.Join(ratios, "; "))
			var mix []string
			ok2 := true
			for _, c := range quiet {
				as := r.answersOf(c, "orders")
				ok2 = ok2 && len(as) > 0 && count(as, is2xx) == len(as)
				mix = append(mix, c+": "+statusMix(as))
			}
			r.judge("d2.2", "quiet tenants: every answer 2xx", ok2, "only 2xx", "%s", strings.Join(mix, "; "))
			as := r.answersOf(noisy, "orders")
			worst, worstSlot, slots := 1.0, -1, 0
			for k := 1; k < 2*pairs; k += 2 {
				w := within(as, warmup+time.Duration(k)*slot, warmup+time.Duration(k+1)*slot)
				s := share(w, is429)
				slots++
				if s < worst {
					worst, worstSlot = s, k
				}
			}
			r.judge("d2.3", "noisy tenant: at least 90% 429 in every on slot", worst >= 0.9, "≥ 90% in each of the 20 on slots",
				"lowest %s (slot %d) over %d on slots; whole run %d of %d answers 429", pct(worst), worstSlot, slots, count(as, is429), len(as))
			judgeSent(r, "d2.4")
		},
	}
}

// judgeSent judges that every stream sent at least 99% of its schedule
// (loadgen's rule, ADR-0014).
func judgeSent(r *runner, id string) {
	r.e.mu.Lock()
	ss := slices.Clone(r.e.streams)
	r.e.mu.Unlock()
	ok, worst, worstName := true, 2.0, ""
	for _, s := range ss {
		_, _, res := s.snapshot()
		f := 0.0
		if res.Requests.Scheduled > 0 {
			f = float64(res.Requests.Sent) / float64(res.Requests.Scheduled)
		}
		if f < worst {
			worst, worstName = f, s.name
		}
		ok = ok && f >= 0.99
	}
	r.judge(id, "every stream sent at least 99% of its schedule", ok, "≥ 99% for every stream", "lowest %s (%s) of %d streams", pct(worst), worstName, len(ss))
}

// scenarioD3 is D3, distributed accuracy (RL-11 – RL-14): pro-co-mobile
// at 2× payments-limit's 100/s (burst 200, fail_closed) round robin over
// the three replicas; the admitted rate tracks the limit line, and
// ADR-0021's check finds no over-admission on Redis time.
func scenarioD3() *scenario {
	const c = "pro-co-mobile"
	limit := gcraLimit{rate: 100, period: time.Second, burst: 200}
	admitted := func(r *runner) []int64 {
		var ts []int64
		for _, a := range r.answersOf(c, "payments") {
			if is2xx(a) && decided(a) {
				ts = append(ts, a.decisionUS)
			}
		}
		slices.Sort(ts)
		return ts
	}
	return &scenario{
		name: "d3", title: "distributed accuracy (RL-11 – RL-14)",
		ids: []checkDef{
			{"d3.1", "zero over-admission on Redis time (ADR-0021)"},
			{"d3.2", "every answer 2xx, 429 or 503"},
			{"d3.3", "admitted rate tracks the limit line: 2xx over 60 s"},
			{"d3.4", "at least 99% of the schedule sent"},
		},
		length: fixed(60*time.Second, 60*time.Second),
		streams: func(bool) []streamSpec {
			return []streamSpec{{consumer: c, route: "payments", rate: 200}}
		},
		tick: func(r *runner, _ int) {
			t := r.live(c, "payments")
			var split []string
			for i, g := range r.e.gws {
				n := int64(0)
				if i < len(t.okBy) {
					n = t.okBy[i]
				}
				split = append(split, fmt.Sprintf("%s %d", g.name, n))
			}
			r.say("%s payments: admitted %3d/s (limit line 100/s)  429 %3d  503 %d  |  %s", c, t.ok, t.limited, t.unavailable, strings.Join(split, "  "))
		},
		run: waitEnd,
		end: func(r *runner) {
			ts := admitted(r)
			most, over, overW := windowMax(ts, limit)
			var parts []string
			for _, w := range accuracyWindows {
				parts = append(parts, fmt.Sprintf("%v: most %d (bound %d)", w, most[w], limit.bound(w)))
			}
			r.say("D3 accuracy (ADR-0021, Redis time): %s; over-admission %d%s", strings.Join(parts, ", "), over, map[bool]string{true: " in a " + overW.String() + " window", false: ""}[over > 0])
			as := r.answersOf(c, "payments")
			r.say("D3: %d admitted in %.0f s (%.1f/s against 100/s); per replica %s", count(as, is2xx), r.e.end.Seconds(), float64(count(as, is2xx))/r.e.end.Seconds(), replicaSplit(r, as))
		},
		checks: func(r *runner) {
			as := r.answersOf(c, "payments")
			ts := admitted(r)
			ok2xx := count(as, is2xx)
			most, over, overW := windowMax(ts, limit)
			var ws []string
			for _, w := range accuracyWindows {
				ws = append(ws, fmt.Sprintf("%v %d/%d", w, most[w], limit.bound(w)))
			}
			overAt := "none over"
			if over > 0 {
				overAt = fmt.Sprintf("%d over in a %v window", over, overW)
			}
			r.judge("d3.1", "zero over-admission on Redis time (ADR-0021)", over == 0 && len(ts) == ok2xx && ok2xx > 0, "every 2xx with Spillway-Decision-Time and 0 over the bound",
				"%d admissions, %d without Spillway-Decision-Time; most per window (admitted/bound): %s; %s", ok2xx, ok2xx-len(ts), strings.Join(ws, ", "), overAt)
			bad := count(as, func(a answer) bool { return !is2xx(a) && !is429(a) && !is503(a) })
			r.judge("d3.2", "every answer 2xx, 429 or 503", bad == 0 && len(as) > 0, "no other status and no unanswered request", "%s (503s are printed, not judged)", statusMix(as))
			r.judge("d3.3", "admitted rate tracks the limit line: 2xx over 60 s", ok2xx >= 6000, "≥ 6,000 (100/s × 60 s; the bound allows 6,200)", "%d", ok2xx)
			judgeSent(r, "d3.4")
			r.say("D3: answers per replica %s", replicaSplit(r, as))
		},
	}
}

// replicaSplit is the answers per replica.
func replicaSplit(r *runner, as []answer) string {
	n := make([]int, len(r.e.gws))
	for _, a := range as {
		if a.replica >= 0 && a.replica < len(n) {
			n[a.replica]++
		}
	}
	var parts []string
	for i, g := range r.e.gws {
		parts = append(parts, fmt.Sprintf("%s %d", g.name, n[i]))
	}
	return strings.Join(parts, ", ")
}
