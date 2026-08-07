package engine

// IMPROVEMENT-PLAN 3.17: completing a workflow with no query handlers wrote the
// JSON value `null` into query_state on every dialect.
//
// json.Marshal of a nil map returns the four bytes `null`, not nil, so the
// `if qsJSON == nil` guard that was supposed to substitute `{}` never fired.
// PostgreSQL's JSONB and MySQL's JSON accept it, so the row went in and the
// query state read back as null; SQL Server's shipped schema does not, and
// CompleteWorkflow, FailWorkflow and ContinueAsNew all failed there with
//
//	The UPDATE statement conflicted with the CHECK constraint
//	"ck_workflow_instances_query_state"
//
// for any workflow without query handlers -- which is most of them.
//
// The test is three-dialect rather than SQL Server-only on purpose: the *fix*
// is that every dialect stores an object, and on two of them the defect is
// invisible unless you look at the value. Asserting only that SQL Server stops
// erroring would let PostgreSQL and MySQL keep writing null forever.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestCompleteWorkflowStoresAnObjectForEmptyQueryState(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			const defName = "query-state-def"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			for _, tc := range []struct {
				name       string
				queryState map[string]string
				want       string
			}{
				{"no query handlers", nil, "{}"},
				{"empty map", map[string]string{}, "{}"},
				{"a handler", map[string]string{"status": "ok"}, `{"status":"ok"}`},
			} {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					runID, _, err := store.StartNewRun(ctx, "", defName, 1,
						json.RawMessage(`{}`),
						fmt.Sprintf("qs-%s-%d", tc.name, time.Now().UnixNano()),
						DefaultTenantUUID, 0)
					if err != nil {
						t.Fatalf("StartNewRun: %v", err)
					}
					claimed, err := store.ClaimWorkflow(ctx, "test-worker")
					if err != nil {
						t.Fatalf("ClaimWorkflow: %v", err)
					}
					if claimed == nil {
						t.Fatal("ClaimWorkflow returned nothing to complete")
					}

					// This is the call that failed outright on a SQL Server
					// built from the shipped schema.
					if err := store.CompleteWorkflow(ctx, claimed.ID, "test-worker",
						claimed.Generation, `{"done":true}`, tc.queryState); err != nil {
						t.Fatalf("CompleteWorkflow: %v\n\nThis is what a SQL Server built from "+
							"migrations/mssql/001_schema.sql does with every completion.", err)
					}

					// And the value itself has to be an object. On PostgreSQL
					// and MySQL nothing errors either way, so this assertion
					// is the only thing that can see the defect there.
					//
					// Read straight out of the column: the store's
					// GetQueryState takes a key and returns one entry, which
					// cannot distinguish an empty object from a JSON null.
					stored := readQueryStateColumn(t, backend, claimed.ID)
					var normalised map[string]string
					if err := json.Unmarshal([]byte(stored), &normalised); err != nil {
						t.Fatalf("query state stored as %q, which is not a JSON object: %v", stored, err)
					}
					again, err := json.Marshal(normalised)
					if err != nil {
						t.Fatalf("re-marshal query state: %v", err)
					}
					if string(again) != tc.want {
						t.Errorf("query state stored as %s (raw %q), want %s", again, stored, tc.want)
					}
					_ = runID
				})
			}
		})
	}
}

// readQueryStateColumn reads workflow_instances.query_state verbatim, so the
// test can tell `{}` from `null` -- a distinction the store's typed accessor
// cannot express.
func readQueryStateColumn(t *testing.T, backend StoreBackend, workflowID string) string {
	t.Helper()
	var q string
	switch backend.Name() {
	case "postgres":
		q = `SELECT query_state::text FROM workflow_instances WHERE id = $1`
	case "mysql":
		q = `SELECT CAST(query_state AS CHAR) FROM workflow_instances WHERE id = ?`
	case "mssql":
		q = `SELECT CAST(query_state AS NVARCHAR(MAX)) FROM workflow_instances WHERE id = @p1`
	default:
		t.Fatalf("readQueryStateColumn: unknown backend %q", backend.Name())
	}
	db := adminDBFor(t, backend)
	var raw sql.NullString
	if err := db.QueryRow(q, workflowID).Scan(&raw); err != nil {
		t.Fatalf("read query_state: %v", err)
	}
	if !raw.Valid {
		t.Fatalf("query_state is SQL NULL for %s", workflowID)
	}
	return raw.String
}

// adminDBFor returns a database handle that can read and write rows whatever
// tenant owns them -- the handle a test uses to check what the code under test
// actually wrote.
//
// On PostgreSQL and MySQL a plain test pool already is one; on SQL Server it is
// not, and that difference is the subject of testutil.AdminDB's comment. It was
// wrong here for long enough to fail three tests at once, all with the same
// symptom: a row written through a correctly scoped store, then read back as
// absent.
func adminDBFor(t *testing.T, backend StoreBackend) *sql.DB {
	t.Helper()
	switch backend.Name() {
	case "postgres":
		return testutil.TestDB(t, testutil.DialectPostgres)
	case "mysql":
		return testutil.MySQLTestDB(t)
	case "mssql":
		return testutil.MSSQLAdminDB(t, testutil.MSSQLTestDB(t))
	}
	t.Fatalf("adminDBFor: unknown backend %q", backend.Name())
	return nil
}
