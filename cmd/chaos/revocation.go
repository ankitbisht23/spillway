package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// revocationOptions are "chaos revocation"'s flags.
type revocationOptions struct {
	controlPlane string   // REST base URL, /api/v1 included
	consumer     int64    // the consumer the keys are issued for
	gateways     []string // host:port of each replica's traffic listener
	path         string   // a route that resolves a sent key
	rounds       int
	limit        time.Duration // each round's sample must be below it (NFR-09)
	every        time.Duration // probe interval per replica
	after        int           // 401s that must follow the first one
	timeout      time.Duration // per probe
	restTimeout  time.Duration // per REST call
	readyWait    time.Duration // for a new key to work on every replica
}

func parseRevocationFlags(args []string, stderr io.Writer) (revocationOptions, error) {
	o := revocationOptions{}
	var gateways string
	fs := flag.NewFlagSet("chaos revocation", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.controlPlane, "controlplane", "http://controlplane:8080/api/v1", "the control plane's REST API")
	fs.Int64Var(&o.consumer, "consumer", 0, "ID of the consumer the keys are issued for (required)")
	fs.StringVar(&gateways, "gateways", "gateway-1:8000,gateway-2:8000,gateway-3:8000,gateway-4:8000,gateway-5:8000", "the replicas, comma-separated host:port")
	fs.StringVar(&o.path, "path", "/status/200", "path of a route that resolves a sent key")
	fs.IntVar(&o.rounds, "rounds", 5, "revocations")
	fs.DurationVar(&o.limit, "limit", 5*time.Second, "each revocation must be rejected on every replica within this (NFR-09)")
	fs.DurationVar(&o.every, "every", 50*time.Millisecond, "probe interval per replica")
	fs.IntVar(&o.after, "after", 10, "probes after the first 401 that must all be 401")
	fs.DurationVar(&o.timeout, "timeout", 4*time.Second, "per-probe timeout")
	fs.DurationVar(&o.restTimeout, "rest-timeout", 10*time.Second, "per-call timeout of the REST API")
	fs.DurationVar(&o.readyWait, "ready-wait", 10*time.Second, "how long a new key may take to work on every replica")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	for g := range strings.SplitSeq(gateways, ",") {
		if g = strings.TrimSpace(g); g != "" {
			o.gateways = append(o.gateways, g)
		}
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected arguments %q", fs.Args())
	case o.consumer <= 0:
		err = errors.New("-consumer: required")
	case len(o.gateways) == 0:
		err = errors.New("-gateways: none")
	case o.rounds < 1 || o.after < 0:
		err = errors.New("-rounds must be positive and -after not negative")
	case o.limit <= 0 || o.every <= 0 || o.timeout <= 0 || o.restTimeout <= 0 || o.readyWait <= 0:
		err = errors.New("-limit, -every, -timeout, -rest-timeout and -ready-wait: must be positive")
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos revocation:", err)
	}
	return o, err
}

// revocationRound is one revocation's result, in milliseconds from just
// before the revoke request was sent.
type revocationRound struct {
	KeyID      int64              `json:"key_id"`
	RejectedMS map[string]float64 `json:"rejected_ms"` // first 401 per replica
	MaxMS      float64            `json:"max_ms"`
	Error      string             `json:"error,omitempty"`
}

type revocationReport struct {
	Rounds  []revocationRound `json:"rounds"`
	P50MS   float64           `json:"p50_ms"`
	MaxMS   float64           `json:"max_ms"`
	LimitMS float64           `json:"limit_ms"`
	Pass    bool              `json:"pass"`
}

// runRevocation is "chaos revocation", the sender that measures key
// revocation on every replica (CP-17, NFR-09, ADR-0042's rule b), in the
// compose network. It reads a control-plane bearer token on stdin. Each
// round issues a key for -consumer, probes every replica with it until
// each answers 200, then, its clock read just before, revokes the key, and
// probes each replica every -every until a 401 and -after more probes,
// which must all be 401. A transport error, a timeout or any other status
// fails the round. A round's sample is the latest replica's first 401;
// every sample must be below -limit. The key and the token stay in memory:
// nothing prints them. It prints its report as JSON on stdout and a
// PASS/FAIL line on stderr, and exits 0 on PASS, 1 on FAIL and 2 on bad
// flags or input.
//
//	chaos revocation -consumer <id> < token
func runRevocation(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseRevocationFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	token, err := bufio.NewReader(stdin).ReadString('\n')
	if token = strings.TrimSpace(token); token == "" {
		_, _ = fmt.Fprintln(stderr, "chaos revocation: no bearer token on stdin", err)
		return 2
	}
	r := revocationRun{o: o, token: token, client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}}
	rep := revocationReport{LimitMS: ms(o.limit), Pass: true}
	var samples []float64
	for range o.rounds {
		round := r.round(ctx)
		if round.Error != "" || round.MaxMS >= ms(o.limit) {
			rep.Pass = false
		}
		if round.Error == "" {
			samples = append(samples, round.MaxMS)
		}
		rep.Rounds = append(rep.Rounds, round)
		if ctx.Err() != nil {
			break
		}
	}
	slices.Sort(samples)
	if n := len(samples); n > 0 {
		rep.P50MS, rep.MaxMS = samples[(n-1)/2], samples[n-1]
	}
	_ = json.NewEncoder(stdout).Encode(rep)
	verdict := map[bool]string{true: "PASS", false: "FAIL"}[rep.Pass]
	_, _ = fmt.Fprintf(stderr, "%s revocation rejected on all %d replicas: %d of %d rounds, p50 %.0f ms, max %.0f ms (limit %.0f ms, every sample)\n",
		verdict, len(o.gateways), len(samples), o.rounds, rep.P50MS, rep.MaxMS, rep.LimitMS)
	if !rep.Pass {
		return 1
	}
	return 0
}

type revocationRun struct {
	o      revocationOptions
	token  string
	client *http.Client
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// round runs one revocation.
func (r *revocationRun) round(ctx context.Context) revocationRound {
	out := revocationRound{RejectedMS: map[string]float64{}}
	var issued struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	if err := r.rest(ctx, fmt.Sprintf("/consumers/%d/keys", r.o.consumer), http.StatusCreated, &issued); err != nil {
		out.Error = "issue: " + err.Error()
		return out
	}
	out.KeyID = issued.ID
	// The key works on every replica: its identity version is applied.
	for _, g := range r.o.gateways {
		deadline := time.Now().Add(r.o.readyWait)
		for {
			code, err := r.probe(ctx, g, issued.Key)
			if code == http.StatusOK {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				out.Error = fmt.Sprintf("setup: the new key does not work on %s after %v (last: %d %v)", g, r.o.readyWait, code, err)
				_ = r.rest(ctx, fmt.Sprintf("/keys/%d/revoke", issued.ID), http.StatusOK, nil)
				return out
			}
			time.Sleep(r.o.every)
		}
	}
	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	for _, g := range r.o.gateways {
		wg.Go(func() {
			at, err := r.untilRejected(ctx, g, issued.Key, start)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, g+": "+err.Error())
				return
			}
			out.RejectedMS[g] = ms(at)
			out.MaxMS = max(out.MaxMS, ms(at))
		})
	}
	if err := r.rest(ctx, fmt.Sprintf("/keys/%d/revoke", issued.ID), http.StatusOK, nil); err != nil {
		mu.Lock()
		errs = append(errs, "revoke: "+err.Error())
		mu.Unlock()
	}
	wg.Wait()
	slices.Sort(errs)
	out.Error = strings.Join(errs, "; ")
	return out
}

// untilRejected probes g every r.o.every from start until a 401, which it
// returns the time of, then r.o.after more probes that must be 401 too.
// It gives up 3 limits after start.
func (r *revocationRun) untilRejected(ctx context.Context, g, key string, start time.Time) (time.Duration, error) {
	var rejected time.Duration
	after := -1 // probes since the first 401
	for next := start; after < r.o.after; next = next.Add(r.o.every) {
		if wait := time.Until(next); wait > 0 {
			time.Sleep(wait)
		}
		code, err := r.probe(ctx, g, key)
		at := time.Since(start)
		switch {
		case err != nil:
			return 0, err
		case code == http.StatusUnauthorized:
			if after < 0 {
				rejected = at
			}
			after++
		case code == http.StatusOK && after < 0:
			if at > 3*r.o.limit {
				return 0, fmt.Errorf("still 200 after %v", at.Round(time.Millisecond))
			}
		case code == http.StatusOK:
			return 0, fmt.Errorf("200 at %v after a 401 at %v: the key came back", at.Round(time.Millisecond), rejected.Round(time.Millisecond))
		default:
			return 0, fmt.Errorf("status %d at %v", code, at.Round(time.Millisecond))
		}
	}
	return rejected, nil
}

// probe sends GET path to g with key; it never reports the key.
func (r *revocationRun) probe(ctx context.Context, g, key string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.o.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+g+r.o.path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", key)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, errors.New("transport error or timeout") // the error may quote the request
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// rest POSTs an empty JSON object to the control plane's path and decodes
// the answer into out unless nil; any status but want is an error, with
// the answer's error message (which never holds a key).
func (r *revocationRun) rest(ctx context.Context, path string, want int, out any) error {
	ctx, cancel := context.WithTimeout(ctx, r.o.restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.o.controlPlane+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("transport error or timeout")
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		var e struct{ Error string }
		_ = json.Unmarshal(body, &e)
		return fmt.Errorf("status %d: %s", resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}
