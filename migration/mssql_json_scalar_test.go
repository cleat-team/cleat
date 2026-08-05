package migration

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// TestMSSQLPayloadConstraintsAcceptJSONScalars covers IMPROVEMENT-PLAN 3.18.
//
// `ISJSON(expression)` with no second argument returns 1 only for a JSON object
// or array; a scalar returns 0. §2.60c made all three stores encode a non-JSON
// signal payload with json.Marshal, which produces the scalar `"payload-1"` --
// accepted by PostgreSQL's JSONB and MySQL's JSON, and refused by SQL Server's
// shipped CHECK constraints. DeliverSignal and CreateUpdateRequest therefore
// failed on any SQL Server built from the shipped schema.
//
// Migration 011 changes both constraints to `ISJSON(payload, VALUE) = 1`, which
// accepts the same set of values the other two dialects do. This runs the real
// Runner over the real files and then writes the exact value the store writes.
//
// The rejection half matters as much as the acceptance half: VALUE must not
// turn the constraint into a no-op, or the migration would trade a defect for
// the absence of a guard.
func TestMSSQLPayloadConstraintsAcceptJSONScalars(t *testing.T) {
	db := newMSSQLScratchDB(t, "cleat_migration_json_scalar_test")
	ctx := context.Background()

	if err := NewRunner(db, engine.DialectMSSQL, migrationsRoot(t)).Run(ctx); err != nil {
		t.Fatalf("apply the shipped SQL Server migrations: %v", err)
	}

	// A workflow to hang the rows off: both tables have a foreign key to it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, task_queue)
		VALUES ('json-scalar-def', 1, 0x00, '[]', 'default')`); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	const wfID = "json-scalar-wf"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES (@p1, 'json-scalar-def', 1, 'ready', '{}', 'default')`, wfID); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	for _, tc := range []struct {
		name    string
		payload string
		accept  bool
	}{
		// What encodeSignalPayload produces for a non-JSON payload, and the
		// value that made 2.60c only two-thirds fixed.
		{"a JSON string scalar", `"payload-1"`, true},
		{"a JSON number scalar", `123`, true},
		{"an object", `{"key":"value"}`, true},
		{"an array", `[1,2,3]`, true},
		// The guard has to keep guarding.
		{"not JSON at all", `payload-1`, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `
				INSERT INTO workflow_signals (workflow_id, signal_name, payload)
				VALUES (@p1, @p2, @p3)`, wfID, "sig-"+tc.name, tc.payload)
			switch {
			case tc.accept && err != nil:
				t.Errorf("workflow_signals.payload rejected %s: %v", tc.payload, err)
			case !tc.accept && err == nil:
				t.Errorf("workflow_signals.payload accepted %s, which is not JSON -- "+
					"the constraint has become a no-op", tc.payload)
			}

			_, err = db.ExecContext(ctx, `
				INSERT INTO workflow_update_requests (workflow_id, update_name, payload)
				VALUES (@p1, @p2, @p3)`, wfID, "upd-"+tc.name, tc.payload)
			switch {
			case tc.accept && err != nil:
				t.Errorf("workflow_update_requests.payload rejected %s: %v", tc.payload, err)
			case !tc.accept && err == nil:
				t.Errorf("workflow_update_requests.payload accepted %s, which is not JSON -- "+
					"the constraint has become a no-op", tc.payload)
			}
		})
	}
}
