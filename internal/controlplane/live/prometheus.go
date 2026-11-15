package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prometheus is a client of Prometheus's HTTP query API (ADR-0059): range
// and instant queries by POST, answers of at most 1 MiB.
type Prometheus struct {
	base   *url.URL
	client *http.Client
}

// maxAnswer bounds a Prometheus answer; the catalogue's top-10 limits keep
// real answers far below it.
const maxAnswer = 1 << 20

// ErrPrometheusUnavailable is a Prometheus that refused the connection,
// timed out or answered 5xx.
var ErrPrometheusUnavailable = errors.New("prometheus unavailable")

// errQueryFailed is any other failed query: a 4xx answer (a bad query),
// an answer that is not Prometheus's JSON, or one over maxAnswer.
var errQueryFailed = errors.New("query failed")

// ParsePrometheusURL checks a Prometheus base URL: http or https, a host,
// no user info (a password would sit in a setting that is not a secret),
// no query and no fragment.
func ParsePrometheusURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("not a URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("want an http or https URL")
	case u.Host == "":
		return nil, errors.New("want a host")
	case u.User != nil:
		return nil, errors.New("user info is not allowed")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("a query or fragment is not allowed")
	}
	return u, nil
}

// NewPrometheus returns a client of the Prometheus at base, a URL that
// ParsePrometheusURL accepted, sending with client (http.DefaultClient if
// nil). Callers bound each query with a context deadline.
func NewPrometheus(base *url.URL, client *http.Client) *Prometheus {
	if client == nil {
		client = http.DefaultClient
	}
	return &Prometheus{base: base, client: client}
}

// The data kinds of a stream result (the spec's StreamMatrix and
// StreamVector). A value that is not a finite number is nil (JSON null).

type matrix struct {
	Kind   string        `json:"kind"`
	StepS  int           `json:"step_s"`
	Series []rangeSeries `json:"series"`
}

type rangeSeries struct {
	Labels map[string]string `json:"labels"`
	Points [][2]*float64     `json:"points"`
}

type vector struct {
	Kind   string   `json:"kind"`
	Series []sample `json:"series"`
}

type sample struct {
	Labels map[string]string `json:"labels"`
	Value  *float64          `json:"value"`
}

// rangeQuery evaluates promql over r ending at end.
func (p *Prometheus) rangeQuery(ctx context.Context, r timeRange, end time.Time, promql string) (matrix, error) {
	form := url.Values{"query": {promql}, "start": {unix(end.Add(-r.span))}, "end": {unix(end)}, "step": {strconv.Itoa(int(r.step / time.Second))}}
	var data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	}
	if err := p.post(ctx, "query_range", form, &data); err != nil {
		return matrix{}, err
	}
	if data.ResultType != "matrix" {
		return matrix{}, fmt.Errorf("%w: result type %q, want matrix", errQueryFailed, data.ResultType)
	}
	out := matrix{Kind: "matrix", StepS: int(r.step / time.Second), Series: make([]rangeSeries, 0, len(data.Result))}
	for _, s := range data.Result {
		rs := rangeSeries{Labels: labels(s.Metric), Points: make([][2]*float64, 0, len(s.Values))}
		for _, v := range s.Values {
			t, val, err := point(v)
			if err != nil {
				return matrix{}, err
			}
			rs.Points = append(rs.Points, [2]*float64{&t, val})
		}
		out.Series = append(out.Series, rs)
	}
	return out, nil
}

// instantQuery evaluates promql at at.
func (p *Prometheus) instantQuery(ctx context.Context, at time.Time, promql string) (vector, error) {
	var data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	}
	if err := p.post(ctx, "query", url.Values{"query": {promql}, "time": {unix(at)}}, &data); err != nil {
		return vector{}, err
	}
	if data.ResultType != "vector" {
		return vector{}, fmt.Errorf("%w: result type %q, want vector", errQueryFailed, data.ResultType)
	}
	out := vector{Kind: "vector", Series: make([]sample, 0, len(data.Result))}
	for _, s := range data.Result {
		_, val, err := point(s.Value)
		if err != nil {
			return vector{}, err
		}
		out.Series = append(out.Series, sample{Labels: labels(s.Metric), Value: val})
	}
	return out, nil
}

// post sends one query and decodes the answer's data into data.
func (p *Prometheus) post(ctx context.Context, endpoint string, form url.Values, data any) error {
	u := p.base.JoinPath("api", "v1", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPrometheusUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("%w: reading the answer: %w", ErrPrometheusUnavailable, err)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: %s", ErrPrometheusUnavailable, resp.Status)
	}
	if len(body) > maxAnswer {
		return fmt.Errorf("%w: the answer is over %d bytes", errQueryFailed, maxAnswer)
	}
	var answer struct {
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
		ErrorType string          `json:"errorType"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return fmt.Errorf("%w: %s, not Prometheus's JSON", errQueryFailed, resp.Status)
	}
	if resp.StatusCode != http.StatusOK || answer.Status != "success" {
		return fmt.Errorf("%w: %s, %s", errQueryFailed, resp.Status, answer.ErrorType)
	}
	if err := json.Unmarshal(answer.Data, data); err != nil {
		return fmt.Errorf("%w: unexpected data", errQueryFailed)
	}
	return nil
}

func unix(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}

func labels(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// point decodes a [<unix seconds>, "<value>"] pair; a value that is not
// a finite number is nil.
func point(v [2]any) (float64, *float64, error) {
	t, ok := v[0].(float64)
	s, ok2 := v[1].(string)
	if !ok || !ok2 {
		return 0, nil, fmt.Errorf("%w: a sample is not [time, \"value\"]", errQueryFailed)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: a sample value is not a number", errQueryFailed)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return t, nil, nil
	}
	return t, &f, nil
}
