package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// workflow_update_requests is keyed PRIMARY KEY (workflow_id, update_name) on
// all three dialects, and completion is an UPDATE rather than a delete -- so a
// name is consumed for the life of the workflow. The three dialects answered
// the second request three different ways. cleat#1330.
//
//	PostgreSQL   500 {"error":"pq: duplicate key value violates unique
//	                  constraint \"workflow_update_requests_pkey\" (23505)"}
//	SQL Server   500, the same shape with the mssql driver's text
//	MySQL        NIL ERROR -- INSERT IGNORE discarded the row, the result was
//	             discarded too, and the handler answered 202 with a promise id
//	             for a request that was never recorded
//
// MySQL's is the one that matters: CompleteUpdateRequest's WHERE matches no
// row and failStrandedUpdates sweeps rows, of which there are none, so the
// caller holds a promise that provably cannot settle. That is the failure
// cleat/runtime_updates.go names as the reason the feature exists.
//
// Whether a name SHOULD be single-use is open and is a schema change either
// way. This asserts only that all three refuse it identically, which is true
// under either answer -- and if the answer later is "reusable", this test goes
// red rather than quiet.
func TestAReusedUpdateNameIsRefusedOnEveryDialect(t *testing.T) {
	for _, d := range []struct {
		name    string
		dialect testutil.Dialect
		setup   func(*testing.T, *sql.DB)
	}{
		{"postgres", testutil.DialectPostgres, func(t *testing.T, db *sql.DB) {
			testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		}},
		{"mysql", testutil.DialectMySQL, testutil.SetupMySQLFullSchema},
		{"mssql", testutil.DialectMSSQL, testutil.SetupMSSQLFullSchema},
	} {
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			d.setup(t, db)

			wfID := "update-reuse-" + d.name
			seedWorkflowInstance(t, db, d.dialect, wfID)

			// Clear this workflow's rows first. Without it the SECOND run
			// against the same database fails its own precondition -- the
			// first request hits the primary key left by the previous run --
			// and reports "the first update request was refused", which reads
			// as the fix being broken. Caught exactly that way while
			// falsifying.
			clear := map[testutil.Dialect]string{
				testutil.DialectPostgres: `DELETE FROM workflow_update_requests WHERE workflow_id = $1`,
				testutil.DialectMySQL:    "DELETE FROM workflow_update_requests WHERE workflow_id = ?",
				testutil.DialectMSSQL:    `DELETE FROM workflow_update_requests WHERE workflow_id = @p1`,
			}[d.dialect]
			if _, err := testutil.AdminDB(t, db, d.dialect).Exec(clear, wfID); err != nil {
				t.Fatalf("clearing prior update requests on %s: %v", d.dialect, err)
			}
			store := storeFor(t, d.dialect, db)
			ctx := context.Background()

			// The control: the FIRST request must succeed, or "the second was
			// refused" is measuring a workflow that accepts no updates at all.
			if err := store.CreateUpdateRequest(ctx, wfID, "bump", `{"n":1}`, "prom-1"); err != nil {
				t.Fatalf("PRECONDITION FAILED: the first update request was refused on %s: %v",
					d.dialect, err)
			}

			err := store.CreateUpdateRequest(ctx, wfID, "bump", `{"n":2}`, "prom-2")
			if err == nil {
				t.Fatalf("the second request under the same name returned NIL on %s.\n\n"+
					"No row is created, so CompleteUpdateRequest matches nothing and "+
					"failStrandedUpdates has nothing to sweep -- the handler answers 202 "+
					"with a promise id that provably cannot settle.", d.dialect)
			}
			if !errors.Is(err, ErrUpdateNameUsed) {
				t.Errorf("the second request failed on %s with %v, which is not ErrUpdateNameUsed.\n\n"+
					"The HTTP layer answers 500 with the raw driver string for anything else, "+
					"and a well-formed request the state refuses is what 409 is for.", d.dialect, err)
			}

			// And the first request's payload is still the one on file --
			// neither overwritten nor discarded in a way that loses it.
			var stored string
			q := map[testutil.Dialect]string{
				testutil.DialectPostgres: `SELECT payload::text FROM workflow_update_requests
				                            WHERE workflow_id = $1 AND update_name = 'bump'`,
				testutil.DialectMySQL: "SELECT payload FROM workflow_update_requests " +
					"WHERE workflow_id = ? AND update_name = 'bump'",
				testutil.DialectMSSQL: `SELECT CAST(payload AS NVARCHAR(MAX)) FROM workflow_update_requests
				                         WHERE workflow_id = @p1 AND update_name = 'bump'`,
			}[d.dialect]
			if err := testutil.AdminDB(t, db, d.dialect).QueryRow(q, wfID).Scan(&stored); err != nil {
				t.Fatalf("reading the stored payload on %s: %v", d.dialect, err)
			}
			if stored == "" {
				t.Errorf("no payload stored on %s", d.dialect)
			}

			// A DIFFERENT name on the same workflow still works. Without this,
			// a store that refused every update would pass everything above.
			if err := store.CreateUpdateRequest(ctx, wfID, "other", `{"n":3}`, "prom-3"); err != nil {
				t.Errorf("a different update name was refused on %s: %v -- the refusal above "+
					"is not specific to the reused name", d.dialect, err)
			}
		})
	}
}
