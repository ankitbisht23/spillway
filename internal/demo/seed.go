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

	"go.yaml.in/yaml/v3"

	"github.com/ankitbisht23/spillway/internal/config"
)

// The seed (SC-01, ADR-0094 §3): deploy/demo/config.yaml is imported and
// published through the REST API, then the tenants and consumers of
// deploy/demo/seed.yaml are created by name if absent, or updated if they
// differ. No API key is seeded: every key is a demo key issued at run time
// (ADR-0095 §3). A second run changes nothing.

// SeedNote is the change note of the seed's import and publish.
const SeedNote = "demo seed"

// SeedFile is deploy/demo/seed.yaml: the demo's tenants and their consumers.
type SeedFile struct {
	Tenants []SeedTenant `yaml:"tenants"`
}

// SeedTenant is a tenant on a plan of the demo config, with its consumers.
type SeedTenant struct {
	Name      string         `yaml:"name"`
	Plan      string         `yaml:"plan"`
	Consumers []SeedConsumer `yaml:"consumers"`
}

// SeedConsumer is a consumer on its tenant's plan; JWTSubject, when set, is
// the token sub that resolves to it (ADR-0049).
type SeedConsumer struct {
	Name       string `yaml:"name"`
	JWTSubject string `yaml:"jwt_subject"`
}

// ParseSeed parses a seed file strictly: unknown fields, invalid or
// duplicate names, a tenant without a plan and a JWT subject given twice
// are errors.
func ParseSeed(data []byte) (SeedFile, error) {
	var f SeedFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return f, fmt.Errorf("seed file: %w", err)
	}
	if len(f.Tenants) == 0 {
		return f, errors.New("seed file: no tenants")
	}
	tenants, subjects := map[string]bool{}, map[string]bool{}
	for i, t := range f.Tenants {
		at := fmt.Sprintf("tenants[%d]", i)
		switch {
		case !config.ValidName(t.Name):
			return f, fmt.Errorf("seed file: %s.name %q: want 1-64 characters from [A-Za-z0-9._-]", at, t.Name)
		case tenants[t.Name]:
			return f, fmt.Errorf("seed file: tenant %q is given twice", t.Name)
		case !config.ValidName(t.Plan):
			return f, fmt.Errorf("seed file: tenant %q: plan %q: want a plan's name", t.Name, t.Plan)
		}
		tenants[t.Name] = true
		consumers := map[string]bool{}
		for j, c := range t.Consumers {
			switch {
			case !config.ValidName(c.Name):
				return f, fmt.Errorf("seed file: %s.consumers[%d].name %q: want 1-64 characters from [A-Za-z0-9._-]", at, j, c.Name)
			case consumers[c.Name]:
				return f, fmt.Errorf("seed file: tenant %q: consumer %q is given twice", t.Name, c.Name)
			case c.JWTSubject != "" && subjects[c.JWTSubject]:
				return f, fmt.Errorf("seed file: jwt_subject %q is given twice", c.JWTSubject)
			}
			consumers[c.Name] = true
			if c.JWTSubject != "" {
				subjects[c.JWTSubject] = true
			}
		}
	}
	return f, nil
}

// SeedReport is what a seed run did.
type SeedReport struct {
	ConfigVersion int64 // the published version the seed left
	Published     bool  // whether this run published it
	Created       int   // tenants and consumers
	Updated       int
	Unchanged     int
}

// Seed imports configYAML and publishes it, then creates or updates the
// tenants and consumers of seed, writing one line per step to out (names,
// IDs and versions; never a token).
func Seed(ctx context.Context, c *Client, configYAML []byte, seed SeedFile, out io.Writer) (SeedReport, error) {
	var rep SeedReport
	v, published, err := importPublish(ctx, c, configYAML)
	if err != nil {
		return rep, err
	}
	rep.ConfigVersion, rep.Published = v, published
	if published {
		_, _ = fmt.Fprintf(out, "demo-seed: config version %d published\n", v)
	} else {
		_, _ = fmt.Fprintf(out, "demo-seed: config version %d is already the seed's\n", v)
	}
	plans := map[string]int64{}
	for _, t := range seed.Tenants {
		if _, ok := plans[t.Plan]; ok {
			continue
		}
		id, err := lookupID(ctx, c, "/plans", url.Values{"name": {t.Plan}})
		if err != nil {
			return rep, err
		}
		if id == 0 {
			return rep, fmt.Errorf("tenant %q: plan %q is not in the demo config", t.Name, t.Plan)
		}
		plans[t.Plan] = id
	}
	count := func(what string, r result) {
		switch r {
		case created:
			rep.Created++
		case updated:
			rep.Updated++
		default:
			rep.Unchanged++
		}
		_, _ = fmt.Fprintf(out, "demo-seed: %s: %s\n", what, r)
	}
	for _, t := range seed.Tenants {
		id, r, err := seedTenant(ctx, c, t.Name, plans[t.Plan])
		if err != nil {
			return rep, err
		}
		count(fmt.Sprintf("tenant %s (plan %s, id %d)", t.Name, t.Plan, id), r)
		for _, sc := range t.Consumers {
			cid, r, err := seedConsumer(ctx, c, id, sc)
			if err != nil {
				return rep, err
			}
			count(fmt.Sprintf("consumer %s (id %d)", sc.Name, cid), r)
		}
	}
	return rep, nil
}

// importPublish imports yaml as the working set, with If-Match on the
// exported one, then validates and publishes it. A 409 "nothing to
// publish" answers the latest version, unpublished by this run.
func importPublish(ctx context.Context, c *Client, yaml []byte) (version int64, published bool, err error) {
	a, err := c.Must(ctx, http.StatusOK, http.MethodGet, "/config/export", nil, nil)
	if err != nil {
		return 0, false, err
	}
	if _, err := c.Must(ctx, http.StatusOK, http.MethodPost, "/config/import?"+url.Values{"note": {SeedNote}}.Encode(), yaml, nil,
		"If-Match", a.Header.Get("ETag"), "Content-Type", "application/yaml"); err != nil {
		return 0, false, err
	}
	var v struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	a, err = c.Must(ctx, http.StatusOK, http.MethodPost, "/config/validate", nil, &v)
	if err != nil {
		return 0, false, err
	}
	if !v.Valid {
		msgs := make([]string, 0, len(v.Errors))
		for _, e := range v.Errors {
			msgs = append(msgs, e.Path+": "+e.Message)
		}
		return 0, false, fmt.Errorf("the demo config does not validate: %s", strings.Join(msgs, "; "))
	}
	b, err := c.Call(ctx, http.MethodPost, "/config/publish", map[string]string{"note": SeedNote}, "If-Match", a.Header.Get("ETag"))
	if err != nil {
		return 0, false, err
	}
	var p struct {
		Version int64 `json:"version"`
	}
	switch {
	case b.Status == http.StatusOK:
		if err := json.Unmarshal(b.Body, &p); err != nil {
			return 0, false, fmt.Errorf("POST /config/publish: %w", err)
		}
		return p.Version, true, nil
	case b.Status == http.StatusConflict && strings.HasPrefix(ErrorMessage(b.Body), "nothing to publish"):
		var list struct{ Items []struct{ Version int64 } }
		if _, err := c.Must(ctx, http.StatusOK, http.MethodGet, "/config/versions?limit=1", nil, &list); err != nil {
			return 0, false, err
		}
		if len(list.Items) == 0 {
			return 0, false, errors.New("publish answered nothing to publish, but no version exists")
		}
		return list.Items[0].Version, false, nil
	}
	return 0, false, fmt.Errorf("POST /config/publish: status %d: %s", b.Status, ErrorMessage(b.Body))
}

// result is what the seed did to one entity.
type result string

const (
	created   result = "created"
	updated   result = "updated"
	unchanged result = "unchanged"
)

// lookupID returns the ID of the one entity of collection that q selects, 0
// for none.
func lookupID(ctx context.Context, c *Client, collection string, q url.Values) (int64, error) {
	var list struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if _, err := c.Must(ctx, http.StatusOK, http.MethodGet, collection+"?"+q.Encode(), nil, &list); err != nil {
		return 0, err
	}
	if len(list.Items) == 0 {
		return 0, nil
	}
	return list.Items[0].ID, nil
}

// tenant is a Tenant as the API answers it.
type tenant struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	PlanID  int64  `json:"plan_id"`
	Status  string `json:"status"`
	Version int64  `json:"version"`
}

// seedTenant creates tenant name on plan, or puts an existing one back on
// plan and active.
func seedTenant(ctx context.Context, c *Client, name string, plan int64) (int64, result, error) {
	var list struct{ Items []tenant }
	if _, err := c.Must(ctx, http.StatusOK, http.MethodGet, "/tenants?"+url.Values{"name": {name}}.Encode(), nil, &list); err != nil {
		return 0, "", err
	}
	want := map[string]any{"name": name, "plan_id": plan, "status": "active"}
	var t tenant
	if len(list.Items) == 0 {
		if _, err := c.Must(ctx, http.StatusCreated, http.MethodPost, "/tenants", want, &t); err != nil {
			return 0, "", err
		}
		return t.ID, created, nil
	}
	t = list.Items[0]
	if t.PlanID == plan && t.Status == "active" {
		return t.ID, unchanged, nil
	}
	if _, err := c.Must(ctx, http.StatusOK, http.MethodPut, "/tenants/"+strconv.FormatInt(t.ID, 10), want, nil, "If-Match", etag(t.Version)); err != nil {
		return 0, "", err
	}
	return t.ID, updated, nil
}

// consumer is a Consumer as the API answers it.
type consumer struct {
	ID         int64   `json:"id"`
	TenantID   int64   `json:"tenant_id"`
	Name       string  `json:"name"`
	PlanID     *int64  `json:"plan_id"`
	Status     string  `json:"status"`
	JWTSubject *string `json:"jwt_subject"`
	Version    int64   `json:"version"`
}

// seedConsumer creates consumer sc of tenant on the tenant's plan, or puts
// an existing one back on it, active, with sc's JWT subject.
func seedConsumer(ctx context.Context, c *Client, tenant int64, sc SeedConsumer) (int64, result, error) {
	var list struct{ Items []consumer }
	q := url.Values{"tenant_id": {strconv.FormatInt(tenant, 10)}, "name": {sc.Name}}
	if _, err := c.Must(ctx, http.StatusOK, http.MethodGet, "/consumers?"+q.Encode(), nil, &list); err != nil {
		return 0, "", err
	}
	var subject *string
	if sc.JWTSubject != "" {
		subject = &sc.JWTSubject
	}
	want := map[string]any{"tenant_id": tenant, "name": sc.Name, "plan_id": nil, "status": "active", "jwt_subject": subject}
	var got consumer
	if len(list.Items) == 0 {
		if _, err := c.Must(ctx, http.StatusCreated, http.MethodPost, "/consumers", want, &got); err != nil {
			return 0, "", err
		}
		return got.ID, created, nil
	}
	got = list.Items[0]
	if got.PlanID == nil && got.Status == "active" && (got.JWTSubject == nil) == (subject == nil) && (subject == nil || *got.JWTSubject == *subject) {
		return got.ID, unchanged, nil
	}
	if _, err := c.Must(ctx, http.StatusOK, http.MethodPut, "/consumers/"+strconv.FormatInt(got.ID, 10), want, nil, "If-Match", etag(got.Version)); err != nil {
		return 0, "", err
	}
	return got.ID, updated, nil
}
