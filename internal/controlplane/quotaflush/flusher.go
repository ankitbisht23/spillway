// Package quotaflush is the control plane's quota flusher (RL-10, D5,
// ADR-0070): on the active replica, every 10 s and one at a time under a
// Postgres advisory lock, it copies the gateways' quota counters from Redis
// into quota_usage with absolute writes, and it re-seeds a tenant's counters
// from Postgres when the tenant's epoch shows that Redis lost them.
package quotaflush

import (
	"context"
	_ "embed"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/quota"
	rlredis "github.com/ankitbisht23/spillway/internal/ratelimit/redis"
	"github.com/ankitbisht23/spillway/internal/secret"
)

// seedScript sets each of a tenant's counters to the larger of its value and
// Postgres's, then the epoch (ADR-0070).
//
//go:embed quota_seed_v1.lua
var seedScript string

var seed = goredis.NewScript(seedScript)

const (
	// FlushEvery is the flush interval: the most a Redis data loss can
	// under-count (RL-10).
	FlushEvery = 10 * time.Second
	// checkEvery is how often the epochs are checked between flushes.
	checkEvery = time.Second
	// epochTTL outlives the longest month plus a day; every flush of the
	// tenant refreshes it.
	epochTTL = 32 * 24 * time.Hour
	// commandTimeout bounds each Redis command or pipeline (ADR-0070).
	commandTimeout = 2 * time.Second
	// keep is how long daily rows stay in quota_usage; monthly rows stay.
	keep       = 90 * 24 * time.Hour
	pruneEvery = time.Hour
	// errorLogEvery is how often a tenant's Redis errors are logged.
	errorLogEvery = time.Minute
	// mgetChunk and seedChunk bound the keys of one MGET or seed call.
	mgetChunk = 1000
	seedChunk = 500
)

// Redis is the flusher's Redis Cluster settings (SEC-09).
type Redis struct {
	Addrs    []string
	AddrMap  map[string]string // dev and tests only (ADR-0004)
	Username string
	Password secret.Value
}

// NewClient returns the flusher's own cluster client: 2 s per command and
// no retries (ADR-0070), never the gateway's limiter client.
func NewClient(r Redis) *goredis.ClusterClient {
	opt := &goredis.ClusterOptions{
		Addrs: r.Addrs, Username: r.Username, Password: r.Password.Reveal(), MaxRetries: -1,
		DialTimeout: commandTimeout, ReadTimeout: commandTimeout, WriteTimeout: commandTimeout, ContextTimeoutEnabled: true,
	}
	if r.AddrMap != nil {
		opt.Dialer = rlredis.Dialer(r.AddrMap)
	}
	return goredis.NewClusterClient(opt)
}

// Flusher flushes and re-seeds quota counters.
type Flusher struct {
	st   *store.Store
	rdb  *goredis.ClusterClient
	name string // this replica's, in epoch values
	log  *slog.Logger
	// Now is the flusher's clock, Redis TIME by default; tests replace it.
	Now func(ctx context.Context) (time.Time, error)

	flushes       *prometheus.CounterVec
	duration      prometheus.Histogram
	reseeds, errs prometheus.Counter
	last          prometheus.Gauge
	mu            sync.Mutex
	tenants       []tenant            // the quota tenants of the last flush
	versions      [2]int64            // the config and identity versions the last flush saw
	lastPrune     time.Time           // on Now's clock; under mu, as Flush may run concurrently
	loggedAt      map[int64]time.Time // tenant -> when its last Redis error was logged
}

// tenant is one tenant's quota consumers.
type tenant struct {
	id        int64
	consumers []int64
}

// New returns a flusher that reads Postgres through st and Redis through
// rdb, names epochs it sets after name, and registers its metrics with reg.
func New(st *store.Store, rdb *goredis.ClusterClient, name string, reg prometheus.Registerer, log *slog.Logger) *Flusher {
	f := &Flusher{
		st: st, rdb: rdb, name: name, log: log, loggedAt: map[int64]time.Time{},
		flushes: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cp_quota_flushes_total", Help: "Quota flushes by result: ok, error, or locked (another flusher held the lock)."}, []string{"result"}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "cp_quota_flush_duration_seconds", Help: "Duration of quota flushes that held the lock.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}}),
		reseeds: prometheus.NewCounter(prometheus.CounterOpts{Name: "cp_quota_reseeds_total", Help: "Tenants whose quota counters were re-seeded from Postgres (a missing epoch)."}),
		errs:    prometheus.NewCounter(prometheus.CounterOpts{Name: "cp_quota_flush_errors_total", Help: "Tenants skipped in a flush or epoch check for a Redis error."}),
		last:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "cp_quota_last_flush_timestamp_seconds", Help: "When the last successful quota flush finished, Unix seconds."}),
	}
	f.Now = f.redisTime
	for _, r := range []string{"ok", "error", "locked"} {
		f.flushes.WithLabelValues(r)
	}
	reg.MustRegister(f.flushes, f.duration, f.reseeds, f.errs, f.last)
	return f
}

func (f *Flusher) redisTime(ctx context.Context) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return f.rdb.Time(ctx).Result()
}

// Run flushes at once and then every FlushEvery, checking the epochs every
// second in between, until ctx ends.
func (f *Flusher) Run(ctx context.Context) {
	f.log.Info("quota flusher started", "every", FlushEvery)
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	f.flush(ctx)
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(last) >= FlushEvery-checkEvery/2 {
			f.flush(ctx)
			last = time.Now()
			continue
		}
		if err := f.CheckEpochs(ctx); err != nil && ctx.Err() == nil {
			f.log.Warn("quota epoch check failed", "err", err)
		}
	}
}

func (f *Flusher) flush(ctx context.Context) {
	if _, err := f.Flush(ctx); err != nil && ctx.Err() == nil {
		f.log.Warn("quota flush failed; trying again next time", "err", err)
	}
}

// Flush makes one flush (ADR-0070) and reports whether it held the lock. In
// one transaction under the flusher's advisory lock, for every tenant with
// a quota consumer, it reads the epoch and the counters of the periods
// quota.Flushed gives for Redis's time, and writes them with
// used = GREATEST(used, count) and refreshes the epoch; a tenant whose epoch
// is missing is re-seeded instead. A Redis error skips its tenant; a
// Postgres error rolls the flush back.
func (f *Flusher) Flush(ctx context.Context) (bool, error) {
	start := time.Now()
	v, err := f.st.LatestVersions(ctx)
	if err != nil {
		f.flushes.WithLabelValues("error").Inc()
		return false, err
	}
	_, plans, err := f.st.PlanQuotas(ctx)
	if err != nil {
		f.flushes.WithLabelValues("error").Inc()
		return false, err
	}
	now, err := f.Now(ctx)
	if err != nil {
		f.flushes.WithLabelValues("error").Inc()
		return false, err
	}
	periods := quota.Flushed(now)
	ran, err := f.st.QuotaFlush(ctx, func(tx *store.QuotaTx) error {
		rows, err := tx.Consumers(ctx, slices.Sorted(maps.Keys(plans)))
		if err != nil {
			return err
		}
		tenants := byTenant(rows)
		f.mu.Lock()
		f.tenants, f.versions = tenants, [2]int64{v.Config, v.Identity}
		f.mu.Unlock()
		var counts []store.QuotaCount
		var seeded []int64
		for _, t := range tenants {
			present, got, err := f.read(ctx, t, periods)
			if err != nil {
				f.redisError(t.id, now, err)
				continue
			}
			if !present {
				if err := f.reseed(ctx, tx, t, now); err != nil {
					return err
				}
				continue
			}
			counts = append(counts, got...)
			seeded = append(seeded, t.id)
		}
		if err := tx.Write(ctx, counts, now); err != nil {
			return err
		}
		f.refresh(ctx, seeded, now)
		return f.prune(ctx, tx, now)
	})
	switch {
	case err != nil:
		f.flushes.WithLabelValues("error").Inc()
		return ran, err
	case !ran:
		f.flushes.WithLabelValues("locked").Inc()
		f.log.Debug("quota flush skipped: another flusher holds the lock")
		return false, nil
	}
	f.flushes.WithLabelValues("ok").Inc()
	f.duration.Observe(time.Since(start).Seconds())
	f.last.SetToCurrentTime()
	return true, nil
}

// CheckEpochs re-seeds at once every tenant of the last flush whose epoch
// is missing, so its quotas count again within about a second of a Redis
// data loss (ADR-0070). When the config or identity version has changed
// since the last flush, it flushes instead, which seeds a tenant that has
// just got its first quota as soon. It does nothing while another flusher
// holds the lock.
func (f *Flusher) CheckEpochs(ctx context.Context) error {
	v, err := f.st.LatestVersions(ctx)
	if err != nil {
		return err
	}
	f.mu.Lock()
	tenants, changed := f.tenants, f.versions != [2]int64{v.Config, v.Identity}
	f.mu.Unlock()
	if changed {
		_, err := f.Flush(ctx)
		return err
	}
	if len(tenants) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	pipe := f.rdb.Pipeline()
	exists := make([]*goredis.IntCmd, len(tenants))
	for i, t := range tenants {
		exists[i] = pipe.Exists(cctx, rlredis.QuotaEpochKey(strconv.FormatInt(t.id, 10)))
	}
	_, _ = pipe.Exec(cctx) // each command carries its own error
	var missing []tenant
	for i, t := range tenants {
		n, err := exists[i].Result()
		if err != nil {
			f.redisError(t.id, time.Now(), err)
			continue
		}
		if n == 0 {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	now, err := f.Now(ctx)
	if err != nil {
		return err
	}
	_, err = f.st.QuotaFlush(ctx, func(tx *store.QuotaTx) error {
		for _, t := range missing {
			if err := f.reseed(ctx, tx, t, now); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// read returns whether t's epoch exists and, if it does, t's counters of
// periods that exist, in one pipeline to the tenant's slot.
func (f *Flusher) read(ctx context.Context, t tenant, periods []quota.Period) (bool, []store.QuotaCount, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	tid := strconv.FormatInt(t.id, 10)
	var keys []string
	var owners []store.QuotaCount // consumer and period of each key
	for _, c := range t.consumers {
		cid := strconv.FormatInt(c, 10)
		for _, p := range periods {
			keys = append(keys, rlredis.QuotaKey(tid, cid, p.ID))
			owners = append(owners, store.QuotaCount{ConsumerID: c, Period: p.ID})
		}
	}
	pipe := f.rdb.Pipeline()
	epoch := pipe.Exists(ctx, rlredis.QuotaEpochKey(tid))
	var gets []*goredis.SliceCmd
	for chunk := range slices.Chunk(keys, mgetChunk) {
		gets = append(gets, pipe.MGet(ctx, chunk...))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return false, nil, err
	}
	if epoch.Val() == 0 {
		return false, nil, nil
	}
	var out []store.QuotaCount
	i := 0
	for _, g := range gets {
		for _, v := range g.Val() {
			if s, ok := v.(string); ok {
				if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 0 {
					c := owners[i]
					c.Used = n
					out = append(out, c)
				}
			}
			i++
		}
	}
	return true, out, nil
}

// reseed re-seeds t's counters of the periods quota.Flushed gives for now
// from Postgres, keeping any larger count Redis still has, then sets the
// epoch, after which the gateways count again (ADR-0070). A tenant seen
// for the first time is seeded the same way.
func (f *Flusher) reseed(ctx context.Context, tx *store.QuotaTx, t tenant, now time.Time) error {
	periods := quota.Flushed(now)
	ids := make([]string, len(periods))
	for i, p := range periods {
		ids[i] = p.ID
	}
	rows, err := tx.Usage(ctx, t.consumers, ids)
	if err != nil {
		return err
	}
	used := map[[2]string]int64{}
	for _, r := range rows {
		used[[2]string{strconv.FormatInt(r.ConsumerID, 10), r.Period}] = r.Used
	}
	tid := strconv.FormatInt(t.id, 10)
	type entry struct {
		key       string
		used, ttl int64
	}
	var entries []entry
	for _, c := range t.consumers {
		cid := strconv.FormatInt(c, 10)
		for _, p := range periods {
			entries = append(entries, entry{rlredis.QuotaKey(tid, cid, p.ID), used[[2]string{cid, p.ID}], p.TTL(now).Milliseconds()})
		}
	}
	value := strconv.FormatInt(now.UnixMilli(), 10) + "-" + f.name
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	chunks := slices.Collect(slices.Chunk(entries, seedChunk))
	if len(chunks) == 0 {
		chunks = [][]entry{nil}
	}
	for i, chunk := range chunks {
		keys := []string{rlredis.QuotaEpochKey(tid)}
		args := []any{"", epochTTL.Milliseconds()}
		if i == len(chunks)-1 {
			args[0] = value // the epoch last, once every counter is seeded
		}
		for _, e := range chunk {
			keys = append(keys, e.key)
			args = append(args, e.used, e.ttl)
		}
		if err := seed.Run(cctx, f.rdb, keys, args...).Err(); err != nil {
			f.redisError(t.id, now, err)
			return nil // the next check tries again
		}
	}
	f.reseeds.Inc()
	f.log.Info("quota counters seeded from Postgres", "tenant_id", t.id, "consumers", len(t.consumers))
	return nil
}

// refresh sets the epochs of tenants back to epochTTL after their flush.
func (f *Flusher) refresh(ctx context.Context, tenants []int64, now time.Time) {
	if len(tenants) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	pipe := f.rdb.Pipeline()
	cmds := make([]*goredis.BoolCmd, len(tenants))
	for i, t := range tenants {
		cmds[i] = pipe.PExpire(ctx, rlredis.QuotaEpochKey(strconv.FormatInt(t, 10)), epochTTL)
	}
	_, _ = pipe.Exec(ctx)
	for i, c := range cmds {
		if err := c.Err(); err != nil {
			f.redisError(tenants[i], now, err)
		}
	}
}

// prune deletes daily rows older than keep, once every pruneEvery.
// lastPrune is under f.mu: the advisory lock orders concurrent Flush calls
// in Postgres but not in Go's memory model.
func (f *Flusher) prune(ctx context.Context, tx *store.QuotaTx, now time.Time) error {
	f.mu.Lock()
	last := f.lastPrune
	f.mu.Unlock()
	if !last.IsZero() && now.Sub(last) < pruneEvery {
		return nil
	}
	n, err := tx.Prune(ctx, quota.Day(now.Add(-keep)).ID)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.lastPrune = now
	f.mu.Unlock()
	if n > 0 {
		f.log.Info("pruned daily quota usage", "rows", n, "older_than", keep)
	}
	return nil
}

// redisError counts a tenant skipped for a Redis error and logs it at most
// once a minute per tenant.
func (f *Flusher) redisError(t int64, now time.Time, err error) {
	f.errs.Inc()
	f.mu.Lock()
	defer f.mu.Unlock()
	if at, ok := f.loggedAt[t]; ok && now.Sub(at) < errorLogEvery {
		return
	}
	f.loggedAt[t] = now
	f.log.Warn("quota flusher: Redis error; the tenant is skipped until the next round", "tenant_id", t, "err", err)
}

// byTenant groups consumer rows, sorted by tenant, into tenants.
func byTenant(rows []store.QuotaConsumer) []tenant {
	var out []tenant
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].id != r.TenantID {
			out = append(out, tenant{id: r.TenantID})
		}
		out[len(out)-1].consumers = append(out[len(out)-1].consumers, r.ID)
	}
	return out
}
