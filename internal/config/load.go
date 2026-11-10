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
	if err := load(data, c, nil); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// LoadIdentity parses a YAML identity file the way Load parses a config
// (GW-17, ADR-0017).
func LoadIdentity(data []byte) (*configpb.Identity, error) {
	id := &configpb.Identity{}
	if err := load(data, id, keyStrings); err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	return id, nil
}

// keyStrings checks that every API key's prefix and hash is a string. YAML
// reads an unquoted all-digit value as a number, and protojson's error would
// quote it, so the error names the key by index instead (ADR-0017).
func keyStrings(doc any) error {
	m, _ := doc.(map[string]any)
	keys, _ := m["api_keys"].([]any)
	for i, k := range keys {
		k, _ := k.(map[string]any)
		for _, field := range []string{"prefix", "hash"} {
			if v, ok := k[field]; ok && v != nil {
				if _, ok := v.(string); !ok {
					return fmt.Errorf("api_keys[%d].%s: not a string; quote it", i, field)
				}
			}
		}
	}
	return nil
}

// load parses data into m, after check, if any, accepts the YAML document.
func load(data []byte, m proto.Message, check func(doc any) error) error {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc == nil {
		return errors.New("empty")
	}
	if check != nil {
		if err := check(doc); err != nil {
			return err
		}
	}
	j, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("not representable as JSON: %w", err)
	}
	return protojson.Unmarshal(j, m)
}
