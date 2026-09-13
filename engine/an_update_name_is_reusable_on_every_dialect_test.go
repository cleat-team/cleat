package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// An update name is reusable, and completing one request must not complete the
// others that share its name. cleat#1416.
//
// This test is the previous one inverted, deliberately rather than replaced.
// TestAReusedUpdateNameIsRefusedOnEveryDialect asserted that all three dialects
// refuse the second request under a name -- correct for the schema it was
// written against, where workflow_update_requests was
// PRIMARY KEY (workflow_id, update_name) and completion is an UPDATE rather
// than a delete, so a name was consumed for the life of the workflow. cleat#1392
// said in its own commit what should happen next:
//
//	If the answer later is "reusable", the new 409 becomes unreachable rather
//	than wrong, and the three-dialect test goes red rather than quiet.
//
// It went red. This is what replaced it.
//
// THE SECOND HALF IS THE PART THAT MATTERS. "The second request is accepted" is
// the easy assertion and it is not where the risk is. Two rows sharing a name
// is a state that could not previously exist, and every reader keyed on
// (workflow_id, update_name) silently addresses "one of them" in it. Measured
// on PostgreSQL against the migration with the old predicate still in place:
//
//	two pending rows, r1 and r2, both named "bump"
//	UPDATE ... WHERE workflow_id = ? AND update_name = 'bump' AND status = 'pending'
//	-> UPDATE 2
//
// One completion closes both. DurableCompleteUpdate then settles ONE promise,
// and the other caller holds a promise nothing will ever settle -- not even
// failStrandedUpdates, which sweeps rows that are still `pending`, and this row
// is now `completed`. That is the exact failure cleat/runtime_updates.go names
// as the reason updates exist at all.
func TestAnUpdateNameIsReusableOnEveryDialect(t *testing.T) {
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

			// Clear this workflow's rows first. Inherited from the test this
			// replaces, and still load-bearing for a different reason: rows
			// left by a previous run against the same database are now LEGAL,
			// so instead of failing a precondition they would inflate the
			// counts asserted below. Under the old primary key that stale row
			// announced itself; now it does not.
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
			// accepted" is measuring a workflow that accepts anything at all
			// for reasons unrelated to the name.
			if err := store.CreateUpdateRequest(ctx, wfID, "bump", `{"n":1}`, "prom-1"); err != nil {
				t.Fatalf("PRECONDITION FAILED: the first update request was refused on %s: %v",
					d.dialect, err)
			}

			if err := store.CreateUpdateRequest(ctx, wfID, "bump", `{"n":2}`, "prom-2"); err != nil {
				t.Fatalf("the second request under the same name was REFUSED on %s: %v\n\n"+
					"An update is a request and a request can be made twice. If this is a "+
					"uniqueness violation, the migration that drops "+
					"PRIMARY KEY (workflow_id, update_name) has not been applied to this "+
					"database -- CREATE TABLE IF NOT EXISTS never alters an existing one, so "+
					"a long-lived test database keeps the old shape.", d.dialect, err)
			}

			pending, err := store.GetPendingUpdateRequests(ctx, wfID)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests on %s: %v", d.dialect, err)
			}
			var bumps []UpdateRequestInfo
			for _, p := range pending {
				if p.UpdateName == "bump" {
					bumps = append(bumps, p)
				}
			}
			if len(bumps) != 2 {
				t.Fatalf("%d pending requests named \"bump\" on %s, want 2 -- both requests "+
					"were accepted, so both must be readable", len(bumps), d.dialect)
			}
			if bumps[0].RequestID == bumps[1].RequestID {
				t.Fatalf("both requests carry request_id %q on %s. They are then "+
					"indistinguishable to CompleteUpdateRequest, which is the whole "+
					"reason the column exists.", bumps[0].RequestID, d.dialect)
			}
			if bumps[0].RequestID == "" || bumps[1].RequestID == "" {
				t.Fatalf("an empty request_id on %s (%q, %q)", d.dialect,
					bumps[0].RequestID, bumps[1].RequestID)
			}
			// Both payloads survive. Under MySQL's old INSERT IGNORE the second
			// was silently discarded, which is cleat#1330's dangerous case.
			payloads := bumps[0].Payload + "|" + bumps[1].Payload
			for _, want := range []string{`"n":1`, `"n":2`} {
				if !strings.Contains(strings.ReplaceAll(payloads, " ", ""), want) {
					t.Errorf("payload %s missing on %s; both requests stored: %q",
						want, d.dialect, payloads)
				}
			}

			// THE HAZARD. Completing one must leave the other alone.
			first := bumps[0]
			if err := store.CompleteUpdateRequest(ctx, wfID, first.RequestID, `{"ok":true}`, ""); err != nil {
				t.Fatalf("CompleteUpdateRequest on %s: %v", d.dialect, err)
			}
			after, err := store.GetPendingUpdateRequests(ctx, wfID)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests after completing, on %s: %v", d.dialect, err)
			}
			var stillPending []string
			for _, p := range after {
				if p.UpdateName == "bump" {
					stillPending = append(stillPending, p.RequestID)
				}
			}
			if len(stillPending) != 1 {
				t.Fatalf("%d requests named \"bump\" are still pending on %s after completing "+
					"ONE of the two, want exactly 1 (%v).\n\n"+
					"Completing by name closes every row that shares it. Only one promise is "+
					"then settled, and the other caller's promise can never settle: "+
					"failStrandedUpdates sweeps rows that are still `pending`, and these are "+
					"now `completed`. That is the failure updates exist to prevent.",
					len(stillPending), d.dialect, stillPending)
			}
			if stillPending[0] == first.RequestID {
				t.Errorf("on %s the request that is still pending is the one that was "+
					"completed (%s) -- the predicate matched the wrong row",
					d.dialect, first.RequestID)
			}

			// A DIFFERENT name on the same workflow still works. Without this,
			// a store that accepted everything unconditionally would pass all
			// of the above.
			if err := store.CreateUpdateRequest(ctx, wfID, "other", `{"n":3}`, "prom-3"); err != nil {
				t.Errorf("a different update name was refused on %s: %v", d.dialect, err)
			}
		})
	}
}

// onlyPendingRequestID returns the request_id of the single pending request
// named `name` on this workflow.
//
// It exists because cleat#1416 changed what CompleteUpdateRequest's third
// argument MEANS -- from the update name to the request's own identity --
// without changing its type. Five existing tests passed the name, compiled
// fine, and silently completed nothing: the UPDATE matched no row, returned
// nil, and the request stayed `pending`. That is the same shape as the defect
// this change exists to prevent, so the tests failed loudly and correctly, each
// with a message about a request still being pending.
//
// Fatals if there is not exactly one. A caller that means "any of them" is
// asking the question this whole change made ambiguous, and should say which.
func onlyPendingRequestID(t *testing.T, store WorkflowStore, wfID, name string) string {
	t.Helper()
	pending, err := store.GetPendingUpdateRequests(context.Background(), wfID)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests(%s): %v", wfID, err)
	}
	var ids []string
	for _, p := range pending {
		if p.UpdateName == name {
			ids = append(ids, p.RequestID)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("%d pending requests named %q on %s, want exactly 1: %v",
			len(ids), name, wfID, ids)
	}
	return ids[0]
}
