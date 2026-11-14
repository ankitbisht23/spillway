package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// YAML import and export of the config working set (CP-05, ADR-0037,
// ADR-0044).

// MaxImportBody is the largest import body (ADR-0036).
const MaxImportBody = 10 << 20

// importOperation is the one operation whose body is YAML.
const importOperation = "POST " + BasePath + "/config/import"

// ExportConfig writes the working set, or a published version, as the YAML
// static mode reads. The working set's ETag is its revision, which an
// import's If-Match carries.
func (s *Server) ExportConfig(ctx context.Context, req apiv1.ExportConfigRequestObject) (apiv1.ExportConfigResponseObject, error) {
	var c *configpb.Config
	var tag *string
	if v := req.Params.Version; v != nil {
		p, err := s.store.GetPublished(ctx, *v)
		if err != nil {
			return nil, err
		}
		c = p.Config
	} else {
		ws, err := s.store.ReadWorkingSet(ctx)
		if err != nil {
			return nil, err
		}
		c, tag = ws.Config, etag(ws.Revision)
	}
	b, err := exportYAML(c)
	if err != nil {
		return nil, err
	}
	return apiv1.ExportConfig200ApplicationyamlResponse{Body: bytes.NewReader(b), ContentLength: int64(len(b)), Headers: apiv1.ExportConfig200ResponseHeaders{ETag: tag}}, nil
}

// ImportConfig replaces the working set from YAML in export's shape, if it
// is at the revision If-Match names. Each entity's rules and SEC-06 are
// checked first; references between entities wait for validate and publish
// (ADR-0037).
func (s *Server) ImportConfig(ctx context.Context, req apiv1.ImportConfigRequestObject) (apiv1.ImportConfigResponseObject, error) {
	note := req.Params.Note
	if note != nil {
		if n := utf8.RuneCountInString(*note); n < 1 || n > maxNote {
			return nil, invalid(fmt.Sprintf("note: want 1 to %d characters", maxNote))
		}
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, invalid("the body could not be read")
	}
	c, ignored, err := parseImport(data)
	if err != nil {
		return nil, err
	}
	if errs := s.checkImport(ctx, c); len(errs) > 0 {
		return nil, validationFailed(errs)
	}
	var extra map[string]any
	if len(ignored) > 0 {
		extra = map[string]any{"ignored": ignored}
	}
	changes := []apiv1.ConfigChange{}
	rev, err := s.store.ImportConfig(ctx, audit(ctx, nil), ifMatch(req.Params.IfMatch), c, note, extra, func(before, after *configpb.Config) (any, bool) {
		changes = configChanges(before, after)
		return changes, len(changes) > 0
	})
	if err != nil {
		return nil, err
	}
	return apiv1.ImportConfig200JSONResponse{
		Body:    apiv1.ConfigImport{WorkingSetRevision: rev, Changes: changes, Ignored: ignored},
		Headers: apiv1.ImportConfig200ResponseHeaders{ETag: etag(rev)},
	}, nil
}

// identitySections are identity, which a config import refuses: identity
// has no working set and changes one audited change at a time (ADR-0037).
var identitySections = []string{"identity", "tenants", "consumers", "api_keys", "access_lists", "overrides"}

// ignoredSections are sections of a static config file that the control
// plane does not keep, with the reason an import gives (ADR-0044).
var ignoredSections = map[string]string{
	"ratelimit": "ratelimit: not kept: the control plane's config has no ratelimit section, and managed gateways take their replica count from heartbeats (RL-24, ADR-0044)",
}

// parseImport parses a YAML config the way the gateway's loader does, YAML
// → JSON → protojson (CP-21), one entity at a time so that an error names
// the entity. A top-level version is dropped, ignoredSections are dropped
// and returned, identity is a 422.
func parseImport(data []byte) (*configpb.Config, []string, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, invalid("body: not YAML: " + strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if doc == nil {
		return nil, nil, invalid("body: required: a YAML config, or {} for an empty one")
	}
	sections, ok := doc.(map[string]any)
	if !ok {
		return nil, nil, invalid("body: want a YAML mapping of config sections, such as upstreams and routes")
	}
	var errs []apiv1.ErrorDetail
	for _, k := range identitySections {
		if _, ok := sections[k]; ok {
			errs = append(errs, detail("identity", 0, "", k, k+": identity cannot be imported; it changes through the identity API, one audited change at a time (ADR-0037)"))
		}
	}
	if len(errs) > 0 {
		return nil, nil, validationFailed(errs)
	}
	ignored := []string{}
	for _, k := range slices.Sorted(maps.Keys(ignoredSections)) {
		if _, ok := sections[k]; ok {
			ignored = append(ignored, ignoredSections[k])
			delete(sections, k)
		}
	}
	delete(sections, "version")

	c := &configpb.Config{}
	r := c.ProtoReflect()
	for _, k := range slices.Sorted(maps.Keys(sections)) {
		fd := r.Descriptor().Fields().ByName(protoreflect.Name(k))
		if fd == nil || !fd.IsList() || fd.Message() == nil {
			return nil, nil, invalid(fmt.Sprintf("body: unknown field %q", k))
		}
		items, ok := sections[k].([]any)
		if !ok && sections[k] != nil {
			return nil, nil, invalid(fmt.Sprintf("body: %s: want a list", k))
		}
		list := r.Mutable(fd).List()
		for i, item := range items {
			at := fmt.Sprintf("%s[%d]", k, i)
			if m, ok := item.(map[string]any); ok {
				if name, ok := m["name"].(string); ok {
					at += fmt.Sprintf(" (%q)", name)
				}
			}
			j, err := json.Marshal(item)
			if err != nil {
				return nil, nil, invalid(fmt.Sprintf("body: %s: not representable as JSON", at))
			}
			e := list.NewElement()
			if err := protojson.Unmarshal(j, e.Message().Interface()); err != nil {
				return nil, nil, invalid(fmt.Sprintf("body: %s: %s", at, protoMessage(err)))
			}
			list.Append(e)
		}
	}
	return c, ignored, nil
}

// protoPrefix is how protojson starts its errors: "proto:", a space that
// is sometimes a no-break space, and a position in the JSON it was given,
// which an import's sender never saw.
var protoPrefix = regexp.MustCompile(`^proto:[\s\x{00a0}]*(\(line \d+:\d+\):[\s\x{00a0}]*)?`)

func protoMessage(err error) string {
	return protoPrefix.ReplaceAllString(err.Error(), "")
}

// checkImport checks every entity of an import as a create would (ADR-0037):
// its name, its entity rules and SEC-06; and that names and IDs are unique
// within each type. It returns every error.
func (s *Server) checkImport(ctx context.Context, c *configpb.Config) []apiv1.ErrorDetail {
	var errs []apiv1.ErrorDetail
	for _, kl := range []struct {
		k    configKind
		list []proto.Message
	}{
		{upstreams, protos(c.GetUpstreams())}, {routes, protos(c.GetRoutes())},
		{policies, protos(c.GetPolicies())}, {plans, protos(c.GetPlans())},
	} {
		names, ids := map[string]bool{}, map[int64]bool{}
		for i, m := range kl.list {
			r := m.ProtoReflect()
			name := r.Get(r.Descriptor().Fields().ByName("name")).String()
			id := r.Get(r.Descriptor().Fields().ByName("id")).Int()
			at := fmt.Sprintf("%s[%d]", kl.k.collection, i)
			switch {
			case !config.ValidName(name):
				errs = append(errs, detail(kl.k.entity, id, name, at+".name", nameRule))
			case names[name]:
				errs = append(errs, detail(kl.k.entity, id, name, at+".name", fmt.Sprintf("name %q is given twice", name)))
			}
			names[name] = true
			switch {
			case id < 0:
				errs = append(errs, detail(kl.k.entity, id, name, at+".id", fmt.Sprintf("id %d: ids are positive", id)))
			case id > 0 && ids[id]:
				errs = append(errs, detail(kl.k.entity, id, name, at+".id", fmt.Sprintf("id %d is given twice", id)))
			}
			ids[id] = true
			if err := kl.k.check(m); err != nil {
				errs = append(errs, detail(kl.k.entity, id, name, at, err.Error()))
			}
		}
	}
	for _, b := range s.blockedEndpoints(ctx, c) {
		errs = append(errs, detail("upstream", b.upstream.GetId(), b.upstream.GetName(), "endpoints", b.err.Error()))
	}
	return errs
}

func protos[M proto.Message](l []M) []proto.Message {
	out := make([]proto.Message, len(l))
	for i, m := range l {
		out[i] = m
	}
	return out
}

// detail is one error of a 422.
func detail(entity string, id int64, name, path, msg string) apiv1.ErrorDetail {
	d := apiv1.ErrorDetail{Entity: &entity, Message: &msg}
	if id != 0 {
		d.Id = &id
	}
	if name != "" {
		d.Name = &name
	}
	if path != "" {
		d.Path = &path
	}
	return d
}
