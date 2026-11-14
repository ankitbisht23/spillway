package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// outageOptions are rule (c)'s settings (ADR-0042).
type outageOptions struct {
	rate        int           // requests per second over all replicas
	before      time.Duration // load before the stop
	length      time.Duration // the outage, from the stop to the start
	restart     string        // the replica restarted during the outage
	restartAt   time.Duration // from the stop
	after       time.Duration // load after the control plane is healthy
	every       time.Duration // scrape interval
	within      time.Duration // (c4) and (c5)'s bound
	slack       time.Duration // (c3)'s slack, (c5)'s sync age bound
	timeout     time.Duration // the load client's per-request timeout
	maxInFlight int
	actionWait  time.Duration // for the script's answer to an action
}

// probeTimeout bounds every probe and scrape (ADR-0042): over 4x the
// 820 ms planning stall.
const probeTimeout = 4 * time.Second

// maxFailureSamples failures are kept for the report.
const maxFailureSamples = 20

// loadCounts are what became of a replica's share of the load.
type loadCounts struct {
	Sent      int64            `json:"sent"`
	OK        int64            `json:"ok"`
	Status    map[string]int64 `json:"status,omitempty"` // other answers by status
	Timeout   int64            `json:"timeout"`
	Transport int64            `json:"transport"`
	Dropped   int64            `json:"dropped"`
}

func (c loadCounts) failed() int64 {
	n := c.Timeout + c.Transport + c.Dropped
	for _, v := range c.Status {
		n += v
	}
	return n
}

func (c *loadCounts) add(d loadCounts) {
	c.Sent += d.Sent
	c.OK += d.OK
	c.Timeout += d.Timeout
	c.Transport += d.Transport
	c.Dropped += d.Dropped
	for k, v := range d.Status {
		if c.Status == nil {
			c.Status = map[string]int64{}
		}
		c.Status[k] += v
	}
}

// loadFailure is one failed request, its due time in seconds from the stop.
type loadFailure struct {
	Gateway string  `json:"gateway"`
	AtS     float64 `json:"at_s"`
	What    string  `json:"what"`
	at      time.Time
}

// gwLoad is one replica's share of the load.
type gwLoad struct {
	name      string
	transport *http.Transport
	client    *http.Client
	paused    atomic.Bool
	inFlight  atomic.Int64 // requests
	scraping  atomic.Int64 // scrapes

	mu     sync.Mutex
	counts loadCounts
}

// outageScrape is one read of a replica's /readyz and /metrics.
type outageScrape struct {
	Gateway  string    `json:"gateway"`
	AtS      float64   `json:"at_s"` // send time, seconds from the stop
	OK       bool      `json:"ok"`
	Err      string    `json:"error,omitempty"`
	Ready    int       `json:"readyz"`
	Config   int64     `json:"config"`
	Identity int64     `json:"identity"`
	SyncAge  float64   `json:"sync_age_s"`
	Sent     time.Time `json:"-"`
	Recv     time.Time `json:"-"` // when the last answer was read
}

// outageTimes is the run's timeline on the sender's clock; a zero time
// never happened. Codes are the script's commands' exit codes, -1 unknown.
// Healthy is t_healthy: the last moment the sender saw the control plane
// not ready, by its own polls of the /readyz that compose's healthcheck
// reads (pollHealthy); HealthyLine is when the script said docker compose
// up returned, which can be up to compose's 2 s healthcheck interval
// later, while gateways may already be syncing again.
type outageTimes struct {
	Start, Stop, Stopped, Pause, Restarted, Ready, Resume, Healthy, HealthyLine, Live time.Time
	StopCode, RestartCode, StartCode                                                  int
	Errors                                                                            []string
}

type outageRun struct {
	r       *cpRun
	o       outageOptions
	key     string // in memory only
	gws     []*gwLoad
	sem     chan struct{}
	scraper *http.Client
	end     atomic.Pointer[time.Time] // when the load and the scrapes stop

	mu       sync.Mutex
	failures []loadFailure
}

func newOutageRun(r *cpRun, key string) *outageRun {
	o := r.o.outage
	u := &outageRun{r: r, o: o, key: key, sem: make(chan struct{}, o.maxInFlight), scraper: &http.Client{Timeout: probeTimeout}}
	for _, g := range r.o.gateways {
		// One transport per replica, so the restarted one's connections
		// can be closed alone; the client closes idle connections before
		// the gateway's 120 s idle timeout would.
		tr := &http.Transport{MaxIdleConns: o.maxInFlight, MaxIdleConnsPerHost: o.maxInFlight, IdleConnTimeout: time.Minute}
		u.gws = append(u.gws, &gwLoad{name: g, transport: tr, client: &http.Client{Transport: tr, Timeout: o.timeout}})
	}
	return u
}

func (u *outageRun) gw(name string) *gwLoad {
	for _, g := range u.gws {
		if g.name == name {
			return g
		}
	}
	return nil
}

// outageReport is rule (c)'s report.
type outageReport struct {
	Setup          string                `json:"setup,omitempty"`
	KeyID          int64                 `json:"key_id,omitempty"`
	TimesS         map[string]float64    `json:"times_s,omitempty"` // from the stop
	Codes          map[string]int        `json:"codes,omitempty"`
	TimelineErrors []string              `json:"timeline_errors,omitempty"`
	Load           loadCounts            `json:"load"`
	PerGateway     map[string]loadCounts `json:"per_gateway,omitempty"`
	OKAfterResume  int64                 `json:"ok_after_resume"`
	Failures       []loadFailure         `json:"failures,omitempty"`
	Baseline       map[string][2]int64   `json:"baseline,omitempty"` // config, identity
	Rules          []outageRule          `json:"rules"`
	RevokeError    string                `json:"revoke_error,omitempty"`
	Scrapes        []outageScrape        `json:"scrapes,omitempty"`
	Pass           bool                  `json:"pass"`
}

type outageRule struct {
	Check    string `json:"check"`
	Pass     bool   `json:"pass"`
	Measured string `json:"measured"`
}

// runOutage is rule (c), NFR-12: under the load, the script stops the
// control plane and Postgres, restarts one replica halfway through the
// outage and starts the control plane again; the sender asks for each
// action with a line "action stop", "action restart <replica>" or
// "action start" on stderr, and the script answers on stdin with
// "stopping" just before docker compose stop and "stopped <exit code>"
// after it, "restarted <exit code>" and "healthy <exit code>". t_stop is
// when "stopping" is read, t_healthy when "healthy" is.
func (r *cpRun) runOutage(ctx context.Context, stdin io.Reader, stderr io.Writer) outageReport {
	rep := outageReport{}
	_, consumer, err := r.consumer(ctx)
	if err == nil && consumer == 0 {
		err = errors.New("consumer " + m5Name + " does not exist: run setup first")
	}
	var issued struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	if err == nil {
		_, err = r.rest.must(ctx, http.StatusCreated, http.MethodPost, fmt.Sprintf("/consumers/%d/keys", consumer), map[string]any{}, &issued)
	}
	if err != nil {
		rep.Setup = err.Error()
		return rep
	}
	rep.KeyID = issued.ID
	defer func() {
		if _, err := r.rest.must(context.WithoutCancel(ctx), http.StatusOK, http.MethodPost, fmt.Sprintf("/keys/%d/revoke", issued.ID), map[string]string{"note": "make verify-m5: rule (c)"}, nil); err != nil {
			rep.RevokeError = err.Error()
		}
	}()
	u := newOutageRun(r, issued.Key)
	if err := u.keyWorks(ctx); err != nil {
		rep.Setup = err.Error()
		return rep
	}
	t, scrapes, okAfterResume := u.timeline(ctx, readLines(stdin), stderr)

	rep.Failures = u.failures
	rep.PerGateway = map[string]loadCounts{}
	for _, g := range u.gws {
		rep.PerGateway[g.name] = g.counts
		rep.Load.add(g.counts)
	}
	rep.OKAfterResume = okAfterResume
	rep.TimelineErrors = t.Errors
	rep.Codes = map[string]int{"stop": t.StopCode, "restart": t.RestartCode, "start": t.StartCode}
	rep.TimesS = map[string]float64{}
	for name, at := range map[string]time.Time{"start": t.Start, "stopped": t.Stopped, "pause": t.Pause, "restarted": t.Restarted,
		"ready": t.Ready, "resume": t.Resume, "healthy": t.Healthy, "healthy_line": t.HealthyLine, "live": t.Live} {
		if !at.IsZero() && !t.Stop.IsZero() {
			rep.TimesS[name] = seconds(at.Sub(t.Stop))
		}
	}
	for i := range rep.Failures {
		rep.Failures[i].AtS = seconds(rep.Failures[i].at.Sub(t.Stop))
	}
	for i := range scrapes {
		scrapes[i].AtS = seconds(scrapes[i].Sent.Sub(t.Stop))
	}
	rep.Scrapes = scrapes
	rep.Rules, rep.Baseline = analyzeOutage(r.o.outage, r.o.gateways, scrapes, t, rep.Load, okAfterResume)
	rep.Pass = true
	for _, x := range rep.Rules {
		rep.Pass = rep.Pass && x.Pass
	}
	return rep
}

func seconds(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }

// keyWorks probes every replica with the key until each answers 200: its
// identity version is applied everywhere.
func (u *outageRun) keyWorks(ctx context.Context) error {
	for _, g := range u.gws {
		deadline := time.Now().Add(10 * time.Second)
		for {
			code, err := u.probe(ctx, g)
			if code == http.StatusOK {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				last := "status " + strconv.Itoa(code)
				if err != nil {
					last = err.Error()
				}
				return fmt.Errorf("the new key does not work on %s after 10 s (last: %s)", g.name, last)
			}
			sleepUntil(ctx, time.Now().Add(50*time.Millisecond))
		}
	}
	return nil
}

func (u *outageRun) probe(ctx context.Context, g *gwLoad) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.r.traffic(g.name)+"/status/200", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", u.key)
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, errors.New("transport error or timeout")
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// timeline runs the load and the scrapes, asks the script for each action
// in turn, and returns the timeline, the scrapes and the restarted
// replica's 2xx answers after it was resumed.
func (u *outageRun) timeline(ctx context.Context, lines <-chan stdinLine, stderr io.Writer) (outageTimes, []outageScrape, int64) {
	o := u.o
	t := outageTimes{StopCode: -1, RestartCode: -1, StartCode: -1, Start: time.Now().Add(100 * time.Millisecond)}
	var wg sync.WaitGroup
	var scrapes []outageScrape
	wg.Go(func() { scrapes = u.scrapeLoop(ctx, t.Start) })
	wg.Go(func() { u.drive(ctx, t.Start) })
	stop := func(at time.Time) { u.end.Store(&at) }
	failed := func(what string, err error) { t.Errors = append(t.Errors, what+": "+err.Error()) }
	var okBefore int64
	g := u.gw(o.restart)

	sleepUntil(ctx, t.Start.Add(o.before))
	_, _ = fmt.Fprintln(stderr, "action stop")
	if l, _, err := expect(ctx, lines, "stopping", o.actionWait); err != nil {
		failed("stop", err)
	} else {
		t.Stop = l.at
		if l, code, err := expect(ctx, lines, "stopped", o.actionWait); err != nil {
			failed("stop", err)
		} else {
			t.Stopped, t.StopCode = l.at, code
		}
	}
	if len(t.Errors) == 0 {
		sleepUntil(ctx, t.Stop.Add(o.restartAt))
		okBefore = u.restart(ctx, g, lines, stderr, &t)
	}
	if !t.Stop.IsZero() && ctx.Err() == nil {
		sleepUntil(ctx, t.Stop.Add(o.length))
		startAt := time.Now()
		_, _ = fmt.Fprintln(stderr, "action start")
		pctx, cancel := context.WithCancel(ctx)
		polled := make(chan time.Time, 1)
		go func() { polled <- u.pollHealthy(pctx, startAt) }()
		if l, code, err := expect(ctx, lines, "healthy", o.actionWait); err != nil {
			failed("start", err)
		} else {
			t.HealthyLine, t.StartCode = l.at, code
		}
		if t.StartCode != 0 {
			cancel()
		}
		select {
		case t.Healthy = <-polled:
		case <-time.After(probeTimeout + time.Second): // a poll in flight
			t.Errors = append(t.Errors, "start: the control plane's /readyz did not answer 200 by the script's healthy")
		}
		cancel()
		if t.Healthy.IsZero() || t.HealthyLine.Before(t.Healthy) {
			t.Healthy = t.HealthyLine
		}
	}
	if t.Healthy.IsZero() {
		stop(time.Now())
	} else {
		stop(later(t.Healthy, t.HealthyLine).Add(o.after))
		if t.StartCode == 0 {
			t.Live = u.waitLive(ctx, t.Healthy.Add(o.within))
		}
	}
	wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.Resume.IsZero() {
		return t, scrapes, 0
	}
	return t, scrapes, g.counts.OK - okBefore
}

// expect waits up to wait for the next stdin line, which must start with
// word; an exit code may follow it.
func expect(ctx context.Context, lines <-chan stdinLine, word string, wait time.Duration) (stdinLine, int, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case l, ok := <-lines:
		if !ok {
			return l, -1, fmt.Errorf("stdin closed while waiting for %q", word)
		}
		f := strings.Fields(l.text)
		if len(f) == 0 || f[0] != word {
			return l, -1, fmt.Errorf("got %q, want %q", l.text, word)
		}
		code := 0
		if len(f) > 1 {
			var err error
			if code, err = strconv.Atoi(f[1]); err != nil {
				return l, -1, fmt.Errorf("got %q: not an exit code", l.text)
			}
		}
		return l, code, nil
	case <-timer.C:
		return stdinLine{}, -1, fmt.Errorf("no %q within %v", word, wait)
	case <-ctx.Done():
		return stdinLine{}, -1, ctx.Err()
	}
}

// restart pauses g's share of the load until its requests in flight are
// answered, asks for its restart, waits up to o.within for its /readyz,
// closes its idle connections (all to the old process) and resumes it. It
// returns g's 2xx count when it resumed. A replica that does not come back
// stays paused.
func (u *outageRun) restart(ctx context.Context, g *gwLoad, lines <-chan stdinLine, stderr io.Writer, t *outageTimes) int64 {
	g.paused.Store(true)
	t.Pause = time.Now()
	deadline := t.Pause.Add(u.o.timeout + time.Second) // the client's timeout bounds them
	for g.inFlight.Load() > 0 && time.Now().Before(deadline) && ctx.Err() == nil {
		sleepUntil(ctx, time.Now().Add(10*time.Millisecond))
	}
	// A scrape sent before the pause is not exempt from (c2): it must be
	// answered by the old process, as must every request.
	deadline = time.Now().Add(2*probeTimeout + time.Second)
	for g.scraping.Load() > 0 && time.Now().Before(deadline) && ctx.Err() == nil {
		sleepUntil(ctx, time.Now().Add(10*time.Millisecond))
	}
	_, _ = fmt.Fprintln(stderr, "action restart "+g.name)
	l, code, err := expect(ctx, lines, "restarted", u.o.actionWait)
	if err != nil {
		t.Errors = append(t.Errors, "restart: "+err.Error())
		return 0
	}
	t.Restarted, t.RestartCode = l.at, code
	if code != 0 {
		return 0
	}
	for time.Since(t.Restarted) <= u.o.within && ctx.Err() == nil {
		if s := u.scrape(ctx, g.name, true); s.Ready == http.StatusOK {
			t.Ready = time.Now()
			break
		}
		sleepUntil(ctx, time.Now().Add(100*time.Millisecond))
	}
	if t.Ready.IsZero() {
		return 0
	}
	g.transport.CloseIdleConnections()
	g.mu.Lock()
	ok := g.counts.OK
	g.mu.Unlock()
	t.Resume = time.Now()
	g.paused.Store(false)
	return ok
}

// pollHealthy reads the control plane's /readyz every 100 ms from start,
// each poll with probeTimeout, until one answers 200, and returns the send
// time of the last poll before it that did not (start if the first did);
// zero if ctx ends first. That poll was handled while the control plane
// was not ready, and the control plane is ready before its election opens
// the gRPC listener, so no gateway can have synced before the time
// returned.
func (u *outageRun) pollHealthy(ctx context.Context, start time.Time) time.Time {
	notReady := start
	for ctx.Err() == nil {
		sent := time.Now()
		if code, _, err := u.get(ctx, u.r.o.controlPlaneAdmin+"/readyz"); err == nil && code == http.StatusOK {
			return notReady
		}
		notReady = sent
		sleepUntil(ctx, sent.Add(100*time.Millisecond))
	}
	return time.Time{}
}

// waitLive reads GET /replicas every second until every replica is live
// and watching, up to deadline; it returns when, zero for never.
func (u *outageRun) waitLive(ctx context.Context, deadline time.Time) time.Time {
	for {
		rs, err := u.r.replicas(ctx)
		if err == nil && u.r.allAt(rs, 0, 0) {
			return time.Now()
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return time.Time{}
		}
		sleepUntil(ctx, time.Now().Add(time.Second))
	}
}

// drive sends o.rate requests per second from start, round robin over
// the replicas that are not paused, until the end is set and reached:
// open model, overdue requests sent at once, a request due while
// o.maxInFlight are in flight dropped (ADR-0014).
func (u *outageRun) drive(ctx context.Context, start time.Time) {
	interval := time.Second / time.Duration(u.o.rate)
	var wg sync.WaitGroup
	for i := 0; ; i++ {
		due := start.Add(time.Duration(i) * interval)
		if e := u.end.Load(); e != nil && !due.Before(*e) {
			break
		}
		sleepUntil(ctx, due)
		if ctx.Err() != nil {
			break
		}
		g := u.pick(i)
		select {
		case u.sem <- struct{}{}:
		default:
			g.inFlight.Add(-1)
			g.mu.Lock()
			g.counts.Dropped++
			g.mu.Unlock()
			u.fail(g.name, due, "dropped")
			continue
		}
		wg.Go(func() {
			defer func() { <-u.sem }()
			defer g.inFlight.Add(-1)
			u.send(ctx, g)
		})
	}
	wg.Wait()
}

// pick returns request i's replica, its in-flight count already raised:
// replica i mod n or, while that one is paused, one of the others in turn,
// so they share its load. The count is raised before the pause is read,
// so restart's wait for in-flight requests cannot miss one. Only one
// replica is ever paused.
func (u *outageRun) pick(i int) *gwLoad {
	n := len(u.gws)
	g := u.gws[i%n]
	g.inFlight.Add(1)
	if !g.paused.Load() || n == 1 {
		return g
	}
	g.inFlight.Add(-1)
	g = u.gws[(i%n+1+(i/n)%(n-1))%n]
	g.inFlight.Add(1)
	return g
}

func (u *outageRun) send(ctx context.Context, g *gwLoad) {
	sent := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.r.traffic(g.name)+"/status/200", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-API-Key", u.key)
	resp, err := g.client.Do(req)
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counts.Sent++
	switch {
	case err != nil && errorKind(err) == "timeout":
		g.counts.Timeout++
		u.fail(g.name, sent, "timeout: "+err.Error()) // the error names the URL, never the key header
	case err != nil:
		g.counts.Transport++
		u.fail(g.name, sent, "transport: "+err.Error())
	case resp.StatusCode/100 == 2:
		g.counts.OK++
	default:
		if g.counts.Status == nil {
			g.counts.Status = map[string]int64{}
		}
		g.counts.Status[strconv.Itoa(resp.StatusCode)]++
		u.fail(g.name, sent, "status "+strconv.Itoa(resp.StatusCode))
	}
}

func (u *outageRun) fail(gateway string, at time.Time, what string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.failures) < maxFailureSamples {
		u.failures = append(u.failures, loadFailure{Gateway: gateway, What: what, at: at})
	}
}

// scrapeLoop scrapes every replica every o.every from first until a tick
// after the end.
func (u *outageRun) scrapeLoop(ctx context.Context, first time.Time) []outageScrape {
	var out []outageScrape
	for k := 0; ; k++ {
		at := first.Add(time.Duration(k) * u.o.every)
		if e := u.end.Load(); e != nil && at.After(*e) {
			return out
		}
		sleepUntil(ctx, at)
		if ctx.Err() != nil {
			return out
		}
		round := make([]outageScrape, len(u.gws))
		var wg sync.WaitGroup
		for i, g := range u.gws {
			wg.Go(func() { round[i] = u.scrape(ctx, g.name, false) })
		}
		wg.Wait()
		out = append(out, round...)
	}
}

// scrape reads name's /readyz and, unless readyOnly, its /metrics, each
// with probeTimeout. It is OK when both were read and the metrics hold the
// three series; a /readyz other than 200 is recorded, not a failure.
func (u *outageRun) scrape(ctx context.Context, name string, readyOnly bool) (s outageScrape) {
	if g := u.gw(name); g != nil {
		g.scraping.Add(1) // before the send time is taken: see restart
		defer g.scraping.Add(-1)
	}
	s = outageScrape{Gateway: name, Sent: time.Now()}
	defer func() { s.Recv = time.Now() }()
	code, _, err := u.get(ctx, "http://"+u.r.admin(name)+"/readyz")
	if err != nil {
		s.Err = "readyz: " + err.Error()
		return s
	}
	s.Ready = code
	if readyOnly {
		return s
	}
	code, body, err := u.get(ctx, "http://"+u.r.admin(name)+"/metrics")
	switch {
	case err != nil:
		s.Err = "metrics: " + err.Error()
		return s
	case code != http.StatusOK:
		s.Err = fmt.Sprintf("metrics: status %d", code)
		return s
	}
	found := 0
	for l := range strings.Lines(string(body)) {
		name, _, v, ok := parseSample(l)
		switch {
		case !ok:
		case name == "gw_config_version":
			s.Config, found = int64(v), found|1
		case name == "gw_identity_version":
			s.Identity, found = int64(v), found|2
		case name == "gw_config_sync_age_seconds":
			s.SyncAge, found = v, found|4
		}
	}
	if found != 7 {
		s.Err = "metrics: gw_config_version, gw_identity_version or gw_config_sync_age_seconds missing"
		return s
	}
	s.OK = true
	return s
}

func (u *outageRun) get(ctx context.Context, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: the compose network's services are the point
	if err != nil {
		return 0, nil, err
	}
	resp, err := u.scraper.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return 0, nil, errors.New(errorKind(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, errors.New("reading the answer: " + errorKind(err))
	}
	return resp.StatusCode, body, nil
}

// analyzeOutage checks rules (c1) to (c5) (ADR-0042) and returns them with
// each replica's versions at the last scrape before the stop. (c2) and
// (c3) take the scrapes sent at or after t_stop and answered before
// t_healthy; (c5) those sent at or after t_healthy.
func analyzeOutage(o outageOptions, gateways []string, scrapes []outageScrape, t outageTimes, load loadCounts, okAfterResume int64) ([]outageRule, map[string][2]int64) {
	elapsed := func(s outageScrape) time.Duration { return s.Sent.Sub(t.Stop) }
	healthy := t.Healthy
	if healthy.IsZero() {
		healthy = time.Unix(1<<40, 0) // never: every scrape after the stop is in the outage
	}
	// The baseline: each replica's last scrape before the stop.
	baseline := map[string][2]int64{}
	for _, s := range scrapes {
		if s.OK && s.Ready == http.StatusOK && s.Sent.Before(t.Stop) {
			baseline[s.Gateway] = [2]int64{s.Config, s.Identity}
		}
	}
	// The restarted replica's first ready scrape after its restart, and
	// its exemption from its pause until then.
	var firstReady *outageScrape
	if !t.Restarted.IsZero() {
		for i := range scrapes {
			s := &scrapes[i]
			if s.Gateway == o.restart && !s.Sent.Before(t.Restarted) && s.OK && s.Ready == http.StatusOK {
				firstReady = s
				break
			}
		}
	}
	exempt := func(s outageScrape) bool {
		return s.Gateway == o.restart && !t.Pause.IsZero() && !s.Sent.Before(t.Pause) && (firstReady == nil || s.Sent.Before(firstReady.Sent))
	}
	at := func(s outageScrape) bool { // at the baseline versions
		b, ok := baseline[s.Gateway]
		return ok && b == [2]int64{s.Config, s.Identity}
	}
	describe := func(s outageScrape) string {
		return fmt.Sprintf("%s at %+.1f s: ok %v %s readyz %d config %d identity %d sync age %.1f s", s.Gateway, seconds(elapsed(s)), s.OK, s.Err, s.Ready, s.Config, s.Identity, s.SyncAge)
	}

	// (c1)
	c1 := outageRule{Check: "(c1) zero failed requests", Pass: load.failed() == 0 && load.Sent > 0 && load.OK == load.Sent,
		Measured: fmt.Sprintf("%d requests: %d 2xx, other statuses %s, %d timeouts, %d transport errors, %d dropped",
			load.Sent+load.Dropped, load.OK, failures(statusCounts(load.Status)), load.Timeout, load.Transport, load.Dropped)}

	// (c2) and (c3), over the scrapes from the stop to healthy.
	var n, bad2, bad3 int
	var first2, first3 string
	minMargin := 1e9
	last := map[string]outageScrape{}
	for _, s := range scrapes {
		// In the outage: sent at or after t_stop and answered before
		// t_healthy, so no answer can come from after a resync.
		if t.Stop.IsZero() || s.Sent.Before(t.Stop) || !s.Recv.Before(healthy) || exempt(s) {
			continue
		}
		n++
		if !s.OK { // shows neither (c2) nor (c3)
			bad2, bad3 = bad2+1, bad3+1
			first2, first3 = cmp.Or(first2, describe(s)), cmp.Or(first3, describe(s))
			continue
		}
		last[s.Gateway] = s
		if s.Ready != http.StatusOK || !at(s) {
			bad2++
			first2 = cmp.Or(first2, describe(s))
		}
		margin := s.SyncAge - (seconds(elapsed(s)) - seconds(o.slack))
		minMargin = min(minMargin, margin)
		if margin < 0 {
			bad3++
			first3 = cmp.Or(first3, describe(s))
		}
	}
	c2 := outageRule{Check: "(c2) config frozen at last good"}
	c3 := outageRule{Check: "(c3) sync age grows through the outage"}
	missing := []string{}
	for _, g := range gateways {
		if _, ok := baseline[g]; !ok {
			missing = append(missing, g)
		}
	}
	switch {
	case t.Stop.IsZero():
		c2.Measured = "the control plane was never stopped"
	case t.StopCode != 0:
		c2.Measured = fmt.Sprintf("docker compose stop exited %d", t.StopCode)
	case len(missing) > 0:
		c2.Measured = "no ready scrape before the stop of " + strings.Join(missing, ", ")
	case bad2 > 0:
		c2.Measured = fmt.Sprintf("%d of %d scrapes in the outage failed, unready or at other versions; first: %s", bad2, n, first2)
	case n == 0:
		c2.Measured = "no scrape in the outage"
	default:
		c2.Pass = true
		c2.Measured = fmt.Sprintf("%d scrapes from the stop to healthy: all ready at %s", n, describeBaseline(gateways, baseline))
	}
	lastMin, lastMax := 1e9, 0.0
	var short []string
	for _, g := range gateways {
		s, ok := last[g]
		if !ok {
			short = append(short, g+" never scraped")
			continue
		}
		lastMin, lastMax = min(lastMin, s.SyncAge), max(lastMax, s.SyncAge)
		if s.SyncAge < seconds(o.length-o.slack) {
			short = append(short, describe(s))
		}
	}
	switch {
	case t.Stop.IsZero() || n == 0:
		c3.Measured = "no scrape in the outage"
	case bad3 > 0:
		c3.Measured = fmt.Sprintf("%d of %d scrapes failed or below (time since the stop - %.0f s); first: %s", bad3, n, seconds(o.slack), first3)
	case len(short) > 0:
		c3.Measured = fmt.Sprintf("last scrape before healthy below %.0f s: %s", seconds(o.length-o.slack), strings.Join(short, "; "))
	default:
		c3.Pass = true
		c3.Measured = fmt.Sprintf("%d scrapes: sync age >= time since the stop - %.0f s, least margin %.1f s; last before healthy %.0f-%.0f s (want >= %.0f s)",
			n, seconds(o.slack), minMargin, lastMin, lastMax, seconds(o.length-o.slack))
	}

	// (c4)
	c4 := outageRule{Check: fmt.Sprintf("(c4) %s boots from its on-disk snapshot", o.restart)}
	switch {
	case t.Pause.IsZero() || t.Restarted.IsZero():
		c4.Measured = "never restarted"
	case t.RestartCode != 0:
		c4.Measured = fmt.Sprintf("docker compose restart exited %d", t.RestartCode)
	case t.Ready.IsZero():
		c4.Measured = fmt.Sprintf("not ready within %.0f s of the restart", seconds(o.within))
	case firstReady == nil || !firstReady.Sent.Before(healthy):
		c4.Measured = "no ready scrape before the control plane was back"
	default:
		ready := t.Ready.Sub(t.Restarted)
		s := *firstReady
		c4.Pass = ready <= o.within && at(s) && s.SyncAge >= seconds(elapsed(s)-o.slack) && okAfterResume > 0
		c4.Measured = fmt.Sprintf("ready %.1f s after docker compose restart returned (limit %.0f s); first ready scrape at %+.1f s: config %d, identity %d, sync age %.0f s; %d 2xx after resuming",
			seconds(ready), seconds(o.within), seconds(elapsed(s)), s.Config, s.Identity, s.SyncAge, okAfterResume)
	}

	// (c5)
	c5 := outageRule{Check: fmt.Sprintf("(c5) recovery within %.0f s", seconds(o.within))}
	if t.Healthy.IsZero() || t.StartCode != 0 {
		c5.Measured = fmt.Sprintf("the control plane did not come back (docker compose up exited %d)", t.StartCode)
	} else {
		synced := map[string]time.Duration{}
		var after, off int
		var firstOff string
		for _, s := range scrapes {
			if s.Sent.Before(t.Healthy) {
				continue
			}
			after++
			if !s.OK || !at(s) {
				off++
				firstOff = cmp.Or(firstOff, describe(s))
				continue
			}
			if _, ok := synced[s.Gateway]; !ok && s.SyncAge < seconds(o.slack) {
				synced[s.Gateway] = s.Sent.Sub(t.Healthy)
			}
		}
		var latest time.Duration
		var unsynced []string
		for _, g := range gateways {
			d, ok := synced[g]
			if !ok || d > o.within {
				unsynced = append(unsynced, g)
			}
			latest = max(latest, d)
		}
		live := "never"
		if !t.Live.IsZero() {
			live = fmt.Sprintf("after %.1f s", seconds(t.Live.Sub(t.Healthy)))
		}
		c5.Pass = len(unsynced) == 0 && !t.Live.IsZero() && t.Live.Sub(t.Healthy) <= o.within && off == 0 && after > 0
		c5.Measured = fmt.Sprintf("sync age < %.0f s on all %d after %.1f s, all live in GET /replicas %s (limit %.0f s); %d of %d scrapes after healthy at the same versions",
			seconds(o.slack), len(gateways), seconds(latest), live, seconds(o.within), after-off, after)
		if len(unsynced) > 0 {
			c5.Measured += "; not synced in time: " + strings.Join(unsynced, ", ")
		}
		if off > 0 {
			c5.Measured += "; first other: " + firstOff
		}
	}
	return []outageRule{c1, c2, c3, c4, c5}, baseline
}

func statusCounts(m map[string]int64) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		out[k] = int(v)
	}
	return out
}

func describeBaseline(gateways []string, b map[string][2]int64) string {
	first := b[gateways[0]]
	same := true
	for _, g := range gateways {
		same = same && b[g] == first
	}
	if same {
		return fmt.Sprintf("config %d, identity %d, as before the stop", first[0], first[1])
	}
	var parts []string
	for _, g := range slices.Sorted(maps.Keys(b)) {
		parts = append(parts, fmt.Sprintf("%s config %d identity %d", g, b[g][0], b[g][1]))
	}
	return strings.Join(parts, ", ") + ", as before the stop"
}

func (rep outageReport) lines(o outageOptions) []string {
	if rep.Setup != "" {
		var ls []string
		for _, c := range []string{"(c1)", "(c2)", "(c3)", "(c4)", "(c5)"} {
			ls = append(ls, line(false, c+" not run", "setup: "+rep.Setup))
		}
		return ls
	}
	var ls []string
	for _, x := range rep.Rules {
		ls = append(ls, line(x.Pass, x.Check, x.Measured))
	}
	ts := rep.TimesS
	timeline := fmt.Sprintf("      timeline (s from the stop): load from %.1f, compose stop done %.1f; %s paused %.1f, restarted %.1f, ready %.1f, resumed %.1f; control plane healthy %.1f (compose up returned %.1f), all live %.1f",
		ts["start"], ts["stopped"], o.restart, ts["pause"], ts["restarted"], ts["ready"], ts["resume"], ts["healthy"], ts["healthy_line"], ts["live"])
	ls = append(ls, timeline)
	var per []string
	for _, g := range slices.Sorted(maps.Keys(rep.PerGateway)) {
		c := rep.PerGateway[g]
		per = append(per, fmt.Sprintf("%s %d/%d", g, c.OK, c.Sent+c.Dropped))
	}
	ls = append(ls, "      2xx per replica: "+strings.Join(per, ", "))
	for i, f := range rep.Failures {
		if i == 5 {
			ls = append(ls, fmt.Sprintf("      … %d more failures in the JSON report", len(rep.Failures)-i))
			break
		}
		ls = append(ls, fmt.Sprintf("      failure: %s at %+.3f s: %s", f.Gateway, f.AtS, f.What))
	}
	for _, e := range rep.TimelineErrors {
		ls = append(ls, "      timeline: "+e)
	}
	if rep.RevokeError != "" {
		ls = append(ls, "      revoking the load key: "+rep.RevokeError)
	}
	return ls
}
