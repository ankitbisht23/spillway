//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	"github.com/ankitbisht23/spillway/internal/controlplane/quotaflush"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/quota"
	"github.com/ankitbisht23/spillway/internal/ratelimit/redis"
)

// Quotas (RL-10, ADR-0070, ADR-0071): plan quotas and quota overrides
// through the API, the flusher on Postgres and the compose Redis Cluster,
// and a managed gateway enforcing a daily quota.

// freshIdentityIDs makes database db hand out tenant and consumer IDs from
// a random start, so quota keys and epochs in the shared Redis Cluster
// (q:v1:{t:<tenant>}:…) never meet another run's.
func freshIdentityIDs(t *testing.T, db string) {
	t.Helper()
	n, err := rand.Int(rand.Reader, big.NewInt(1<<40))
	if err != nil {
		t.Fatal(err)
	}
	base := 1<<40 + n.Int64()
	owner := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	for _, table := range []string{"tenants", "consumers"} {
		if _, err := owner.Exec(t.Context(), fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', 'id'), $1)", table), base); err != nil {
			t.Fatal(err)
		}
	}
}

// quotaRedis returns a cluster client of the flusher's kind for the compose
// cluster, through the address map.
func quotaRedis(t *testing.T) (*goredis.ClusterClient, map[string]string) {
	t.Helper()
	addrMap, err := redis.ParseAddrMap(os.Getenv(redis.AddrMapEnv))
	if err != nil || addrMap == nil {
		t.Fatalf("%s is not set (%v); run make test-integration", redis.AddrMapEnv, err)
	}
	rdb := quotaflush.NewClient(quotaflush.Redis{Addrs: []string{"redis-0:6379"}, AddrMap: addrMap})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, addrMap
}

// quotaWorld is a control plane with plan free (daily 100, monthly 1000)
// published, tenant acme on it and consumers bot and app.
type quotaWorld struct {
	r        roles
	db       string
	st       *store.Store
	tenant   int64
	bot, app int64
	plan     int64
}

func newQuotaWorld(t *testing.T) *quotaWorld {
	t.Helper()
	c, iss, db := startControlPlane(t, io.Discard)
	freshIdentityIDs(t, db)
	w := &quotaWorld{r: newRoles(t, c, iss), db: db, st: openStore(t, db)}
	created(t, w.r.editor, "/upstreams", ordersUpstream)
	created(t, w.r.editor, "/routes", ordersRoute)
	w.plan, _ = created(t, w.r.editor, "/plans", map[string]any{"name": "free", "quotas": map[string]any{"daily": 100, "monthly": 1000}})
	_, tag := validate(t, w.r.viewer)
	w.r.publisher.must(200, "POST", "/config/publish", note("plan free with quotas"), "If-Match", tag)
	w.tenant, _ = created(t, w.r.admin, "/tenants", map[string]any{"name": "acme", "plan_id": w.plan})
	w.bot, _ = created(t, w.r.keyAdmin, "/consumers", map[string]any{"tenant_id": w.tenant, "name": "bot"})
	w.app, _ = created(t, w.r.keyAdmin, "/consumers", map[string]any{"tenant_id": w.tenant, "name": "app"})
	return w
}

func (w *quotaWorld) usage(t *testing.T, ids ...int64) map[int64]apiv1.QuotaUsage {
	t.Helper()
	var q []string
	for _, id := range ids {
		q = append(q, fmt.Sprintf("consumer_id=%d", id))
	}
	var l apiv1.QuotaUsageList
	w.r.viewer.must(200, "GET", "/quota-usage?"+strings.Join(q, "&"), nil).decode(t, &l)
	out := map[int64]apiv1.QuotaUsage{}
	for _, u := range l.Items {
		out[u.ConsumerId] = u
	}
	return out
}

// TestQuotaAPI: plan quotas; quota overrides as their own resource, with
// RBAC, If-Match, audit records and identity versions, kept apart from
// /overrides; and /quota-usage with the effective quotas (RL-10, CP-12 –
// CP-14, CP-20, ADR-0070, ADR-0071).
func TestQuotaAPI(t *testing.T) {
	w := newQuotaWorld(t)
	r := w.r

	// Plans carry quotas; each is 0 to 10^15.
	var p apiv1.Plan
	r.viewer.must(200, "GET", fmt.Sprintf("/plans/%d", w.plan), nil).decode(t, &p)
	if p.Quotas == nil || *p.Quotas.Daily != 100 || *p.Quotas.Monthly != 1000 {
		t.Errorf("plan quotas %+v", p.Quotas)
	}
	if e := r.editor.must(422, "POST", "/plans", map[string]any{"name": "bad", "quotas": map[string]any{"daily": -1}}).apiErr(t); !strings.Contains(e.Error, "quotas.daily: -1") {
		t.Errorf("a negative plan quota: %+v", e)
	}

	// Quota overrides: key-admin and admin write, every role reads.
	body := map[string]any{"consumer_id": w.bot, "quotas": map[string]any{"daily": 7}, "note": "launch"}
	r.viewer.must(403, "POST", "/quota-overrides", body)
	r.editor.must(403, "POST", "/quota-overrides", body)
	r.publisher.must(403, "POST", "/quota-overrides", body)
	standing, tag := created(t, r.keyAdmin, "/quota-overrides", body)
	if e := r.admin.must(409, "POST", "/quota-overrides", body).apiErr(t); !strings.Contains(e.Error, "standing quota override") {
		t.Errorf("a second standing quota override: %+v", e)
	}
	ends := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	grant, grantTag := created(t, r.admin, "/quota-overrides", map[string]any{"consumer_id": w.bot, "quotas": map[string]any{"daily": 50, "monthly": 60}, "ends_at": ends,
		"starts_at": time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)})
	for _, tt := range []struct {
		body map[string]any
		path string
	}{
		{map[string]any{"consumer_id": w.bot, "quotas": map[string]any{"monthly": int64(1e15) + 1}}, "quotas"},
		{map[string]any{"consumer_id": w.bot, "quotas": map[string]any{"daily": 1}, "starts_at": ends, "ends_at": ends}, "ends_at"},
		{map[string]any{"quotas": map[string]any{"daily": 1}}, "consumer_id"},
	} {
		e := r.keyAdmin.must(422, "POST", "/quota-overrides", tt.body).apiErr(t)
		if e.Details == nil || (*e.Details)[0].Path == nil || *(*e.Details)[0].Path != tt.path {
			t.Errorf("%v: %+v, want a validation error on %s", tt.body, e, tt.path)
		}
	}
	if e := r.keyAdmin.must(422, "POST", "/quota-overrides", map[string]any{"consumer_id": 999999999999999, "quotas": map[string]any{"daily": 1}}).apiErr(t); !strings.Contains(e.Error, "no such consumer") {
		t.Errorf("an unknown consumer: %+v", e)
	}
	var page apiv1.QuotaOverrideList
	r.viewer.must(200, "GET", fmt.Sprintf("/quota-overrides?consumer_id=%d", w.bot), nil).decode(t, &page)
	if len(page.Items) != 2 || *page.Items[0].Id != standing || *page.Items[0].Quotas.Daily != 7 || *page.Items[0].Quotas.Monthly != 0 || *page.Items[0].Note != "launch" {
		t.Errorf("quota overrides %+v", page.Items)
	}

	// /overrides is policy overrides only; /quota-overrides quota ones.
	var po apiv1.OverrideList
	r.viewer.must(200, "GET", "/overrides", nil).decode(t, &po)
	if len(po.Items) != 0 {
		t.Errorf("/overrides lists %+v, want no quota override", po.Items)
	}
	r.viewer.must(404, "GET", fmt.Sprintf("/overrides/%d", standing), nil)
	r.keyAdmin.must(404, "DELETE", fmt.Sprintf("/overrides/%d", standing), nil, "If-Match", tag)

	// Update and delete need If-Match; each change is an identity change
	// with an audit record.
	upd := map[string]any{"consumer_id": w.bot, "quotas": map[string]any{"daily": 8}}
	r.keyAdmin.must(428, "PUT", fmt.Sprintf("/quota-overrides/%d", standing), upd)
	r.keyAdmin.must(412, "PUT", fmt.Sprintf("/quota-overrides/%d", standing), upd, "If-Match", `"1"`)
	a := r.keyAdmin.must(200, "PUT", fmt.Sprintf("/quota-overrides/%d", standing), upd, "If-Match", tag)
	if a.etag() == tag {
		t.Errorf("the update kept ETag %s", tag)
	}
	r.keyAdmin.must(204, "DELETE", fmt.Sprintf("/quota-overrides/%d", grant), nil, "If-Match", grantTag)
	r.viewer.must(404, "GET", fmt.Sprintf("/quota-overrides/%d", grant), nil)
	recs := auditPage(t, r.admin, "?entity=quota_override&limit=10")
	var actions []string
	for _, rec := range recs.Items {
		actions = append(actions, rec.Action)
		if rec.IdentityVersion == nil {
			t.Errorf("%s has no identity version", rec.Action)
		}
	}
	if got := strings.Join(actions, " "); got != "quota_override.delete quota_override.update quota_override.create quota_override.create" {
		t.Errorf("audit actions %q", got)
	}

	// The identity snapshot carries the quota override: policy 0, quotas.
	id, err := w.st.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(id.GetOverrides()) != 1 || id.GetOverrides()[0].GetPolicyId() != 0 || id.GetOverrides()[0].GetQuotas().GetDaily() != 8 {
		t.Errorf("identity overrides %v", id.GetOverrides())
	}

	// Usage: the override replaces both of bot's quotas; app has its
	// plan's; nothing flushed yet; unknown IDs are left out.
	u := w.usage(t, w.bot, w.app, 999999999999999)
	day, month := quota.Day(time.Now()).ID, quota.Month(time.Now()).ID
	if len(u) != 2 || u[w.bot].Daily.Quota != 8 || u[w.bot].Monthly.Quota != 0 || u[w.app].Daily.Quota != 100 || u[w.app].Monthly.Quota != 1000 ||
		u[w.app].Daily.Period != day || u[w.app].Monthly.Period != month || u[w.app].Daily.Used != 0 || u[w.app].Daily.FlushedAt != nil {
		t.Errorf("usage %+v", u)
	}
	r.viewer.must(400, "GET", "/quota-usage", nil)
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("consumer_id=%d", i+1)
	}
	r.viewer.must(400, "GET", "/quota-usage?"+strings.Join(many, "&"), nil)
}

// counter sets a quota counter as gateways leave it.
func setCounter(t *testing.T, rdb *goredis.ClusterClient, tenant, consumer int64, period string, n int64) {
	t.Helper()
	if err := rdb.Set(context.Background(), redis.QuotaKey(fmt.Sprint(tenant), fmt.Sprint(consumer), period), n, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
}

func epochExists(t *testing.T, rdb *goredis.ClusterClient, tenant int64) bool {
	t.Helper()
	n, err := rdb.Exists(context.Background(), redis.QuotaEpochKey(fmt.Sprint(tenant))).Result()
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// dropTenant deletes a tenant's quota keys of periods, its epoch first: a
// Redis data loss.
func dropTenant(t *testing.T, rdb *goredis.ClusterClient, tenant int64, consumers []int64, periods ...string) {
	t.Helper()
	keys := []string{redis.QuotaEpochKey(fmt.Sprint(tenant))}
	for _, c := range consumers {
		for _, p := range periods {
			keys = append(keys, redis.QuotaKey(fmt.Sprint(tenant), fmt.Sprint(c), p))
		}
	}
	for _, k := range keys {
		if err := rdb.Del(context.Background(), k).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestQuotaFlusher: the flusher's absolute writes never double-count or
// lower used; a missing epoch is re-seeded from Postgres, keeping any larger
// count Redis has, within a check interval; only the lock holder flushes;
// the previous day and month are flushed for 60 s after they end; daily
// rows are pruned after 90 days; and a Redis data loss loses at most the
// counts since the last flush (RL-10, D5, ADR-0070).
func TestQuotaFlusher(t *testing.T) {
	w := newQuotaWorld(t)
	rdb, _ := quotaRedis(t)
	ctx := t.Context()
	consumers := []int64{w.bot, w.app}
	day, month := quota.Day(time.Now()).ID, quota.Month(time.Now()).ID
	t.Cleanup(func() {
		dropTenant(t, rdb, w.tenant, consumers, day, month, "d20300131", "m203001", "d20300201", "m203002")
	})
	reg := prometheus.NewRegistry()
	f := quotaflush.New(w.st, rdb, "cp-test", reg, slog.New(slog.DiscardHandler))
	flush := func(f *quotaflush.Flusher) {
		t.Helper()
		if ran, err := f.Flush(ctx); err != nil || !ran {
			t.Fatalf("flush: ran %v, %v", ran, err)
		}
	}

	// The first flush seeds the tenant: no counters, an epoch.
	flush(f)
	if !epochExists(t, rdb, w.tenant) {
		t.Fatal("no epoch after the first flush")
	}
	setCounter(t, rdb, w.tenant, w.bot, day, 5)
	setCounter(t, rdb, w.tenant, w.bot, month, 7)
	flush(f)
	used := func() (int64, int64) {
		t.Helper()
		u := w.usage(t, w.bot)[w.bot]
		if u.Daily.FlushedAt == nil {
			t.Fatal("no flushed_at")
		}
		return u.Daily.Used, u.Monthly.Used
	}
	if d, m := used(); d != 5 || m != 7 {
		t.Fatalf("after a flush: daily %d, monthly %d; want 5, 7", d, m)
	}
	// A repeated flush, and a late one with a lower count: used stays.
	flush(f)
	setCounter(t, rdb, w.tenant, w.bot, day, 3)
	flush(f)
	if d, _ := used(); d != 5 {
		t.Errorf("after a repeated and a lower flush: daily %d, want 5", d)
	}
	setCounter(t, rdb, w.tenant, w.bot, day, 9)
	flush(f)

	// Data loss: the 4 counted since the last flush are lost, the rest is
	// re-seeded from Postgres within one check, and counting resumes from
	// there.
	if err := rdb.IncrBy(ctx, redis.QuotaKey(fmt.Sprint(w.tenant), fmt.Sprint(w.bot), day), 4).Err(); err != nil {
		t.Fatal(err)
	}
	dropTenant(t, rdb, w.tenant, consumers, day, month)
	start := time.Now()
	if err := f.CheckEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); !epochExists(t, rdb, w.tenant) || took > 2*time.Second {
		t.Fatalf("re-seed: epoch %v after %v", epochExists(t, rdb, w.tenant), took)
	}
	if d, m := getKey(t, rdb, redis.QuotaKey(fmt.Sprint(w.tenant), fmt.Sprint(w.bot), day)), getKey(t, rdb, redis.QuotaKey(fmt.Sprint(w.tenant), fmt.Sprint(w.bot), month)); d != "9" || m != "7" {
		t.Errorf("re-seeded counters %s, %s; want Postgres's 9, 7: lost at most the counts since the last flush", d, m)
	}
	if got := getKey(t, rdb, redis.QuotaKey(fmt.Sprint(w.tenant), fmt.Sprint(w.app), day)); got != "<missing>" {
		t.Errorf("app's counter %s: a zero is not written", got)
	}
	if got := metricValue(t, reg, "cp_quota_reseeds_total", ""); got != 2 {
		t.Errorf("cp_quota_reseeds_total %v, want 2 (the first seed and the re-seed)", got)
	}
	if got := metricValue(t, reg, "cp_quota_flushes_total", "ok"); got != 5 {
		t.Errorf(`cp_quota_flushes_total{result="ok"} %v, want 5`, got)
	}

	// A tenant that gets its first quota (an identity change) is seeded by
	// the next check, which flushes when a version has changed.
	tenant2, _ := created(t, w.r.admin, "/tenants", map[string]any{"name": "tiny", "plan_id": w.plan})
	t.Cleanup(func() { dropTenant(t, rdb, tenant2, nil) })
	if err := f.CheckEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if epochExists(t, rdb, tenant2) {
		t.Error("a tenant without consumers has an epoch")
	}
	created(t, w.r.keyAdmin, "/consumers", map[string]any{"tenant_id": tenant2, "name": "new"})
	if err := f.CheckEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if !epochExists(t, rdb, tenant2) {
		t.Error("the check after a new quota consumer did not seed its tenant")
	}

	// The running flusher's 1 s check re-seeds by itself.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); f.Run(runCtx) }()
	time.Sleep(200 * time.Millisecond) // past Run's first flush
	dropTenant(t, rdb, w.tenant, consumers, day, month)
	took := eventually(t, "the running flusher re-seeds", func() bool { return epochExists(t, rdb, w.tenant) })
	stop()
	<-done
	t.Logf("re-seeded %v after the data loss", took)
	if took > 2*time.Second {
		t.Errorf("re-seeded %v after the data loss, want within 2 s", took)
	}

	// Two flushers on one database: the lock holder only.
	owner := pgConnect(t, pgURL(t, "spillway", w.db), pgOwnerPassword)
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1397774156, 2)"); err != nil {
		t.Fatal(err)
	}
	if ran, err := f.Flush(ctx); ran || err != nil {
		t.Errorf("flush while another holds the lock: ran %v, %v", ran, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	f2 := quotaflush.New(w.st, rdb, "cp-test-2", prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ran := 0
	for range 10 {
		for _, fl := range []*quotaflush.Flusher{f, f2} {
			wg.Go(func() {
				ok, err := fl.Flush(ctx)
				if err != nil {
					t.Errorf("concurrent flush: %v", err)
				}
				if ok {
					mu.Lock()
					ran++
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()
	locked := metricValue(t, reg, "cp_quota_flushes_total", "locked")
	t.Logf("20 concurrent flushes: %d held the lock; flusher 1 skipped %v", ran, locked)
	if ran < 1 || locked < 1 {
		t.Errorf("ran %d, locked %v: want the lock-holding run and at least the held-lock skip", ran, locked)
	}
	if d, m := used(); d != 9 || m != 7 {
		t.Errorf("after concurrent flushes: %d, %d; want 9, 7", d, m)
	}

	// Rollover on a fake clock: 30 s after a month's end the previous day
	// and month are flushed too; 90 s after, they are not.
	setCounter(t, rdb, w.tenant, w.bot, "d20300131", 11)
	setCounter(t, rdb, w.tenant, w.bot, "m203001", 12)
	setCounter(t, rdb, w.tenant, w.bot, "d20300201", 1)
	setCounter(t, rdb, w.tenant, w.bot, "m203002", 1)
	f.Now = func(context.Context) (time.Time, error) { return time.Date(2030, 2, 1, 0, 0, 30, 0, time.UTC), nil }
	flush(f)
	rows := usageRows(t, w.db, w.bot)
	for p, want := range map[string]int64{"d20300131": 11, "m203001": 12, "d20300201": 1, "m203002": 1} {
		if rows[p] != want {
			t.Errorf("after the rollover flush: %s = %d, want %d (%v)", p, rows[p], want, rows)
		}
	}
	setCounter(t, rdb, w.tenant, w.bot, "d20300131", 20)
	f.Now = func(context.Context) (time.Time, error) { return time.Date(2030, 2, 1, 0, 1, 30, 0, time.UTC), nil }
	flush(f)
	if rows := usageRows(t, w.db, w.bot); rows["d20300131"] != 11 {
		t.Errorf("90 s after the day's end: d20300131 = %d, want 11 (no longer flushed)", rows["d20300131"])
	}

	// Pruning, on a new flusher's first flush: daily rows older than 90
	// days go, monthly rows and newer days stay.
	if _, err := owner.Exec(ctx, `INSERT INTO quota_usage (consumer_id, period, used, flushed_at) VALUES ($1, 'd20291101', 1, now()), ($1, 'm202911', 1, now())`, w.bot); err != nil {
		t.Fatal(err)
	}
	f3 := quotaflush.New(w.st, rdb, "cp-test-3", prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	f3.Now = f.Now
	flush(f3)
	rows = usageRows(t, w.db, w.bot)
	if _, ok := rows["d20291101"]; ok {
		t.Errorf("a 92-day-old daily row survived pruning: %v", rows)
	}
	if rows["m202911"] != 1 || rows["d20300131"] != 11 {
		t.Errorf("pruning removed a monthly or recent row: %v", rows)
	}
}

// usageRows is consumer's quota_usage rows by period.
func usageRows(t *testing.T, db string, consumer int64) map[string]int64 {
	t.Helper()
	conn := pgConnect(t, pgURL(t, "spillway", db), pgOwnerPassword)
	rows, err := conn.Query(t.Context(), "SELECT period, used FROM quota_usage WHERE consumer_id = $1", consumer)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			t.Fatal(err)
		}
		out[p] = n
	}
	return out
}

// metricValue is the value of counter name in reg, the series with
// result="result" if result is set.
func metricValue(t *testing.T, reg *prometheus.Registry, name, result string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if mf.GetName() == name && (result == "" || len(m.GetLabel()) == 1 && m.GetLabel()[0].GetValue() == result) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestManagedQuota: a managed gateway enforces a plan's daily quota of 5 —
// the sixth request is 429 with Retry-After to UTC midnight and the
// quota/daily items — and the control plane's flusher shows 5 used within
// 15 s; a quota override then raises it (RL-10, ADR-0070).
func TestManagedQuota(t *testing.T) {
	iss := newTestIssuer(t)
	db := migratedDatabase(t)
	freshIdentityIDs(t, db)
	rdb, addrMap := quotaRedis(t)
	var mapped []string
	for k, v := range addrMap {
		mapped = append(mapped, k+"="+v)
	}
	addrs := "SPILLWAY_REDIS_ADDR_MAP=" + strings.Join(mapped, ",")
	grpcAddr := freeAddr(t)
	cp := startCP(t, "controlplane", db, grpcAddr, iss, "SPILLWAY_CP_REDIS_ADDRS=redis-0:6379", addrs)
	_, ws := exportYAML(t, cp.rest, "")
	importYAML(t, cp.rest, 200, []byte(fmt.Sprintf("upstreams: [{name: u, endpoints: [{address: '%s'}]}]\nroutes: [{name: r, upstream: u, match: {path_prefix: /r}}]\nplans: [{name: free, quotas: {daily: 5}}]\n",
		upstreamAnswering(t, "ok"))), ws, "")
	_, tag := validate(t, cp.rest)
	var v struct{ Version int64 }
	cp.rest.must(200, "POST", "/config/publish", note("daily 5"), "If-Match", tag).decode(t, &v)
	tenant, _ := created(t, cp.rest, "/tenants", map[string]any{"name": "acme", "plan_id": planID(t, cp.rest)})
	consumer, _ := created(t, cp.rest, "/consumers", map[string]any{"tenant_id": tenant, "name": "bot"})
	day := quota.Day(time.Now()).ID
	t.Cleanup(func() { dropTenant(t, rdb, tenant, []int64{consumer}, day, quota.Month(time.Now()).ID) })
	var k apiv1.IssuedApiKey
	cp.rest.must(201, "POST", fmt.Sprintf("/consumers/%d/keys", consumer), nil).decode(t, &k)
	g := startManaged(t, "gateway-1", grpcAddr, t.TempDir(), "SPILLWAY_GATEWAY_REDIS_ADDRS=redis-0:6379", addrs)
	g.waitVersion(t, int(v.Version))
	// Until the flusher seeds the new tenant (its next check, which sees the
	// identity change) the quota is unseeded and, with no fail_closed policy,
	// not enforced.
	eventually(t, "the flusher seeds the tenant's epoch", func() bool { return epochExists(t, rdb, tenant) })
	send := func() (int, http.Header) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, g.url+"/r", nil)
		req.Header.Set("X-API-Key", k.Key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	eventually(t, "the gateway has the consumer's key", func() bool { code, _ := send(); return code == http.StatusOK })
	for i := 2; i <= 5; i++ {
		if code, h := send(); code != http.StatusOK || !strings.Contains(h.Get("RateLimit-Policy"), `"quota/daily";q=5;w=86400`) {
			t.Fatalf("request %d: %d, RateLimit-Policy %q", i, code, h.Get("RateLimit-Policy"))
		}
	}
	code, h := send()
	left := time.Until(quota.Day(time.Now()).End)
	retry, _ := strconv.ParseInt(h.Get("Retry-After"), 10, 64)
	if code != http.StatusTooManyRequests || time.Duration(retry)*time.Second < left || time.Duration(retry)*time.Second > left+2*time.Second ||
		!strings.Contains(h.Get("RateLimit"), `"quota/daily";r=0;t=`) {
		t.Errorf("sixth request: %d, Retry-After %q (midnight is %v away), RateLimit %q", code, h.Get("Retry-After"), left, h.Get("RateLimit"))
	}
	start := time.Now()
	eventually(t, "quota_usage shows 5", func() bool {
		var l apiv1.QuotaUsageList
		cp.rest.must(200, "GET", fmt.Sprintf("/quota-usage?consumer_id=%d", consumer), nil).decode(t, &l)
		return len(l.Items) == 1 && l.Items[0].Daily.Used == 5 && l.Items[0].Daily.Quota == 5
	})
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("quota_usage showed 5 after %v, want within 15 s", took)
	}

	// A quota override reaches the gateway with the identity stream.
	created(t, cp.rest, "/quota-overrides", map[string]any{"consumer_id": consumer, "quotas": map[string]any{"daily": 7}})
	eventually(t, "the override's quota applies", func() bool { code, _ := send(); return code == http.StatusOK })
	g.stop(syscall.SIGTERM)
	cp.stop(syscall.SIGTERM)
	noKeyIn(t, "the gateway's log", g.out.String(), true, partsOf(t, k.Key))
	noKeyIn(t, "the control plane's log", cp.out.String(), true, partsOf(t, k.Key))
}
