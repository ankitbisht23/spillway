// Package config loads Spillway's config (CP-21) and compiles it into
// immutable snapshots that are swapped atomically (GW-21).
package config

import (
	"encoding/json"
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// Load parses a YAML config: YAML → JSON → proto through protojson, so YAML
// files, the admin API and stored snapshots share one schema (CP-21). Unknown
// fields are errors.
func Load(data []byte) (*configpb.Config, error) {
	c := &configpb.Config{}
	if err := load(data, c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// LoadIdentity parses a YAML identity file the way Load parses a config
// (GW-17, ADR-0017).
func LoadIdentity(data []byte) (*configpb.Identity, error) {
	id := &configpb.Identity{}
	if err := load(data, id); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	return id, nil
}

func load(data []byte, m proto.Message) error {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc == nil {
		return errors.New("empty")
	}
	j, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("not representable as JSON: %w", err)
	}
	return protojson.Unmarshal(j, m)
}
