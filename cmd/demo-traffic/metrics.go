package main

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// scrape is one read of a gateway's admin metrics, with when it was sent
// and when its answer was read (ADR-0031's bounds of its read time).
type scrape struct {
	sent, recv time.Time
	ok         bool
	samples    []sample
}

type sample struct {
	name   string
	labels map[string]string
	v      float64
}

// scrapeTimeout bounds one scrape (ADR-0098 D6: 2 s, above the worst stall).
const scrapeTimeout = 2 * time.Second

// scrapeGateway reads g's metrics from its admin port.
func scrapeGateway(ctx context.Context, client *http.Client, g gateway) scrape {
	m := scrape{sent: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.admin+"/metrics", nil) //nolint:gosec // G704: the demo gateways' admin ports in the compose network are the point
	if err != nil {
		m.recv = time.Now()
		return m
	}
	resp, err := client.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		m.recv = time.Now()
		return m
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	_ = resp.Body.Close()
	m.recv = time.Now()
	if err != nil || resp.StatusCode != http.StatusOK {
		return m
	}
	m.ok = true
	m.samples = parseMetrics(string(body))
	return m
}

// parseMetrics parses Prometheus text exposition: each sample's name,
// labels and value; comments and malformed lines are skipped.
func parseMetrics(body string) []sample {
	var out []sample
	for l := range strings.Lines(body) {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' {
			continue
		}
		cut := strings.LastIndexByte(l, '}')
		head, rest := l, ""
		if cut >= 0 {
			head, rest = l[:cut+1], strings.TrimSpace(l[cut+1:])
		} else if i := strings.IndexByte(l, ' '); i > 0 {
			head, rest = l[:i], strings.TrimSpace(l[i+1:])
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		s := sample{labels: map[string]string{}, v: v}
		name, lbls, _ := strings.Cut(head, "{")
		s.name = name
		for kv := range strings.SplitSeq(strings.TrimSuffix(lbls, "}"), `",`) {
			if k, val, ok := strings.Cut(kv, `="`); ok {
				s.labels[strings.TrimSpace(strings.TrimPrefix(k, ","))] = strings.TrimSuffix(val, `"`)
			}
		}
		out = append(out, s)
	}
	return out
}

// sum is the sum of name's samples whose labels include the pairs of
// match.
func (m scrape) sum(name string, match ...string) float64 {
	total := 0.0
	for _, s := range m.samples {
		if s.name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(match); i += 2 {
			ok = ok && s.labels[match[i]] == match[i+1]
		}
		if ok {
			total += s.v
		}
	}
	return total
}

// circuit is the orders upstream's breaker state: 0 closed, 1 open, 2
// half-open; -1 without a reading.
func (m scrape) circuit() float64 {
	if !m.ok {
		return -1
	}
	return m.sum("gw_circuit_state", "upstream", "orders")
}

// scrapeAll reads every gateway's metrics at once.
func scrapeAll(ctx context.Context, client *http.Client, gws []gateway) []scrape {
	out := make([]scrape, len(gws))
	done := make(chan struct{})
	for i, g := range gws {
		go func() {
			out[i] = scrapeGateway(ctx, client, g)
			done <- struct{}{}
		}()
	}
	for range gws {
		<-done
	}
	return out
}
