package store

import (
	"context"

	"github.com/ankitbisht23/spillway/internal/controlplane/store/db"
)

// RecordDemoFault writes the audit record of a demo fault switch (SC-03,
// ADR-0095 §5): action demo_fault.update, entity demo_fault, the fault's
// name as the entity ID, and its state before and after. The fault lives
// in toxiproxy, which is not transactional, so the caller writes the record
// once toxiproxy has confirmed the change.
func (s *Store) RecordDemoFault(ctx context.Context, a Audit, name string, before, after bool, note *string) error {
	return a.write(ctx, db.New(s.pool), change{action: "demo_fault.update", entity: "demo_fault", name: name, note: note,
		diff: map[string]any{"before": map[string]bool{"active": before}, "after": map[string]bool{"active": after}}})
}

// RecordPlaygroundRun writes the audit record of a Playground run's start
// (verb create) or stop (verb delete) (UI-07, ADR-0096 §3): entity
// playground_run, the run's ID as the entity ID, and diff, its shape as
// {"after": …} or {"before": …}. A run lives in the control plane's memory,
// so the caller writes the record once the run started or stopped.
func (s *Store) RecordPlaygroundRun(ctx context.Context, a Audit, verb, id string, diff any) error {
	return a.write(ctx, db.New(s.pool), change{action: "playground_run." + verb, entity: "playground_run", name: id, diff: diff})
}
