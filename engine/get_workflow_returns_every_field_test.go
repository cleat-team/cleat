package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// cleat#1105. Four fields of WorkflowInstance were wrong or absent on
// GetWorkflowByID, found separately and all by accident in one day:
//
//	completed_at        selected, scanned into a local, never assigned (#1091)
//	started_at          the column did not exist                        (#1090)
//	parent_workflow_id  written on every child, never selected          (#1103)
//	created_at          never selected -- reported as year one          (this)
//
// Four findings in one function is not four bugs, it is one missing check.
// CLAUDE.md: a backlog of similar findings is usually one missing abstraction.
//
// This is that check, and it is driven by the STRUCT rather than by a list of
// column names, so a field added later is covered without anyone remembering to
// extend anything.
//
// # The fixture has to be hostile
//
// The row is deliberately non-zero in every column, because zero is a
// legitimate value for most of these on an ordinary run and a default-shaped
// fixture would make the guard vacuous. If a field comes back zero here, the
// read path lost it.
//
// # Exemptions carry an argument, not a name
//
// An exemption list that grows without one is exactly what produced this class:
// each entry below says why the field cannot be populated from
// workflow_instances, and a field that is merely inconvenient does not qualify.
func TestGetWorkflowByIDReturnsEveryFieldTheRowCanHold(t *testing.T) {
	// Fields that are NOT columns of workflow_instances. Each is a fact about
	// the schema, checkable against migrations/postgres/001_schema.sql.
	exempt := map[string]string{
		"min_version": "a column of workflow_defs, not of workflow_instances -- it " +
			"describes the DEFINITION's compatibility floor and is populated on the " +
			"paths that load a definition",
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"in":1}`), "everyfield", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			makeEveryColumnNonZero(t, store, id)

			wf, err := store.GetWorkflowByID(ctx, id)
			if err != nil || wf == nil {
				t.Fatalf("GetWorkflowByID: %v (nil=%v)", err, wf == nil)
			}

			v := reflect.ValueOf(*wf)
			ty := v.Type()
			checked := 0
			for i := 0; i < ty.NumField(); i++ {
				name := strings.Split(ty.Field(i).Tag.Get("json"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				if why, ok := exempt[name]; ok {
					t.Logf("exempt: %s -- %s", name, why)
					continue
				}
				checked++
				if v.Field(i).IsZero() {
					t.Errorf("GetWorkflowByID left %s at its zero value.\n\n"+
						"Every column of this row was set to a non-zero value before the "+
						"fetch, so a zero here means the read path does not carry the "+
						"field: it is missing from the SELECT, or scanned into a local "+
						"and never assigned. Both have happened -- cleat#1091, #1103, "+
						"#1105.\n\nIf the field genuinely cannot come from "+
						"workflow_instances, add it to `exempt` WITH THE REASON; an "+
						"exemption without an argument is how this class started.", name)
				}
			}

			// A guard that checks nothing passes. Measured 2026-09-09: 22 fields
			// carry a json tag and one is exempt.
			if checked < 15 {
				t.Fatalf("only %d fields were checked; the struct has shrunk or the "+
					"reflection stopped seeing tags. That is a broken guard, not a "+
					"clean run.", checked)
			}
		})
	}
}

// makeEveryColumnNonZero sets every column GetWorkflowByID could read to a
// distinctly non-zero value, so that a zero in the result means the read lost
// it rather than the row never having had it.
func makeEveryColumnNonZero(t *testing.T, store WorkflowStore, id string) {
	t.Helper()
	db := rawDBOf(t, store)

	var ph string
	switch store.(type) {
	case *PostgresStore:
		ph = "$1"
	case *MySQLStore:
		ph = "?"
	case *MSSQLStore:
		ph = "@p1"
	default:
		t.Fatalf("makeEveryColumnNonZero: unsupported store type %T", store)
	}

	// Literals rather than bound parameters: the values are fixed and
	// test-controlled, and binding twenty of them across three placeholder
	// dialects buys nothing here.
	stmt := `UPDATE workflow_instances SET ` + strings.Join([]string{
		`status = 'done'`,
		`assigned_to = 'worker-everyfield'`,
		`result = '{"r":1}'`,
		`error_msg = 'an error'`,
		`error_code = 'E_TEST'`,
		`error_op = 'an_op'`,
		`trace_id = 'trace-everyfield'`,
		`priority = 7`,
		`generation = 3`,
		`reclaim_count = 5`,
		`pending_terminal_status = 'failed'`,
		`continued_from = 'wf-previous'`,
		`parent_workflow_id = 'wf-parent'`,
		`completed_at = ` + nowLiteral(store),
		`started_at = ` + nowLiteral(store),
		`next_wake_at = ` + nowLiteral(store),
	}, ", ") + ` WHERE id = ` + ph

	if _, err := db.Exec(stmt, id); err != nil {
		t.Fatalf("populating every column: %v", err)
	}
}

func nowLiteral(store WorkflowStore) string {
	switch store.(type) {
	case *MySQLStore:
		return "NOW(6)"
	case *MSSQLStore:
		return "SYSUTCDATETIME()"
	default:
		return "now()"
	}
}
