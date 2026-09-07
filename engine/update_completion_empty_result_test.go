package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestAFailedUpdateCompletesAndSettlesTheCallersPromise is the test that should
// have existed, and it is a real-database test on purpose.
//
// # The defect
//
// `workflow_update_requests.result` is JSONB on PostgreSQL and JSON on MySQL,
// and "" is not valid JSON. Every FAILING update completes with an empty result
// by construction -- cleat/runtime_updates.go passes "" on all three failure
// paths (no handler registered, validator refusal, handler error), and the
// worker's stranded-update sweep passes "" too.
//
// So the UPDATE failed with `invalid input syntax for type json (22P02)`, the
// row stayed `pending`, and the caller's promise was never settled. That is the
// exact symptom updates were built to fix (IMPROVEMENT-PLAN 3.238), restored on
// the failure path -- and it reached every caller whose update was refused.
//
// # Why no existing test caught it
//
// `CompleteUpdateRequest` appeared in the test suite FOUR times before this
// file, and every one was a mock implementing the interface -- fakeUpdateStore,
// stubWorkflowStore, mockCollectMetricsStore, mockGCStore. The method had four
// test doubles and no test. A double accepts "" happily, because a Go string
// has no opinion about JSON; only a database does.
//
// That is CLAUDE.md's "watch which layer is holding the test up": the assertion
// was passing on the strength of the layer that could not fail.
//
// # All three dialects reject it, but only a measurement says so
//
// Each refuses "" in its own way, which is why all three run rather than one
// standing in for the others. Falsified 2026-09-07 by reverting jsonOrNull:
//
//	postgres  pq: invalid input syntax for type json (22P02)
//	mysql     Error 3140 (22032): Invalid JSON text: "The document is empty."
//	mssql     The UPDATE statement conflicted with the CHECK constraint
//	          "ck_workflow_update_requests_result"
//
// The MSSQL row is the one worth keeping. An earlier draft of this comment said
// SQL Server ACCEPTED "" -- because `migrations/mssql/001_schema.sql` CHECKs
// `payload` and not `result`, and 001 is where anyone looks. The constraint is
// added by `037_json_column_checks.sql`. Reading the first migration and
// concluding is the mistake CLAUDE.md warns about, and running the test is what
// caught it.
func TestAFailedUpdateCompletesAndSettlesTheCallersPromise(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			db, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			us, ok := store.(UpdateStore)
			if !ok {
				t.Fatalf("%s: the store does not satisfy UpdateStore", backend.name)
			}

			// A real workflow row: workflow_update_requests.workflow_id carries
			// a foreign key on PostgreSQL, so an invented ID is rejected there
			// with 23503 -- and accepted on the dialects that do not enforce it,
			// which would have made this test measure two different things.
			wfID := seedWorkflowForLock(t, store)
			const name = "refuse-me"
			if err := store.CreateUpdateRequest(ctx, wfID, name, `{"n":1}`, "prom-1"); err != nil {
				t.Fatalf("CreateUpdateRequest: %v", err)
			}

			// The failing shape: an empty result and an error message, exactly
			// what runUpdate passes when a handler is missing, a validator
			// refuses, or a handler errors.
			if err := us.CompleteUpdateRequest(ctx, wfID, name, "", "handler refused"); err != nil {
				t.Fatalf("completing a FAILED update was rejected by the database: %v\n\n"+
					"An update that fails must still be recordable as failed. When this errors "+
					"the row stays 'pending' and the caller's promise is never settled, which is "+
					"the hang updates exist to prevent. The cause was passing \"\" into a JSON "+
					"column; jsonOrNull renders it as NULL.", err)
			}

			// It must be COMPLETED, not merely un-errored: an UPDATE that
			// matched zero rows also returns nil.
			pending, err := us.GetPendingUpdateRequests(ctx, wfID)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests: %v", err)
			}
			for _, p := range pending {
				if p.UpdateName == name {
					t.Errorf("the request is still pending after being completed.\n\n"+
						"The write reported success and changed nothing, so the caller waits "+
						"forever on a promise nothing will settle. status=%q", p.Status)
				}
			}

			// And the empty result must round-trip as "" rather than as the
			// string "null" -- callers above the store read this field.
			var got sql.NullString
			q := `SELECT result FROM workflow_update_requests WHERE workflow_id = $1 AND update_name = $2`
			switch backend.dialect {
			case testutil.DialectMySQL:
				q = strings.NewReplacer("$1", "?", "$2", "?").Replace(q)
			case testutil.DialectMSSQL:
				q = strings.NewReplacer("$1", "@p1", "$2", "@p2").Replace(q)
			}
			if err := db.QueryRowContext(ctx, q, wfID, name).Scan(&got); err != nil {
				t.Fatalf("reading the stored result: %v", err)
			}
			if got.Valid && got.String != "" {
				t.Errorf("an empty result was stored as %q, want SQL NULL.\n\n"+
					"NULL is what round-trips: GetPendingUpdateRequests reads this through "+
					"COALESCE(...,''), so NULL comes back as \"\" and nothing above the store "+
					"sees a difference. Storing the literal \"null\" would come back as the "+
					"four-character string.", got.String)
			}
		})
	}
}
