// Package demo is the demo's client code (SC-01, ADR-0094): the seed, which
// sets up the demo database through the control plane's REST API, and the
// toxiproxy proxies of the demo gateways' Redis and upstream paths.
// cmd/demo-traffic runs both.
package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ankitbisht23/spillway/internal/secret"
)

// tokenMaxAge: a token older than this is replaced before a call; Keycloak's
// tokens live 5 minutes.
const tokenMaxAge = time.Minute

// Client calls the control plane's REST API with a service token from a
// client-credentials grant. Neither the client secret nor the token ever
// appears in an error.
type Client struct {
	base, tokenURL, clientID string
	secret                   secret.Value
	http                     *http.Client
	timeout                  time.Duration

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

// NewClient returns a client of the REST API at base (/api/v1 included)
// that gets its tokens for clientID with sec from tokenURL; timeout bounds
// each call.
func NewClient(base, tokenURL, clientID string, sec secret.Value, timeout time.Duration) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), tokenURL: tokenURL, clientID: clientID, secret: sec, timeout: timeout,
		http: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}}
}

// bearer returns a service token no older than tokenMaxAge.
func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenAt) < tokenMaxAge {
		return c.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.clientID}, "client_secret": {c.secret.Reveal()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode())) //nolint:gosec // G704: Keycloak in the compose network is the point
	if err != nil {
		return "", fmt.Errorf("a token for %s: %w", c.clientID, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return "", unreachable(fmt.Sprintf("a token for %s: transport error or timeout", c.clientID))
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.AccessToken == "" {
		return "", unreachable(fmt.Sprintf("a token for %s: status %d %s", c.clientID, resp.StatusCode, out.Error))
	}
	c.token, c.tokenAt = out.AccessToken, time.Now()
	return c.token, nil
}

// Answer is a REST answer.
type Answer struct {
	Status int
	Header http.Header
	Body   []byte
}

// Call sends method path with body: nil for none, []byte as it is (its
// Content-Type in header), anything else as JSON. header is name, value
// pairs.
func (c *Client) Call(ctx context.Context, method, path string, body any, header ...string) (Answer, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return Answer{}, err
	}
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return Answer{}, err
		}
		rd, ctype = bytes.NewReader(j), "application/json"
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd) //nolint:gosec // G704: the compose control plane is the point
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.http.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return Answer{}, unreachable(fmt.Sprintf("%s %s: transport error or timeout", method, path))
	}
	defer func() { _ = resp.Body.Close() }()
	a := Answer{Status: resp.StatusCode, Header: resp.Header}
	if a.Body, err = io.ReadAll(io.LimitReader(resp.Body, 16<<20)); err != nil {
		return a, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	return a, nil
}

// Must is Call, with any status but want an error that quotes the answer's
// error message, and the answer decoded into out unless nil.
func (c *Client) Must(ctx context.Context, want int, method, path string, body, out any, header ...string) (Answer, error) {
	a, err := c.Call(ctx, method, path, body, header...)
	if err != nil {
		return a, err
	}
	if a.Status != want {
		return a, &StatusError{Status: a.Status, msg: fmt.Sprintf("%s %s: status %d, want %d: %s", method, path, a.Status, want, ErrorMessage(a.Body))}
	}
	if out != nil {
		if err := json.Unmarshal(a.Body, out); err != nil {
			return a, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return a, nil
}

// StatusError is an answer with a status Must did not want.
type StatusError struct {
	Status int
	msg    string
}

func (e *StatusError) Error() string { return e.msg }

// unreachable is a token or transport error.
type unreachable string

func (e unreachable) Error() string { return string(e) }

// NotReady reports whether err means the control plane cannot answer yet: a
// token or transport error, 401 (its signing keys not fetched yet,
// ADR-0039) or a 502, 503 or 504.
func NotReady(err error) bool {
	var u unreachable
	if errors.As(err, &u) {
		return true
	}
	var se *StatusError
	return errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusBadGateway || se.Status == http.StatusServiceUnavailable || se.Status == http.StatusGatewayTimeout)
}

// ErrorMessage is an error answer's message and up to five details.
func ErrorMessage(body []byte) string {
	var e struct {
		Error   string `json:"error"`
		Details []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return "(no error message)"
	}
	msg := e.Error
	for i, d := range e.Details {
		if i == 5 {
			msg += fmt.Sprintf("; and %d more", len(e.Details)-i)
			break
		}
		msg += fmt.Sprintf("; %s: %s", d.Path, d.Message)
	}
	return msg
}

// etag is the If-Match value of an entity at version.
func etag(version int64) string { return strconv.Quote(strconv.FormatInt(version, 10)) }
