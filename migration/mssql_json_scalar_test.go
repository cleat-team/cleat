package migration_test

// package migration_test (external), not migration: see runner_test.go's
// file header for why -- engine/testutil now depends on this package.

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/migration"
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

	if err := runMigrations(t, ctx,
		migration.NewRunner(db, migration.DialectMSSQL, migrationsRoot(t)),
		migration.DialectMSSQL); err != nil {
		t.Fatalf("apply the shipped SQL Server migrations: %v", err)
	}

	// A workflow to hang the rows off: both tables have a foreign key to it.
	//
	// One pinned connection, tenant_id stamped into the session before each
	// write. cleat#2205 (migration 103) added an AFTER INSERT / AFTER UPDATE
	// block predicate to every table this file writes, bound to the same
	// dbo.fn_tenant_filter the FILTER predicates already used; unlike FILTER,
	// BLOCK checks SESSION_CONTEXT on a write regardless of the tenant_id
	// value the statement itself supplies, so a plain db.ExecContext insert
	// (no session context at all) is refused outright. These rows have no
	// tenant_id column in the INSERT and so take the schema default
	// (DefaultTenantUUID); the session context must match it or the write is
	// refused as a mismatch rather than merely as "unset". See
	// engine/flush_dialect_test.go's identical fix for why this has to be
	// sp_set_session_context on one *sql.Conn rather than db.ExecContext,
	// which may hand each call to a different pooled connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection to seed workflow_defs/workflow_instances: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=N'`+engine.DefaultTenantUUID+`'`,
	); err != nil {
		t.Fatalf("set the tenant session context: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, task_queue)
		VALUES ('json-scalar-def', 1, 0x00, '[]', 'default')`); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	const wfID = "json-scalar-wf"
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES (@p1, 'json-scalar-def', 1, 'ready', '{}', 'default')`, wfID); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	for _, tc := range []struct {
		name    string
		payload string
		accept  bool
	}{
		// What encodeJSONPayload produces for a non-JSON payload, and the
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
			// Same pinned conn as the seed above, not db.ExecContext:
			// workflow_signals is also in migration 103's covered table set,
			// and neither insert names a tenant_id column, so it takes the
			// same schema default the seed rows above rely on -- the session
			// context has to be the SAME value on the SAME connection, or
			// the block predicate refuses these too.
			_, err := conn.ExecContext(ctx, `
				INSERT INTO workflow_signals (workflow_id, signal_name, payload)
				VALUES (@p1, @p2, @p3)`, wfID, "sig-"+tc.name, tc.payload)
			switch {
			case tc.accept && err != nil:
				t.Errorf("workflow_signals.payload rejected %s: %v", tc.payload, err)
			case !tc.accept && err == nil:
				t.Errorf("workflow_signals.payload accepted %s, which is not JSON -- "+
					"the constraint has become a no-op", tc.payload)
			}

			_, err = conn.ExecContext(ctx, `
				INSERT INTO workflow_update_requests (workflow_id, request_id, update_name, payload)
				VALUES (@p1, @p2, @p2, @p3)`, wfID, "upd-"+tc.name, tc.payload)
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
