package store

import (
	"context"
	"encoding/json"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// The config settings: a config file's ratelimit section, kept as one row
// of the config working set (ADR-0045).

// Settings is the config settings row; its Spec is the RateLimitSettings
// message's protojson.
type Settings = db.ConfigSetting

// GetSettings returns the config settings.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	return db.New(s.pool).GetSettings(ctx)
}

// UpdateSettings replaces the config settings with spec, if they are at
// version ifMatch; like every config edit it bumps the working-set revision,
// which becomes their version, and writes its audit record (ADR-0037,
// ADR-0045).
func (s *Store) UpdateSettings(ctx context.Context, a Audit, ifMatch int64, spec []byte) (Settings, error) {
	var out Settings
	err := s.inConfigTx(ctx, func(q *db.Queries, rev int64) error {
		cur, err := q.GetSettings(ctx)
		if err != nil {
			return err
		}
		if err := checkVersion(cur.Version, ifMatch); err != nil {
			return err
		}
		if out, err = q.UpdateSettings(ctx, db.UpdateSettingsParams{Spec: spec, Version: rev}); err != nil {
			return err
		}
		return a.write(ctx, q, change{action: "settings.update", entity: "settings", id: 1, before: cur, after: out})
	})
	return out, err
}

// ParseSettings parses a settings row's spec; empty settings are nil, as a
// config without a ratelimit section has none.
func ParseSettings(spec []byte) (*configpb.RateLimitSettings, error) {
	r := &configpb.RateLimitSettings{}
	if err := protojson.Unmarshal(spec, r); err != nil {
		return nil, err
	}
	if proto.Size(r) == 0 {
		return nil, nil
	}
	return r, nil
}

// settingsSpec is r as a settings row's spec; nil is empty settings.
func settingsSpec(r *configpb.RateLimitSettings) ([]byte, error) {
	if r == nil {
		return []byte("{}"), nil
	}
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(r)
}

// writeSettings sets the settings to r at revision rev if they differ from
// cur, the working set's; an import and a rollback write them this way.
func writeSettings(ctx context.Context, q *db.Queries, cur, r *configpb.RateLimitSettings, rev int64) error {
	if proto.Equal(cur, r) {
		return nil
	}
	spec, err := settingsSpec(r)
	if err != nil {
		return err
	}
	_, err = q.UpdateSettings(ctx, db.UpdateSettingsParams{Spec: spec, Version: rev})
	return err
}

// settingsOf is c's ratelimit section, nil when empty.
func settingsOf(c *configpb.Config) *configpb.RateLimitSettings {
	if r := c.GetRatelimit(); proto.Size(r) > 0 {
		return r
	}
	return nil
}

// settingsJSON is r as an audit record holds it.
func settingsJSON(r *configpb.RateLimitSettings) json.RawMessage {
	spec, err := settingsSpec(r)
	if err != nil {
		return json.RawMessage("{}")
	}
	return spec
}
