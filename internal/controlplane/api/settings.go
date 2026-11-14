package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ankitbisht23/spillway/internal/config"
	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/apiv1"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// The config settings: the working set's ratelimit section (ADR-0045).

func (s *Server) GetConfigSettings(ctx context.Context, _ apiv1.GetConfigSettingsRequestObject) (apiv1.GetConfigSettingsResponseObject, error) {
	st, err := s.store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	out, err := settingsOut(st)
	if err != nil {
		return nil, err
	}
	return apiv1.GetConfigSettings200JSONResponse{Body: out, Headers: apiv1.GetConfigSettings200ResponseHeaders{ETag: etag(st.Version)}}, nil
}

func (s *Server) UpdateConfigSettings(ctx context.Context, req apiv1.UpdateConfigSettingsRequestObject) (apiv1.UpdateConfigSettingsResponseObject, error) {
	r := &configpb.RateLimitSettings{}
	if err := toProto(req.Body.Ratelimit, r); err != nil {
		return nil, invalid(err.Error())
	}
	if err := config.CheckRateLimit(r); err != nil {
		return nil, validation("settings", "ratelimit", err.Error())
	}
	spec, err := store.SpecOf(r)
	if err != nil {
		return nil, err
	}
	st, err := s.store.UpdateSettings(ctx, audit(ctx, showAs(settingsOut)), ifMatch(req.Params.IfMatch), spec)
	if err != nil {
		return nil, err
	}
	out, err := settingsOut(st)
	if err != nil {
		return nil, err
	}
	return apiv1.UpdateConfigSettings200JSONResponse{Body: out, Headers: apiv1.UpdateConfigSettings200ResponseHeaders{ETag: etag(st.Version)}}, nil
}

// settingsOut is the REST form of the settings row.
func settingsOut(st store.Settings) (apiv1.ConfigSettings, error) {
	out := apiv1.ConfigSettings{Version: &st.Version, UpdatedAt: &st.UpdatedAt}
	if err := json.Unmarshal(st.Spec, &out.Ratelimit); err != nil {
		return out, fmt.Errorf("stored config settings: %w", err)
	}
	return out, nil
}
