package api

import (
	"context"
	"fmt"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Validate, publish, versions and rollback (CP-02 - CP-04, SEC-06,
// ADR-0037).

// ValidateConfig validates the working set and previews it against the
// latest published version; its ETag is the revision publish needs.
func (s *Server) ValidateConfig(ctx context.Context, _ apiv1.ValidateConfigRequestObject) (apiv1.ValidateConfigResponseObject, error) {
	ws, err := s.store.ReadWorkingSet(ctx)
	if err != nil {
		return nil, err
	}
	errs := s.validateConfig(ctx, ws.Config, ws.Refs)
	out := apiv1.ConfigValidation{WorkingSetRevision: ws.Revision, Valid: len(errs) == 0, Errors: errs}
	if out.Errors == nil {
		out.Errors = []apiv1.ErrorDetail{}
	}
	var base *configpb.Config
	from := "no published version"
	if ws.Base != nil {
		out.BaseVersion, base, from = &ws.Base.Version, ws.Base.Config, fmt.Sprintf("version %d", ws.Base.Version)
	}
	out.Changes = configChanges(base, ws.Config)
	if out.Diff, err = configDiff(base, ws.Config, from, fmt.Sprintf("working set (revision %d)", ws.Revision)); err != nil {
		return nil, err
	}
	return apiv1.ValidateConfig200JSONResponse{Body: out, Headers: apiv1.ValidateConfig200ResponseHeaders{ETag: etag(ws.Revision)}}, nil
}

// check is the store's Check: CP-03's validation, then the changes for the
// audit record.
func (s *Server) check(ctx context.Context, c *configpb.Config, base *store.Published, refs []store.IdentityRef) (any, error) {
	if errs := s.validateConfig(ctx, c, refs); len(errs) > 0 {
		return nil, validationFailed(errs)
	}
	var b *configpb.Config
	if base != nil {
		b = base.Config
	}
	return configChanges(b, c), nil
}

// PublishConfig publishes the working set as a new config version (CP-02).
func (s *Server) PublishConfig(ctx context.Context, req apiv1.PublishConfigRequestObject) (apiv1.PublishConfigResponseObject, error) {
	note, err := changeNote(req.Body)
	if err != nil {
		return nil, err
	}
	v, err := s.store.Publish(ctx, audit(ctx, nil), ifMatch(req.Params.IfMatch), note, s.check)
	if err != nil {
		return nil, err
	}
	return apiv1.PublishConfig200JSONResponse(versionOf(v)), nil
}

// RollbackConfig publishes a copy of an earlier version (CP-04, D5).
func (s *Server) RollbackConfig(ctx context.Context, req apiv1.RollbackConfigRequestObject) (apiv1.RollbackConfigResponseObject, error) {
	note, err := changeNote(req.Body)
	if err != nil {
		return nil, err
	}
	v, err := s.store.Rollback(ctx, audit(ctx, nil), req.V, ifMatch(req.Params.IfMatch), note, s.check)
	if err != nil {
		return nil, err
	}
	return apiv1.RollbackConfig200JSONResponse(versionOf(v)), nil
}

// maxNote is the longest change note, in characters (CP-02).
const maxNote = 2000

func changeNote(body *apiv1.ChangeNote) (string, error) {
	if body == nil {
		return "", invalid("body: required")
	}
	if n := utf8.RuneCountInString(body.Note); n < 1 || n > maxNote {
		return "", validation("config", "note", fmt.Sprintf("note: a change note of 1 to %d characters is required", maxNote))
	}
	return body.Note, nil
}

// ListConfigVersions lists published versions, newest first; the cursor
// is the last version of the page.
func (s *Server) ListConfigVersions(ctx context.Context, req apiv1.ListConfigVersionsRequestObject) (apiv1.ListConfigVersionsResponseObject, error) {
	p, err := page(req.Params.Limit, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListVersions(ctx, p.After, p.Limit)
	if err != nil {
		return nil, err
	}
	rows, next := pageOf(rows, p, func(v store.ConfigVersion) int64 { return v.Version })
	items := make([]apiv1.ConfigVersion, len(rows))
	for i, v := range rows {
		items[i] = versionOf(v)
	}
	return apiv1.ListConfigVersions200JSONResponse{Items: items, NextCursor: next}, nil
}

// GetConfigVersion reads one published version with its snapshot.
func (s *Server) GetConfigVersion(ctx context.Context, req apiv1.GetConfigVersionRequestObject) (apiv1.GetConfigVersionResponseObject, error) {
	v, err := s.store.GetVersion(ctx, req.V)
	if err != nil {
		return nil, err
	}
	c := &configpb.Config{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(v.Snapshot, c); err != nil {
		return nil, fmt.Errorf("config version %d: %w", v.Version, err)
	}
	out := versionOf(v)
	snapshot := jsonFields(c.ProtoReflect())
	out.Snapshot = &snapshot
	return apiv1.GetConfigVersion200JSONResponse(out), nil
}

// DiffConfigVersion is the changes from version against (default v-1; 0
// is the empty config) to version v.
func (s *Server) DiffConfigVersion(ctx context.Context, req apiv1.DiffConfigVersionRequestObject) (apiv1.DiffConfigVersionResponseObject, error) {
	against := req.V - 1
	if req.Params.Against != nil {
		against = *req.Params.Against
	}
	if against < 0 {
		return nil, invalid("against: want a version, or 0 for the empty config")
	}
	to, err := s.store.GetPublished(ctx, req.V)
	if err != nil {
		return nil, err
	}
	var from *configpb.Config
	if against > 0 {
		p, err := s.store.GetPublished(ctx, against)
		if err != nil {
			return nil, err
		}
		from = p.Config
	}
	diff, err := configDiff(from, to.Config, fmt.Sprintf("version %d", against), fmt.Sprintf("version %d", req.V))
	if err != nil {
		return nil, err
	}
	return apiv1.DiffConfigVersion200JSONResponse{FromVersion: against, ToVersion: req.V, Changes: configChanges(from, to.Config), Diff: diff}, nil
}

// versionOf is a config version's REST form, without its snapshot.
func versionOf(v store.ConfigVersion) apiv1.ConfigVersion {
	return apiv1.ConfigVersion{
		Version: v.Version, PublishedAt: v.PublishedAt.UTC(), PublishedBy: v.PublishedBy, Note: v.Note,
		Kind: apiv1.ConfigVersionKind(v.Kind), RollbackOf: v.RollbackOf, WorkingSetRevision: v.WorkingSetRevision,
		SchemaVersion: apiv1.SchemaVersion{Major: v.SchemaMajor, Minor: v.SchemaMinor},
	}
}
