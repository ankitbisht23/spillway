package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ankitbisht23/spillway/internal/controlplane/store"
	"github.com/ankitbisht23/spillway/internal/gen/configpb"
)

// A config entity's REST form is its config.proto message's fields plus the
// REST-only version, created_at and updated_at (ADR-0036). These functions
// go through JSON both ways, so the proto stays the one schema (CP-21).

// restOnly are the REST fields that are not the proto's; id is the proto's
// but is assigned by the control plane, so a client's id is ignored too.
var restOnly = []string{"id", "version", "created_at", "updated_at"}

// toProto parses a request body, the generated model of a config entity,
// into m: JSON, without the REST-only fields, through protojson.
func toProto(body any, m proto.Message) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var fields map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		return err
	}
	for _, f := range restOnly {
		delete(fields, f)
	}
	if eps, ok := fields["endpoints"].([]any); ok {
		for _, e := range eps {
			if e, ok := e.(map[string]any); ok {
				delete(e, "id")
			}
		}
	}
	if b, err = json.Marshal(fields); err != nil {
		return err
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		// protojson's messages carry its own position notes; keep the gist.
		return fmt.Errorf("body: %s", strings.TrimPrefix(err.Error(), "proto: "))
	}
	return nil
}

// specOf is m's protojson form without id, name and endpoints, which have
// columns (ADR-0036).
func specOf(m proto.Message) ([]byte, error) {
	m = proto.Clone(m)
	r := m.ProtoReflect()
	for _, f := range []protoreflect.Name{"id", "name", "endpoints"} {
		if fd := r.Descriptor().Fields().ByName(f); fd != nil {
			r.Clear(fd)
		}
	}
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
}

// fromRow fills out, the generated model of a config entity, from a stored
// row: its spec, id, name and endpoints, and the REST-only fields.
func fromRow(row store.ConfigRow, m proto.Message, out any) error {
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(row.Spec, m); err != nil {
		return fmt.Errorf("stored spec of %d: %w", row.ID, err)
	}
	r := m.ProtoReflect()
	fields := r.Descriptor().Fields()
	r.Set(fields.ByName("id"), protoreflect.ValueOfInt64(row.ID))
	r.Set(fields.ByName("name"), protoreflect.ValueOfString(row.Name))
	if u, ok := m.(*configpb.Upstream); ok {
		for _, e := range row.Endpoints {
			u.Endpoints = append(u.Endpoints, &configpb.Endpoint{Id: e.ID, Address: e.Address, Draining: e.Draining})
		}
	}
	fieldsMap := jsonFields(r)
	fieldsMap["version"] = row.Version
	fieldsMap["created_at"] = row.CreatedAt.UTC().Format(time.RFC3339Nano)
	fieldsMap["updated_at"] = row.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if _, ok := m.(*configpb.Upstream); ok && fieldsMap["endpoints"] == nil {
		fieldsMap["endpoints"] = []any{}
	}
	b, err := json.Marshal(fieldsMap)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// jsonFields is a message's populated fields by proto name, as protojson
// writes them except that 64-bit integers stay JSON numbers (ADR-0036).
func jsonFields(m protoreflect.Message) map[string]any {
	out := map[string]any{}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			l := v.List()
			items := make([]any, l.Len())
			for i := range l.Len() {
				items[i] = jsonValue(fd, l.Get(i))
			}
			out[string(fd.Name())] = items
		default:
			out[string(fd.Name())] = jsonValue(fd, v)
		}
		return true
	})
	return out
}

func jsonValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	if fd.Kind() == protoreflect.MessageKind {
		return jsonFields(v.Message())
	}
	return v.Interface() // string, bool, int32, int64, float64: config.proto has no others
}

// limitsJSON is limits in protojson form, as overrides store them
// (ADR-0036).
func limitsJSON(limits []*configpb.Limit) ([]byte, error) {
	items := make([]json.RawMessage, len(limits))
	for i, l := range limits {
		b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(l)
		if err != nil {
			return nil, err
		}
		items[i] = b
	}
	return json.Marshal(items)
}
