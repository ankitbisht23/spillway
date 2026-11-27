package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The scenarios with actions: D4's rollout through the REST API, D5's and
// D6's faults through setDemoFault, D7's heavy hitters and block.

// publishStep is one D4 publish: its version, when it was sent and when
// every replica had applied it (from time 0).
type publishStep struct {
	what          string
	version       int64
	sent, applied time.Duration
}

func (p publishStep) took() time.Duration { return p.applied - p.sent }

// validated validates the working set and returns its revision's ETag;
// with mustBeValid, a working set that does not validate is an error (a
// rollback replaces the working set, so it needs only the revision).
func validated(ctx context.Context, r *runner, mustBeValid bool) (string, error) {
	var v struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	a, err := r.cp.Must(ctx, http.StatusOK, http.MethodPost, "/config/validate", nil, &v)
	if err != nil {
		return "", err
	}
	if !v.Valid && mustBeValid {
		var msgs []string
		for _, e := range v.Errors {
			msgs = append(msgs, e.Path+": "+e.Message)
		}
		return "", fmt.Errorf("the working set does not validate: %s", strings.Join(msgs, "; "))
	}
	return a.Header.Get("ETag"), nil
}

// publish validates and publishes the working set with note (or, with
// rollbackTo > 0, rolls back to that version), then polls the version
// every 100 ms until every replica has applied it (ADR-0042).
func publish(ctx context.Context, r *runner, what, note string, rollbackTo int64) (publishStep, error) {
	etag, err := validated(ctx, r, rollbackTo == 0)
	if err != nil {
		return publishStep{}, err
	}
	step := publishStep{what: what, sent: r.e.since()}
	path := "/config/publish"
	if rollbackTo > 0 {
		path = "/config/versions/" + strconv.FormatInt(rollbackTo, 10) + "/rollback"
	}
	var v struct {
		Version int64 `json:"version"`
	}
	if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodPost, path, map[string]string{"note": note}, &v, "If-Match", etag); err != nil {
		return step, err
	}
	step.version = v.Version
	deadline := time.Now().Add(30 * time.Second)
	for {
		var cv struct {
			Applied []struct {
				Replica string `json:"replica"`
			} `json:"applied"`
		}
		if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/config/versions/"+strconv.FormatInt(v.Version, 10), nil, &cv); err != nil {
			return step, err
		}
		n := 0
		for _, g := range r.e.gws {
			if slices.ContainsFunc(cv.Applied, func(a struct {
				Replica string `json:"replica"`
			}) bool {
				return a.Replica == g.name
			}) {
				n++
			}
		}
		if n == len(r.e.gws) {
			step.applied = r.e.since()
			return step, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return step, fmt.Errorf("version %d not applied on every replica within 30 s (%d of %d)", v.Version, n, len(r.e.gws))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// latestVersion is the latest published config version.
func latestVersion(ctx context.Context, r *runner) (int64, error) {
	var list struct {
		Items []struct {
			Version int64 `json:"version"`
		} `json:"items"`
	}
	if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/config/versions?limit=1", nil, &list); err != nil {
		return 0, err
	}
	if len(list.Items) == 0 {
		return 0, fmt.Errorf("no published config version (make demo-reset publishes the demo's)")
	}
	return list.Items[0].Version, nil
}

// shadowReport is the part of getShadowReport D4 reads.
type shadowReport struct {
	WouldDeny        *float64 `json:"would_deny"`
	KeysShadowDenied int64    `json:"keys_shadow_denied_total"`
	Keys             []struct {
		ConsumerID   *int64 `json:"consumer_id"`
		APIKeyID     *int64 `json:"api_key_id"`
		ShadowDenied int64  `json:"shadow_denied"`
	} `json:"keys"`
}

// consumerName names a consumer ID by the run's keys.
func consumerName(r *runner, id *int64) string {
	if id == nil {
		return "(none)"
	}
	for _, c := range append(slices.Clone(baseConsumers), "free-co-mobile", "pro-co-mobile", "enterprise-co-mobile", "free-co-partner") {
		if k := r.e.keys.current(c); k != nil && k.consumerID == *id {
			return c
		}
	}
	return "consumer #" + strconv.FormatInt(*id, 10)
}

func idString(id *int64) string {
	if id == nil {
		return "none"
	}
	return "#" + strconv.FormatInt(*id, 10)
}

// scenarioD4 is D4, safe rollout (RL-26, UI-01, CP-04), on the base load
// alone: a stricter policy in shadow, its report naming who would be hit,
// promote to enforce, roll back.
func scenarioD4() *scenario {
	var mu sync.Mutex
	var steps []publishStep
	var shadow, promote, rollback publishStep
	var rep shadowReport
	addStep := func(r *runner, s publishStep) {
		mu.Lock()
		steps = append(steps, s)
		mu.Unlock()
		var gws []string
		for _, g := range r.e.gws {
			gws = append(gws, g.name)
		}
		r.say("D4 %s: version %d applied on %s %s after the publish", s.what, s.version, strings.Join(gws, ", "), secs(s.took()))
	}
	return &scenario{
		name: "d4", title: "safe rollout (RL-26, UI-01, CP-04)",
		ids: []checkDef{
			{"d4.1", "each publish applied on every replica within 5 s"},
			{"d4.2", "shadow never rejects: no 429 for the base consumers"},
			{"d4.3", "the shadow report names who would be hit"},
			{"d4.4", "enforce: enterprise-co-web throttled, the others not"},
			{"d4.5", "after the rollback: no 429, and the policy gone"},
			{"d4.6", "the audit log has the three publishes"},
		},
		length:  fixed(90*time.Second, 150*time.Second),
		streams: func(bool) []streamSpec { return nil },
		tick: func(r *runner, _ int) {
			var parts []string
			for _, c := range baseConsumers {
				t := r.live(c, "orders")
				parts = append(parts, fmt.Sprintf("%s 2xx %3d 429 %3d", c, t.ok, t.limited))
			}
			r.say("orders: %s", strings.Join(parts, " | "))
		},
		run: func(r *runner) (err error) {
			ctx := r.ctx
			name, notePrefix := "orders-guard", "D4"
			if r.check {
				name, notePrefix = "orders-guard-"+r.runID, "verify-m8 D4"
			}
			v0, err := latestVersion(ctx, r)
			if err != nil {
				return err
			}
			r.measure("d4_version_before", v0)
			// 0 s: the stricter policy, in shadow.
			if !r.e.sleepUntil(ctx, 0) {
				return ctx.Err()
			}
			body := map[string]any{
				"name": name, "level": "route", "match": map[string]any{"routes": []string{"orders"}}, "key_template": "{consumer}",
				"algorithm": "gcra", "limits": []map[string]any{{"rate": 50, "period": "1s", "burst": 50}}, "mode": "shadow", "failure_mode": "fail_open",
			}
			var pol struct {
				ID int64 `json:"id"`
			}
			if _, err := r.cp.Must(ctx, http.StatusCreated, http.MethodPost, "/policies", body, &pol); err != nil {
				if strings.Contains(err.Error(), "status 409") {
					return fmt.Errorf("%w (a policy %s is left from an interrupted D4: make demo-reset removes it)", err, name)
				}
				return err
			}
			rolledBack := false
			defer func() {
				if rolledBack {
					return
				}
				// Never leave the stricter policy behind: back to v0.
				cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if _, rerr := publish(cctx, r, "clean-up rollback", notePrefix+" rollback (clean-up) "+r.runID, v0); rerr != nil {
					r.say("D4: could not roll back to version %d: %v (make demo-reset restores the seed)", v0, rerr)
				} else {
					r.say("D4: rolled back to version %d", v0)
				}
			}()
			r.say("D4: policy %s (orders, per consumer, GCRA 50/s burst 50) created in shadow", name)
			if shadow, err = publish(ctx, r, "shadow", notePrefix+" shadow "+r.runID, 0); err != nil {
				return err
			}
			addStep(r, shadow)
			// The report: who would be hit.
			reportAt, promoteAt := 40*time.Second, 45*time.Second
			if r.check {
				reportAt, promoteAt = 60*time.Second, 60*time.Second
			}
			if !r.e.sleepUntil(ctx, reportAt) {
				return ctx.Err()
			}
			if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/policies/"+strconv.FormatInt(pol.ID, 10)+"/shadow-report?window=15m", nil, &rep); err != nil {
				return err
			}
			wd := "unknown (no Prometheus)"
			if rep.WouldDeny != nil {
				wd = fmt.Sprintf("%.0f", *rep.WouldDeny)
			}
			r.say("D4 shadow report (15 min): %s would-be denials; %d keys with %d would-be denials in heavy hitters; keys that would be hit:", wd, len(rep.Keys), rep.KeysShadowDenied)
			for _, k := range rep.Keys {
				if k.ShadowDenied > 0 {
					r.say("  %s, key %s: %d would-be denials", consumerName(r, k.ConsumerID), idString(k.APIKeyID), k.ShadowDenied)
				}
			}
			// Promote to enforce.
			if !r.e.sleepUntil(ctx, promoteAt) {
				return ctx.Err()
			}
			a, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/policies/"+strconv.FormatInt(pol.ID, 10), nil, nil)
			if err != nil {
				return err
			}
			body["mode"] = "enforce"
			if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodPut, "/policies/"+strconv.FormatInt(pol.ID, 10), body, nil, "If-Match", a.Header.Get("ETag")); err != nil {
				return err
			}
			if promote, err = publish(ctx, r, "promote", notePrefix+" promote "+r.runID, 0); err != nil {
				return err
			}
			addStep(r, promote)
			// Roll back.
			rollbackAt := 75 * time.Second
			if r.check {
				rollbackAt = promote.applied + 21*time.Second
			}
			if !r.e.sleepUntil(ctx, rollbackAt) {
				return ctx.Err()
			}
			rollback, err = publish(ctx, r, "rollback", fmt.Sprintf("%s rollback %s", notePrefix, r.runID), v0)
			if err != nil {
				return err
			}
			rolledBack = true
			addStep(r, rollback)
			end := r.e.end
			if r.check {
				end = rollback.applied + 11*time.Second
			}
			r.e.sleepUntil(ctx, end)
			return nil
		},
		end: func(r *runner) {
			for _, c := range baseConsumers {
				as := r.answersOf(c, "")
				r.say("D4: %s %d answers: shadow %d × 429, enforce %d × 429, after the rollback %d × 429", c, len(as),
					count(within(as, shadow.applied, promote.sent), is429), count(within(as, promote.applied, rollback.sent), is429), count(within(as, rollback.applied, r.e.end), is429))
			}
		},
		checks: func(r *runner) {
			ctx := r.ctx
			var took []string
			ok := len(steps) == 3
			for _, s := range steps {
				took = append(took, fmt.Sprintf("%s v%d %s", s.what, s.version, secs(s.took())))
				ok = ok && s.took() <= 5*time.Second
			}
			r.judge("d4.1", "each publish applied on every replica within 5 s", ok, "3 publishes, each ≤ 5 s", "%s", strings.Join(took, ", "))
			n429 := 0
			for _, c := range baseConsumers {
				n429 += count(within(r.answersOf(c, ""), shadow.applied, promote.sent), is429)
			}
			r.judge("d4.2", "shadow never rejects: no 429 for the base consumers", n429 == 0, "0 × 429 from shadow applied to promote", "%d × 429 in [%s, %s)", n429, secs(shadow.applied), secs(promote.sent))
			ent := r.e.keys.current("enterprise-co-web")
			var hit []string
			only := rep.WouldDeny != nil && *rep.WouldDeny > 0
			nHit := 0
			for _, k := range rep.Keys {
				if k.ShadowDenied == 0 {
					continue
				}
				nHit++
				hit = append(hit, fmt.Sprintf("%s key %s (%d)", consumerName(r, k.ConsumerID), idString(k.APIKeyID), k.ShadowDenied))
				only = only && k.APIKeyID != nil && k.ConsumerID != nil && ent != nil && *k.APIKeyID == ent.id && *k.ConsumerID == ent.consumerID
			}
			wd := "null"
			if rep.WouldDeny != nil {
				wd = fmt.Sprintf("%.0f", *rep.WouldDeny)
			}
			r.judge("d4.3", "the shadow report names who would be hit", only && nHit == 1, "would_deny > 0 and exactly one key: enterprise-co-web's", "would_deny %s; keys %s", wd, strings.Join(hit, ", "))
			w0, w1 := promote.applied+time.Second, promote.applied+21*time.Second
			e := within(r.answersOf("enterprise-co-web", "orders"), w0, w1)
			others := count(within(r.answersOf("free-co-web", ""), w0, w1), is429) + count(within(r.answersOf("pro-co-web", ""), w0, w1), is429)
			r.judge("d4.4", "enforce: enterprise-co-web throttled, the others not", len(e) > 0 && share(e, is429) >= 0.5 && others == 0,
				"≥ 50% of enterprise-co-web's orders 429, 0 × 429 for free-co-web and pro-co-web", "enterprise-co-web %s of %d orders answers 429; free-co-web and pro-co-web %d × 429", pct(share(e, is429)), len(e), others)
			p0, p1 := rollback.applied+time.Second, rollback.applied+11*time.Second
			post := 0
			for _, c := range baseConsumers {
				post += count(within(r.answersOf(c, ""), p0, p1), is429)
			}
			gone, gerr := policyGone(ctx, r)
			r.judge("d4.5", "after the rollback: no 429, and the policy gone", post == 0 && gone && gerr == nil, "0 × 429 and no "+fmt.Sprintf("orders-guard-%s", r.runID)+" in the latest snapshot",
				"%d × 429 in [%s, %s); policy in the latest snapshot: %v%s", post, secs(p0), secs(p1), !gone, errSuffix(gerr))
			found, aerr := auditNotes(ctx, r, "config", []string{"verify-m8 D4 shadow " + r.runID, "verify-m8 D4 promote " + r.runID, "verify-m8 D4 rollback " + r.runID})
			r.judge("d4.6", "the audit log has the three publishes", found == 3 && aerr == nil, "3 records with the notes", "%d of 3 found%s", found, errSuffix(aerr))
		},
	}
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

// policyGone reports whether the latest published snapshot lacks this
// run's orders-guard policy.
func policyGone(ctx context.Context, r *runner) (bool, error) {
	v, err := latestVersion(ctx, r)
	if err != nil {
		return false, err
	}
	var cv struct {
		Snapshot struct {
			Policies []struct {
				Name string `json:"name"`
			} `json:"policies"`
		} `json:"snapshot"`
	}
	if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/config/versions/"+strconv.FormatInt(v, 10), nil, &cv); err != nil {
		return false, err
	}
	for _, p := range cv.Snapshot.Policies {
		if p.Name == "orders-guard-"+r.runID {
			return false, nil
		}
	}
	return true, nil
}

// auditRecord is the part of an audit record the checks read.
type auditRecord struct {
	EntityID *string `json:"entity_id"`
	Note     *string `json:"note"`
	Actor    struct {
		Name string `json:"name"`
	} `json:"actor"`
}

// auditSince lists entity's audit records since time 0 minus a minute.
func auditSince(ctx context.Context, r *runner, entity string) ([]auditRecord, error) {
	q := url.Values{"entity": {entity}, "since": {r.e.t0.Add(-time.Minute).UTC().Format(time.RFC3339)}, "limit": {"200"}}
	var list struct {
		Items []auditRecord `json:"items"`
	}
	_, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/audit?"+q.Encode(), nil, &list)
	return list.Items, err
}

// auditNotes counts the notes found among entity's records.
func auditNotes(ctx context.Context, r *runner, entity string, notes []string) (int, error) {
	items, err := auditSince(ctx, r, entity)
	if err != nil {
		return 0, err
	}
	found := 0
	for _, n := range notes {
		if slices.ContainsFunc(items, func(a auditRecord) bool { return a.Note != nil && *a.Note == n }) {
			found++
		}
	}
	return found, nil
}

// scenarioD5 is D5, the Redis failure (RL-22 – RL-24): pro-co-mobile on
// catalog (fail_open) and payments (fail_closed) round robin, and on orders
// (degraded) at 2× its per-replica 33/s to each replica, while redis-cut
// black-holes the demo gateways' Redis path.
func scenarioD5() *scenario {
	const c = "pro-co-mobile"
	var mu sync.Mutex
	var tc, to time.Duration // the cut: on returned, off sent
	var s0, s1 []scrape      // at Tc and at To
	var liveN, liveOf int
	cut := func() (time.Duration, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		return tc, to
	}
	return &scenario{
		name: "d5", title: "Redis failure (RL-22 – RL-24)",
		ids: []checkDef{
			{"d5.0", "before the cut: three live replicas, Redis decides"},
			{"d5.1", "fail-open: catalog keeps serving in the cut"},
			{"d5.2", "fail-closed: payments answers 503 in the cut"},
			{"d5.3", "degraded: orders admitted per replica at limit ÷ 3"},
			{"d5.4", "within one budget, client side: median latency"},
			{"d5.5", "within one budget, gateway side: Redis checks ≤ 10 ms"},
			{"d5.6", "every Redis error in the cut is timeout or conn"},
			{"d5.7", "recovery: Redis decides again"},
			{"d5.8", "every stream sent at least 99% of its schedule"},
		},
		length: fixed(45*time.Second, 50*time.Second),
		streams: func(bool) []streamSpec {
			return []streamSpec{
				{consumer: c, route: "catalog", rate: 100},
				{consumer: c, route: "payments", rate: 10},
				{consumer: c, route: "orders", rate: 66, replica: 1},
				{consumer: c, route: "orders", rate: 66, replica: 2},
				{consumer: c, route: "orders", rate: 66, replica: 3},
			}
		},
		tick: func(r *runner, _ int) {
			cat, pay := r.live(c, "catalog"), r.live(c, "payments")
			var per []string
			for i, g := range r.e.gws {
				t := newTally()
				for _, s := range r.streamsOf(c, "orders") {
					if s.spec.replica == i+1 {
						t.merge(s.takeLive())
					}
				}
				per = append(per, fmt.Sprintf("%s %d", g.name, t.ok))
			}
			state := ""
			if a, b := cut(); a > 0 && b == 0 {
				state = "[Redis cut] "
			}
			r.say("%scatalog 2xx %3d 503 %d | payments 2xx %2d 503 %2d | orders admitted per replica: %s (degraded line 33/s)", state, cat.ok, cat.unavailable, pay.ok, pay.unavailable, strings.Join(per, ", "))
		},
		run: func(r *runner) error {
			ctx := r.ctx
			var reps struct {
				Items []struct {
					ID   string `json:"id"`
					Live bool   `json:"live"`
				} `json:"items"`
			}
			if _, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/replicas", nil, &reps); err != nil {
				return err
			}
			for _, it := range reps.Items {
				if slices.ContainsFunc(r.e.gws, func(g gateway) bool { return g.name == it.ID }) && it.Live {
					liveN++
				}
			}
			liveOf = len(reps.Items)
			if !r.e.sleepUntil(ctx, 10*time.Second) {
				return ctx.Err()
			}
			if err := r.setFault(ctx, "redis-cut", true); err != nil {
				return err
			}
			mu.Lock()
			tc = r.e.since()
			mu.Unlock()
			r.say("redis-cut: on: the demo gateways' Redis traffic is black-holed at toxiproxy")
			s0 = scrapeAll(ctx, r.scrape, r.e.gws)
			off := 30 * time.Second
			if r.check {
				off = tc + 20*time.Second
			}
			if !r.e.sleepUntil(ctx, off) {
				return ctx.Err()
			}
			s1 = scrapeAll(ctx, r.scrape, r.e.gws)
			mu.Lock()
			to = r.e.since()
			mu.Unlock()
			if err := r.setFault(ctx, "redis-cut", false); err != nil {
				return err
			}
			r.say("redis-cut: off")
			r.e.sleepUntil(ctx, r.e.end)
			return nil
		},
		end: func(r *runner) {
			a, b := cut()
			if a == 0 || b == 0 {
				return
			}
			cat, pay := within(r.answersOf(c, "catalog"), a, b), within(r.answersOf(c, "payments"), a, b)
			r.say("D5 in the cut (%.0f s): catalog %s 2xx (fail-open), payments %s 503 (fail-closed)", (b - a).Seconds(), pct(share(cat, is2xx)), pct(share(pay, is503)))
			for i, g := range r.e.gws {
				o := within(answersOfReplica(r, c, "orders", i+1), a, b)
				r.say("D5 in the cut: orders on %s admitted %d: %.1f/s after its burst of 66, against 33/s (degraded, limit ÷ 3 live replicas)", g.name, count(o, is2xx), float64(count(o, is2xx)-66)/(b-a).Seconds())
			}
			after := within(r.answersOf(c, ""), b+5*time.Second, b+15*time.Second)
			r.say("D5 recovery: %s of the answers due 5–15 s after the cut decided by Redis again", pct(share(after, decided)))
		},
		checks: func(r *runner) {
			a, b := cut()
			cat, pay := r.answersOf(c, "catalog"), r.answersOf(c, "payments")
			before := 10 * time.Second
			dc, dp := share(within(cat, 0, before), decided), share(within(pay, 0, before), decided)
			r.judge("d5.0", "before the cut: three live replicas, Redis decides", liveN == 3 && dc >= 0.9 && dp >= 0.9, "3 live replicas; ≥ 90% of catalog and payments decided by Redis in [0, 10 s)",
				"%d of %d replicas live; Redis decided catalog %s, payments %s", liveN, liveOf, pct(dc), pct(dp))
			if a == 0 || b == 0 {
				for _, d := range []checkDef{{"d5.1", ""}, {"d5.2", ""}, {"d5.3", ""}, {"d5.4", ""}, {"d5.5", ""}, {"d5.6", ""}, {"d5.7", ""}} {
					r.judge(d.id, "the cut", false, "a cut", "setup: the fault was not switched")
				}
				judgeSent(r, "d5.8")
				return
			}
			cc, pc := within(cat, a, b), within(pay, a, b)
			r.judge("d5.1", "fail-open: catalog keeps serving in the cut", share(cc, is2xx) >= 0.999, "≥ 99.9% 2xx", "%s of %d (%s)", pct(share(cc, is2xx)), len(cc), statusMix(cc))
			okGrow := len(s0) == len(r.e.gws) && len(s1) == len(r.e.gws)
			var grow []string
			for i, g := range r.e.gws {
				if !okGrow || !s0[i].ok || !s1[i].ok {
					okGrow = false
					grow = append(grow, g.name+" not read")
					continue
				}
				d := s1[i].sum("gw_requests_total", "route", "payments", "reason", "fail_closed") - s0[i].sum("gw_requests_total", "route", "payments", "reason", "fail_closed")
				lo, hi := s0[i].recv.Sub(r.e.t0), s1[i].sent.Sub(r.e.t0)
				mine := count(pay, func(x answer) bool { return x.replica == i && is503(x) && x.due >= lo && x.done <= hi })
				okGrow = okGrow && d >= float64(mine)
				grow = append(grow, fmt.Sprintf("%s +%.0f (its share %d)", g.name, d, mine))
			}
			r.judge("d5.2", "fail-closed: payments answers 503 in the cut", len(pc) > 0 && count(pc, is503) == len(pc) && okGrow, "every payments answer 503; each replica's fail_closed count grew by its share",
				"%s; gw_requests_total{route=\"payments\",reason=\"fail_closed\"}: %s", statusMix(pc), strings.Join(grow, ", "))
			okDeg := true
			var deg []string
			l := (b - a).Seconds()
			for i, g := range r.e.gws {
				o := within(answersOfReplica(r, c, "orders", i+1), a, b)
				rate := float64(count(o, is2xx)-66) / l
				okDeg = okDeg && rate >= 0.9*33 && rate <= 1.1*33
				deg = append(deg, fmt.Sprintf("%s %.1f/s", g.name, rate))
			}
			r.judge("d5.3", "degraded: orders admitted per replica at limit ÷ 3", okDeg, "(2xx − 66) ÷ 20 s within 33/s ± 10%", "%s over %.1f s", strings.Join(deg, ", "), l)
			okLat := true
			var lat []string
			for _, route := range []string{"catalog", "payments", "orders"} {
				as := r.answersOf(c, route)
				m0, m1 := quantile(latencies(within(as, 0, before)), 0.5), quantile(latencies(within(as, a, b)), 0.5)
				okLat = okLat && m1 <= m0+10*time.Millisecond
				lat = append(lat, fmt.Sprintf("%s %s → %s", route, ms(m0), ms(m1)))
			}
			r.judge("d5.4", "within one budget, client side: median latency", okLat, "each route's median in the cut ≤ its median before + 10 ms", "%s", strings.Join(lat, ", "))
			okBud := len(s0) == len(r.e.gws) && len(s1) == len(r.e.gws)
			var bud []string
			script := 0.0
			for i, g := range r.e.gws {
				if !okBud || !s0[i].ok || !s1[i].ok {
					okBud = false
					continue
				}
				n := s1[i].sum("gw_ratelimit_check_duration_seconds_count", "backend", "redis") - s0[i].sum("gw_ratelimit_check_duration_seconds_count", "backend", "redis")
				f := s1[i].sum("gw_ratelimit_check_duration_seconds_bucket", "backend", "redis", "le", "0.01") - s0[i].sum("gw_ratelimit_check_duration_seconds_bucket", "backend", "redis", "le", "0.01")
				okBud = okBud && n > 0 && f/n >= 0.99
				bud = append(bud, fmt.Sprintf("%s %.0f of %.0f (%s)", g.name, f, n, pct(f/max(n, 1))))
				script += s1[i].sum("gw_redis_errors_total", "kind", "script") - s0[i].sum("gw_redis_errors_total", "kind", "script")
			}
			r.judge("d5.5", "within one budget, gateway side: Redis checks ≤ 10 ms", okBud, "≥ 99% of each replica's Redis checks in le=\"0.01\"", "%s", strings.Join(bud, ", "))
			kinds := map[string]int{}
			for _, x := range within(r.answersOf(c, ""), a, b) {
				for k := range strings.SplitSeq(x.redisErr, ",") {
					if k != "" {
						kinds[k]++
					}
				}
			}
			okKinds := len(kinds) > 0 && okBud
			for k := range kinds {
				okKinds = okKinds && (k == "timeout" || k == "conn")
			}
			r.judge("d5.6", "every Redis error in the cut is timeout or conn", okKinds && script == 0, "only timeout and conn; gw_redis_errors_total{kind=\"script\"} unchanged",
				"Spillway-Redis-Error kinds %v; script errors +%.0f", kinds, script)
			rc, rp := within(cat, b+5*time.Second, b+15*time.Second), within(pay, b+5*time.Second, b+15*time.Second)
			r.judge("d5.7", "recovery: Redis decides again", len(rc) > 0 && len(rp) > 0 && share(rc, decided) >= 0.9 && share(rp, decided) >= 0.9, "≥ 90% of catalog and payments decided by Redis in [To + 5 s, To + 15 s)",
				"catalog %s, payments %s", pct(share(rc, decided)), pct(share(rp, decided)))
			judgeSent(r, "d5.8")
		},
	}
}

// answersOfReplica returns the answers of consumer's stream on route that
// goes to replica n only.
func answersOfReplica(r *runner, consumer, route string, n int) []answer {
	var out []answer
	for _, s := range r.streamsOf(consumer, route) {
		if s.spec.replica == n {
			as, _, _ := s.snapshot()
			out = append(out, as...)
		}
	}
	return out
}

// scraper reads every replica's metrics every second from its start until
// stopped (ADR-0031, ADR-0098 D6).
type scraper struct {
	mu   sync.Mutex
	all  [][]scrape // per replica
	stop chan struct{}
	done chan struct{}
}

func startScraper(r *runner) *scraper {
	sc := &scraper{all: make([][]scrape, len(r.e.gws)), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(sc.done)
		first := time.Now()
		for k := 0; ; k++ {
			select {
			case <-sc.stop:
				return
			case <-r.ctx.Done():
				return
			case <-time.After(time.Until(first.Add(time.Duration(k) * time.Second))):
			}
			ms := scrapeAll(r.ctx, r.scrape, r.e.gws)
			sc.mu.Lock()
			for i := range ms {
				sc.all[i] = append(sc.all[i], ms[i])
			}
			sc.mu.Unlock()
		}
	}()
	return sc
}

func (sc *scraper) halt() {
	select {
	case <-sc.stop:
	default:
		close(sc.stop)
	}
	<-sc.done
}

// series returns replica i's scrapes so far.
func (sc *scraper) series(i int) []scrape {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return slices.Clone(sc.all[i])
}

// retries and firsts are D6's R and Q (ADR-0101 §13).
func retries(m scrape) float64 {
	return m.sum("gw_retries_total", "upstream", "orders", "outcome", "sent")
}

func exhausted(m scrape) float64 {
	return m.sum("gw_retries_total", "upstream", "orders", "outcome", "budget_exhausted")
}

func firsts(m scrape) float64 {
	return m.sum("gw_upstream_duration_seconds_count", "upstream", "orders", "attempt", "1")
}

// budgetResult is ADR-0101 §13's rule on one replica's scrapes.
type budgetResult struct {
	windows, over int
	tightest      float64 // the largest ΔR ÷ (0.2 ΔQ + 30)
	gaps, failed  int     // consecutive scrapes over 2 s apart; unanswered scrapes
}

// budgetRule checks ADR-0101 §13 (amending ADR-0098 d6.3): for answered
// scrapes i < j with recv_j − send_i ≤ 9.9 s, ΔR(i, j) ≤ 0.2 × ΔQ(k, j) +
// 30, k the latest answered scrape with send_i − recv_k ≥ 10 s (pairs
// without one are not checked); a window is a scrape j with a checked pair
// whose send_j − recv_i ≥ 8 s.
func budgetRule(ms []scrape) budgetResult {
	var b budgetResult
	for k, m := range ms {
		if !m.ok {
			b.failed++
		}
		if k > 0 && m.sent.Sub(ms[k-1].sent) > 2*time.Second {
			b.gaps++
		}
	}
	for j, mj := range ms {
		counted := false
		for i, mi := range ms[:j] {
			if !mi.ok || !mj.ok || mj.recv.Sub(mi.sent) > 9900*time.Millisecond {
				continue
			}
			k := -1
			for c, mc := range ms[:i] {
				if mc.ok && mi.sent.Sub(mc.recv) >= 10*time.Second {
					k = c
				}
			}
			if k < 0 {
				continue
			}
			dr, dq := retries(mj)-retries(mi), firsts(mj)-firsts(ms[k])
			if 5*dr > dq+150 {
				b.over++
			}
			b.tightest = max(b.tightest, dr/(dq/5+30))
			if !counted && mj.sent.Sub(mi.recv) >= 8*time.Second {
				b.windows++
				counted = true
			}
		}
	}
	return b
}

// scenarioD6 is D6, upstream protection (GW-13, GW-14): pro-co-mobile on
// orders while upstream-slow adds 4 s to every orders endpoint (over the
// route's 2.5 s per-try timeout); the breakers open, retries stay within
// the budget, the breakers close after the fault.
func scenarioD6() *scenario {
	const c = "pro-co-mobile"
	var mu sync.Mutex
	var tsSent, ts, te time.Duration
	var scr *scraper
	times := func() (time.Duration, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		return ts, te
	}
	scraperOf := func() *scraper {
		mu.Lock()
		defer mu.Unlock()
		return scr
	}
	allOrders := func(r *runner) []answer {
		var out []answer
		for _, cn := range append(slices.Clone(baseConsumers), c) {
			out = append(out, r.answersOf(cn, "orders")...)
		}
		return out
	}
	circuitName := map[float64]string{-1: "unread", 0: "closed", 1: "open", 2: "half-open"}
	return &scenario{
		name: "d6", title: "upstream protection (GW-13, GW-14)",
		ids: []checkDef{
			{"d6.1", "before the fault: every orders answer 2xx, circuits closed"},
			{"d6.2", "the breaker opens on every replica"},
			{"d6.3", "retries within budget (ADR-0101 §13)"},
			{"d6.4", "recovery: circuits close, orders answers 2xx"},
		},
		length: fixed(70*time.Second, 75*time.Second),
		streams: func(bool) []streamSpec {
			return []streamSpec{{consumer: c, route: "orders", rate: 60}}
		},
		tick: func(r *runner, _ int) {
			t := r.live(c, "orders")
			sc := scraperOf()
			if sc == nil {
				return
			}
			var circ []string
			var sent, exh float64
			for i, g := range r.e.gws {
				s := sc.series(i)
				if len(s) == 0 {
					continue
				}
				last := s[len(s)-1]
				circ = append(circ, g.name+" "+circuitName[last.circuit()])
				if last.ok && s[0].ok {
					sent += retries(last) - retries(s[0])
					exh += exhausted(last) - exhausted(s[0])
				}
			}
			state := ""
			if a, b := times(); a > 0 && b == 0 {
				state = "[upstream slow] "
			}
			r.say("%scircuit: %s | retries sent %.0f, budget_exhausted %.0f | %s orders: 2xx %d 503 %d other %d no answer %d", state, strings.Join(circ, ", "), sent, exh, c, t.ok, t.unavailable, t.other+t.limited+t.forbidden, t.failed)
		},
		run: func(r *runner) error {
			ctx := r.ctx
			sc := startScraper(r)
			mu.Lock()
			scr = sc
			mu.Unlock()
			defer sc.halt()
			if !r.e.sleepUntil(ctx, 10*time.Second) {
				return ctx.Err()
			}
			mu.Lock()
			tsSent = r.e.since()
			mu.Unlock()
			if err := r.setFault(ctx, "upstream-slow", true); err != nil {
				return err
			}
			mu.Lock()
			ts = r.e.since()
			mu.Unlock()
			r.say("upstream-slow: on: every orders endpoint answers 4 s late (per-try timeout 2.5 s)")
			if !r.e.sleepUntil(ctx, ts+30*time.Second) {
				return ctx.Err()
			}
			if err := r.setFault(ctx, "upstream-slow", false); err != nil {
				return err
			}
			mu.Lock()
			te = r.e.since()
			mu.Unlock()
			r.say("upstream-slow: off")
			r.e.sleepUntil(ctx, min(r.e.end, te+30*time.Second))
			return nil
		},
		end: func(r *runner) {
			a, b := times()
			sc := scraperOf()
			if a == 0 || b == 0 || sc == nil {
				return
			}
			for i, g := range r.e.gws {
				s := sc.series(i)
				opened, closed := firstState(s, r.e.t0.Add(a), 1), firstState(s, r.e.t0.Add(b), 0)
				br := budgetRule(s)
				r.say("D6 %s: circuit opened %s after the fault, closed %s after it; retries sent %.0f, budget_exhausted %.0f; tightest budget window at %.0f%%",
					g.name, durOrNever(opened, r.e.t0.Add(a)), durOrNever(closed, r.e.t0.Add(b)), retries(last(s))-retries(s[0]), exhausted(last(s))-exhausted(s[0]), 100*br.tightest)
			}
			r.say("D6 %s orders in the fault: %s", c, statusMix(within(r.answersOf(c, "orders"), a, b)))
		},
		checks: func(r *runner) {
			a, b := times()
			sc := scraperOf()
			if a == 0 || b == 0 || sc == nil {
				for _, d := range []checkDef{{"d6.1", ""}, {"d6.2", ""}, {"d6.3", ""}, {"d6.4", ""}} {
					r.judge(d.id, "the fault", false, "a fault", "setup: the fault was not switched")
				}
				return
			}
			as := allOrders(r)
			mu.Lock()
			sent := tsSent
			mu.Unlock()
			pre, inFlight := beforeFault(as, sent, a)
			okClosed := true
			for i := range r.e.gws {
				for _, m := range sc.series(i) {
					if m.sent.Before(r.e.t0.Add(a)) && m.circuit() != 0 {
						okClosed = false
					}
				}
			}
			r.judge("d6.1", "before the fault: every orders answer 2xx, circuits closed", len(pre) > 0 && count(pre, is2xx) == len(pre) && okClosed, "only 2xx; gw_circuit_state 0 on every replica",
				"%s; circuits closed: %v; %d left out, not completed before the switch-on was sent (%s)", statusMix(pre), okClosed, len(inFlight), statusMix(inFlight))
			okOpen := true
			var open []string
			for i, g := range r.e.gws {
				s := sc.series(i)
				at := firstState(s, r.e.t0.Add(a), 1)
				on, off := scrapeBefore(s, r.e.t0.Add(a)), scrapeBefore(s, r.e.t0.Add(b))
				grew := off.sum("gw_requests_total", "route", "orders", "reason", "circuit_open") - on.sum("gw_requests_total", "route", "orders", "reason", "circuit_open")
				okOpen = okOpen && !at.IsZero() && at.Sub(r.e.t0.Add(a)) <= 15*time.Second && on.ok && off.ok && grew > 0
				open = append(open, fmt.Sprintf("%s opened %s, circuit_open +%.0f", g.name, durOrNever(at, r.e.t0.Add(a)), grew))
			}
			r.judge("d6.2", "the breaker opens on every replica", okOpen, "gw_circuit_state 1 within 15 s on every replica, circuit_open answers grow", "%s", strings.Join(open, "; "))
			okBud := true
			var bud []string
			for i, g := range r.e.gws {
				s := sc.series(i)
				br := budgetRule(s)
				okBud = okBud && br.over == 0 && br.windows >= 30 && br.gaps == 0 && br.failed == 0
				bud = append(bud, fmt.Sprintf("%s %d windows, %d pairs over, tightest %.0f%%, %d gaps, %d unanswered; sent %.0f, budget_exhausted %.0f",
					g.name, br.windows, br.over, 100*br.tightest, br.gaps, br.failed, retries(last(s))-retries(s[0]), exhausted(last(s))-exhausted(s[0])))
			}
			r.judge("d6.3", "retries within budget (ADR-0101 §13)", okBud, "no pair over 0.2 × ΔQ + 30, ≥ 30 windows, every scrape answered, ≤ 2 s apart", "%s", strings.Join(bud, "; "))
			okRec := true
			var rec []string
			for i, g := range r.e.gws {
				at := firstState(sc.series(i), r.e.t0.Add(b), 0)
				okRec = okRec && !at.IsZero() && at.Sub(r.e.t0.Add(b)) <= 30*time.Second
				rec = append(rec, fmt.Sprintf("%s closed %s", g.name, durOrNever(at, r.e.t0.Add(b))))
			}
			late := within(as, b+20*time.Second, b+30*time.Second)
			r.judge("d6.4", "recovery: circuits close, orders answers 2xx", okRec && len(late) > 0 && share(late, is2xx) >= 0.99, "every circuit 0 within 30 s; ≥ 99% 2xx in [Te + 20 s, Te + 30 s)",
				"%s; %s of %d answers 2xx", strings.Join(rec, ", "), pct(share(late, is2xx)), len(late))
		},
	}
}

// beforeFault splits the answers due before ts (when the upstream-slow
// switch-on returned) for d6.1 (ADR-0102, amending ADR-0098): judged are
// those completed before sent (when the switch-on request was sent);
// leftOut are the rest, in flight at sent or due after it.
func beforeFault(as []answer, sent, ts time.Duration) (judged, leftOut []answer) {
	for _, a := range within(as, 0, ts) {
		if a.done < sent {
			judged = append(judged, a)
		} else {
			leftOut = append(leftOut, a)
		}
	}
	return judged, leftOut
}

// firstState is the receive time of the first answered scrape sent at or
// after from whose circuit is state; zero if none.
func firstState(ms []scrape, from time.Time, state float64) time.Time {
	for _, m := range ms {
		if m.ok && !m.sent.Before(from) && m.circuit() == state {
			return m.recv
		}
	}
	return time.Time{}
}

// scrapeBefore is the last answered scrape sent before t.
func scrapeBefore(ms []scrape, t time.Time) scrape {
	var out scrape
	for _, m := range ms {
		if m.ok && m.sent.Before(t) {
			out = m
		}
	}
	return out
}

func last(ms []scrape) scrape {
	if len(ms) == 0 {
		return scrape{}
	}
	return ms[len(ms)-1]
}

func durOrNever(at, from time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return secs(at.Sub(from))
}
