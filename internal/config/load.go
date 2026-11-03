// Package config loads Spillway's config (CP-21) and compiles it into
// immutable snapshots that are swapped atomically (GW-21).
package config

import (
	"encoding/json"
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Load parses a YAML config: YAML → JSON → proto through protojson, so YAML
// files, the admin API and stored snapshots share one schema (CP-21). Unknown
// fields are errors.
func Load(data []byte) (*configpb.Config, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if doc == nil {
		return nil, errors.New("config: empty")
	}
	j, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("config: not representable as JSON: %w", err)
	}
	c := &configpb.Config{}
	if err := protojson.Unmarshal(j, c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}
