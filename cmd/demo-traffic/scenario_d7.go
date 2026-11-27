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

// D7, the leaked key (RL-21, RL-09, CP-20): a fresh free-co-partner key
// floods orders at 20× its limit, heavy hitters surface it, and a block
// (createAccessList, deny on the key: the Throttling screen's one click)
// reaches every replica within 5 s.

const partner = "free-co-partner"

// heavyHitter is the part of listHeavyHitters' items D7 reads.
type heavyHitter struct {
	ConsumerID *int64 `json:"consumer_id"`
	APIKeyID   *int64 `json:"api_key_id"`
	Denied     int64  `json:"denied"`
}

// heavyHitters reads the top 10 throttled keys of the windows that start
// at or after since (whole minutes, ADR-0041).
func heavyHitters(ctx context.Context, r *runner, since time.Time) ([]heavyHitter, error) {
	var list struct {
		Items []heavyHitter `json:"items"`
	}
	q := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "limit": {"10"}}
	_, err := r.cp.Must(ctx, http.StatusOK, http.MethodGet, "/heavy-hitters?"+q.Encode(), nil, &list)
	return list.Items, err
}

// probe is one block probe: when it was sent and answered (from time 0)
// and its status (0: no answer).
type probe struct {
	sent, recv time.Duration
	status     int
}

// blockResult is one block: the access-list entry, when it was sent, and
// each replica's probes.
type blockResult struct {
	key     *apiKey
	entryID int64
	sent    time.Duration
	probes  [][]probe // per replica, by send time
}

// first403 is replica i's first 403's answer time, false if none.
func (b blockResult) first403(i int) (time.Duration, bool) {
	at, found := time.Duration(0), false
	for _, p := range b.probes[i] {
		if p.status == http.StatusForbidden && (!found || p.recv < at) {
			at, found = p.recv, true
		}
	}
	return at, found
}

// sample is the block's time on its slowest replica, false if a replica
// never answered 403.
func (b blockResult) sample() (time.Duration, bool) {
	worst := time.Duration(0)
	for i := range b.probes {
		at, ok := b.first403(i)
		if !ok {
			return 0, false
		}
		worst = max(worst, at-b.sent)
	}
	return worst, true
}

// problems lists what breaks ADR-0098 d7.2's no-flip rule (ten probes
// sent after the first 403, all 403) and d7.3 (probes answered before the
// block 2xx or 429; every probe answered, 2xx, 429 or 403; after the
// first 403 only 403).
func (b blockResult) problems(gws []gateway) (noFlip, answers []string) {
	for i, ps := range b.probes {
		f, ok := b.first403(i)
		after := 0
		for _, p := range ps {
			switch {
			case p.status == 0:
				answers = append(answers, fmt.Sprintf("%s: a probe sent at %s got no answer", gws[i].name, secs(p.sent)))
			case p.recv < b.sent && p.status != 200 && p.status != 429:
				answers = append(answers, fmt.Sprintf("%s: %d before the block", gws[i].name, p.status))
			case p.status != 200 && p.status != 429 && p.status != http.StatusForbidden:
				answers = append(answers, fmt.Sprintf("%s: %d", gws[i].name, p.status))
			}
			if ok && p.sent >= f {
				after++
				if p.status != http.StatusForbidden {
					noFlip = append(noFlip, fmt.Sprintf("%s: %d after its first 403", gws[i].name, p.status))
				}
			}
		}
		if ok && after < 10 {
			noFlip = append(noFlip, fmt.Sprintf("%s: %d probes after its first 403, want 10", gws[i].name, after))
		}
	}
	return noFlip, answers
}

// block blocks key with an access-list deny on it (ADR-0062), timing it per
// replica: from just before the request, one probe loop per replica sends
// the key every 50 ms (4 s timeout each) until ten probes after its first
// 403, at most 10 s after the block.
func block(r *runner, key *apiKey, note string) (blockResult, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 20*time.Second)
	defer cancel()
	res := blockResult{key: key, probes: make([][]probe, len(r.e.gws))}
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	defer client.CloseIdleConnections()
	var mu sync.Mutex
	sentAt := time.Duration(-1)
	blockSent := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return sentAt
	}
	var loops sync.WaitGroup
	for i, g := range r.e.gws {
		loops.Go(func() {
			var inflight sync.WaitGroup
			defer inflight.Wait()
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			after := -1 // probes sent since this replica's first 403; -1 before it
			for {
				mu.Lock()
				seen := slices.ContainsFunc(res.probes[i], func(p probe) bool { return p.status == http.StatusForbidden })
				mu.Unlock()
				if seen && after < 0 {
					after = 0
				}
				if after >= 10 {
					return
				}
				if s := blockSent(); s >= 0 && r.e.since() > s+10*time.Second {
					return
				}
				if after >= 0 {
					after++
				}
				inflight.Go(func() {
					p := probe{sent: r.e.since()}
					status, _, err := probeAnswer(ctx, client, g, key, "orders")
					p.recv = r.e.since()
					if err == nil {
						p.status = status
					}
					mu.Lock()
					res.probes[i] = append(res.probes[i], p)
					mu.Unlock()
				})
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	sentAt = r.e.since()
	res.sent = sentAt
	mu.Unlock()
	var entry struct {
		ID int64 `json:"id"`
	}
	_, err := r.cp.Must(ctx, http.StatusCreated, http.MethodPost, "/access-lists", map[string]any{"action": "deny", "subject_type": "key", "api_key_id": key.id, "note": note}, &entry)
	if err != nil {
		cancel()
	}
	loops.Wait()
	res.entryID = entry.ID
	for i := range res.probes {
		slices.SortFunc(res.probes[i], func(a, b probe) int { return int(a.sent - b.sent) })
	}
	return res, err
}

// scenarioD7 is D7.
func scenarioD7() *scenario {
	var mu sync.Mutex
	var seenAt, topAt time.Duration // when K1 first appeared, and first was the top entry by denied
	var rank int
	var denied int64
	var blocks []blockResult
	state := func() (time.Duration, int, int64) {
		mu.Lock()
		defer mu.Unlock()
		return seenAt, rank, denied
	}
	return &scenario{
		name: "d7", title: "leaked key (RL-21, RL-09, CP-20)",
		ids: []checkDef{
			{"d7.1", "heavy hitters surface the leaked key within 20 s"},
			{"d7.2", "a block reaches every replica within 5 s, ten rounds"},
			{"d7.3", "probes 2xx or 429 before each block, 403 after"},
			{"d7.4", "the audit log has the ten access-list records"},
		},
		length: fixed(60*time.Second, 120*time.Second),
		streams: func(bool) []streamSpec {
			return []streamSpec{{consumer: partner, route: "orders", rate: 200}}
		},
		tick: func(r *runner, _ int) {
			t := r.live(partner, "orders")
			hh := "not in heavy hitters yet"
			if at, rk, d := state(); at > 0 {
				hh = fmt.Sprintf("heavy hitters #%d with %d denied", rk, d)
			}
			r.say("%s orders: 2xx %3d  429 %3d  403 %3d | %s", partner, t.ok, t.limited, t.forbidden, hh)
		},
		run: func(r *runner) error {
			ctx := r.ctx
			k1 := r.e.keys.current(partner)
			if k1 == nil {
				return fmt.Errorf("no key for %s", partner)
			}
			since := r.e.t0.Truncate(time.Minute) // windows are whole minutes (ADR-0041)
			found := false
			limit := r.e.end
			if r.check {
				limit = 20 * time.Second
			}
			for n := 1; r.e.since() < limit && !found; n++ {
				if !r.e.sleepUntil(ctx, time.Duration(n)*time.Second) {
					return ctx.Err()
				}
				items, err := heavyHitters(ctx, r, since)
				if err != nil {
					r.say("heavy hitters: %v", err)
					continue
				}
				top := -1
				for i, it := range items {
					if top < 0 || it.Denied > items[top].Denied {
						top = i
					}
				}
				for i, it := range items {
					if it.APIKeyID == nil || *it.APIKeyID != k1.id {
						continue
					}
					mu.Lock()
					if seenAt == 0 {
						seenAt, rank = r.e.since(), i+1
						r.say("heavy hitters: %s of %s is #%d with %d denied, %s after the flood started", k1, partner, i+1, it.Denied, secs(seenAt))
					}
					rank, denied = i+1, it.Denied
					if topAt == 0 && i == top && it.ConsumerID != nil && *it.ConsumerID == k1.consumerID {
						topAt = r.e.since()
					}
					mu.Unlock()
				}
				mu.Lock()
				found = topAt > 0 && (r.check || r.block)
				mu.Unlock()
			}
			if r.check {
				for _, s := range r.streamsOf(partner, "orders") {
					s.stop()
				}
				r.say("the flood stops")
			}
			if !r.check && !r.block {
				r.e.sleepUntil(ctx, r.e.end)
				return nil
			}
			if !found && !r.check {
				r.say("D7: the key did not reach the top of heavy hitters within %.0f s", limit.Seconds())
				return nil
			}
			rounds := 1
			if r.check {
				rounds = 10
			}
			for i := 1; i <= rounds; i++ {
				key := k1
				if i > 1 {
					var err error
					if key, err = r.e.keys.newKey(ctx, partner); err != nil {
						return err
					}
					if err := waitApplied(ctx, r.e.gws, key); err != nil {
						return err
					}
				}
				note := "demo-traffic d7: block the leaked key"
				if r.check {
					note = fmt.Sprintf("verify-m8 D7 %s round %d", r.runID, i)
				}
				b, err := block(r, key, note)
				if err != nil {
					return err
				}
				mu.Lock()
				blocks = append(blocks, b)
				mu.Unlock()
				var per []string
				for j, g := range r.e.gws {
					if at, ok := b.first403(j); ok {
						per = append(per, fmt.Sprintf("%s %s", g.name, secs(at-b.sent)))
					} else {
						per = append(per, g.name+" never")
					}
				}
				r.say("block %d: %s denied (access-list entry #%d); first 403 after %s", i, key, b.entryID, strings.Join(per, ", "))
			}
			if !r.check {
				r.e.sleepUntil(ctx, r.e.since()+3*time.Second)
			}
			return nil
		},
		end: func(r *runner) {
			as := r.answersOf(partner, "orders")
			r.say("D7: %s sent %d requests: %s", partner, len(as), statusMix(as))
		},
		checks: func(r *runner) {
			at, rk, d := state()
			mu.Lock()
			top := topAt
			mu.Unlock()
			k1 := r.e.keys.current(partner)
			r.judge("d7.1", "heavy hitters surface the leaked key within 20 s", top > 0 && top <= 20*time.Second, "the top entry by denied is K1 of free-co-partner within 20 s",
				"K1 (%s) seen %s, top %s; rank %d, %d denied", k1, durOr(at), durOr(top), rk, d)
			var samples []float64
			var noFlip, answers []string
			ok := len(blocks) == 10
			for _, b := range blocks {
				s, done := b.sample()
				ok = ok && done && s <= 5*time.Second
				samples = append(samples, s.Seconds())
				nf, an := b.problems(r.e.gws)
				noFlip, answers = append(noFlip, nf...), append(answers, an...)
			}
			mx := 0.0
			if len(samples) > 0 {
				mx = slices.Max(samples)
			}
			r.judge("d7.2", "a block reaches every replica within 5 s, ten rounds", ok && len(noFlip) == 0, "10 samples, each ≤ 5 s, ten 403s after each first 403",
				"%d rounds: max %.2f s, median %.2f s; %s", len(blocks), mx, median(samples), firstOr(noFlip, "no flip after a 403"))
			r.judge("d7.3", "probes 2xx or 429 before each block, 403 after", len(blocks) == 10 && len(answers) == 0, "only 2xx or 429 before, 403 after, every probe answered",
				"%s", firstOr(answers, "as wanted"))
			ids := map[string]bool{}
			for _, b := range blocks {
				ids[strconv.FormatInt(b.entryID, 10)] = true
			}
			items, err := auditSince(r.ctx, r, "access_list")
			n := 0
			for _, it := range items {
				if it.EntityID != nil && ids[*it.EntityID] && it.Actor.Name == "service-account-spillway-ci" {
					n++
				}
			}
			r.judge("d7.4", "the audit log has the ten access-list records", n == 10 && len(ids) == 10 && err == nil, "10 records by service-account-spillway-ci", "%d of %d found%s", n, len(ids), errSuffix(err))
		},
	}
}

func durOr(d time.Duration) string {
	if d == 0 {
		return "never"
	}
	return "after " + secs(d)
}

func firstOr(xs []string, none string) string {
	if len(xs) == 0 {
		return none
	}
	if len(xs) == 1 {
		return xs[0]
	}
	return fmt.Sprintf("%s (and %d more)", xs[0], len(xs)-1)
}
