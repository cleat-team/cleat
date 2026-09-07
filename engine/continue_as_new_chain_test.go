package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// dialectOf reports which dialect a store built by StoreBackend.Setup speaks,
// so a raw-SQL assertion can spell its placeholders correctly.
func dialectOf(t *testing.T, store WorkflowStore) Dialect {
	t.Helper()
	switch store.(type) {
	case *PostgresStore:
		return DialectPostgres
	case *MySQLStore:
		return DialectMySQL
	case *MSSQLStore:
		return DialectMSSQL
	default:
		t.Fatalf("dialectOf: unsupported store type %T", store)
		return ""
	}
}

// continuedFrom reads the raw continued_from column for one run.
//
// Raw SQL on purpose. Nothing in Go reads this column yet: cleat#826 was
// decided as "record the link", and exposing it -- "give me the terminal run
// of this chain" -- is caller-facing and is a separate decision, filed
// separately. So the assertion has to reach the row directly, and a test that
// went through a reader would be testing a reader that does not exist.
func continuedFrom(t *testing.T, store WorkflowStore, runID string) (string, bool) {
	t.Helper()
	d := dialectOf(t, store)
	var v sql.NullString
	q := "SELECT continued_from FROM workflow_instances WHERE id = " + d.placeholder(1)
	if err := rawDBOf(t, store).QueryRow(q, runID).Scan(&v); err != nil {
		t.Fatalf("reading continued_from for %s: %v", runID, err)
	}
	return v.String, v.Valid
}

// TestContinueAsNewRecordsThePredecessorItCameFrom is cleat#826.
//
// ContinueAsNew inserted a new instance row with a fresh id and marked the old
// one 'done', and nothing connected the two. A caller holding the id it started
// saw that run complete with an empty result -- `{}`, because a workflow that
// continues never returned a value -- and had no way to name the run carrying
// the real one. The feature's whole purpose is to outlive a single run.
//
// The link is written by the SUCCESSOR: continued_from on the new row holds the
// id of the run that continued into it. That direction is what makes the chain
// walkable both ways -- backwards by reading the column, forwards through
// idx_workflow_instances_continued_from.
//
// Deliberately NOT parent_workflow_id, and this test does not assert that
// because it cannot: the reason is that every consumer of that column would
// treat a continuation as a child. GetChildCount would count it as a live child
// of the run it replaced, and enforceParentClosePolicy sweeps a workflow's
// children at the moment it completes -- which is exactly when ContinueAsNew
// completes the predecessor, calling that function directly. See the migration
// header for the full reasoning.
func TestContinueAsNewRecordsThePredecessorItCameFrom(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			truncateAll(t, store)
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: "chain-wf", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}
			if _, _, err := store.StartNewRun(ctx, "", "chain-wf", 1,
				json.RawMessage(`{"remaining":3}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Walk a three-run chain, recording each id as it is created.
			chain := []string{}
			for i := 0; i < 3; i++ {
				wf, err := store.ClaimWorkflow(ctx, "worker-1")
				if err != nil || wf == nil {
					t.Fatalf("ClaimWorkflow (iteration %d): wf=%v err=%v", i, wf, err)
				}
				chain = append(chain, wf.ID)
				if i == 2 {
					break // leave the last one claimed rather than continuing again
				}
				next, err := store.ContinueAsNew(ctx, wf.ID, "worker-1", wf.Generation,
					"chain-wf", 1, json.RawMessage(`{"remaining":0}`), nil, "", nil, 0)
				if err != nil {
					t.Fatalf("ContinueAsNew (iteration %d): %v", i, err)
				}
				if next == wf.ID {
					t.Fatalf("ContinueAsNew returned the same id it was given (%s)", next)
				}
			}
			if len(chain) != 3 {
				t.Fatalf("expected a chain of 3 runs, got %d: %v", len(chain), chain)
			}

			// The first run began the chain: it continued from nothing.
			if got, ok := continuedFrom(t, store, chain[0]); ok {
				t.Errorf("the first run in a chain has continued_from = %q; it should be NULL, "+
					"since nothing continued into it", got)
			}

			// Every later run records the one before it.
			for i := 1; i < len(chain); i++ {
				got, ok := continuedFrom(t, store, chain[i])
				if !ok {
					t.Errorf("run %d of the chain (%s) has a NULL continued_from, so the chain "+
						"is broken at that link and a caller holding %s can never reach it",
						i, chain[i], chain[0])
					continue
				}
				if got != chain[i-1] {
					t.Errorf("run %d records continued_from = %s, want %s", i, got, chain[i-1])
				}
			}

			// The deliverable is a WALKABLE chain, so walk it the way a caller
			// would: forwards from the id they started, which is the direction
			// the index exists to serve. A correct column with no usable
			// forward walk would satisfy every assertion above and still leave
			// #826 open.
			d := dialectOf(t, store)
			q := "SELECT id FROM workflow_instances WHERE continued_from = " + d.placeholder(1)
			cur := chain[0]
			hops := 0
			for {
				var next string
				err := rawDBOf(t, store).QueryRow(q, cur).Scan(&next)
				if err == sql.ErrNoRows {
					break
				}
				if err != nil {
					t.Fatalf("walking forward from %s: %v", cur, err)
				}
				cur = next
				hops++
				if hops > len(chain) {
					t.Fatalf("forward walk did not terminate after %d hops; continued_from "+
						"has a cycle", hops)
				}
			}
			if hops != len(chain)-1 {
				t.Errorf("forward walk from the caller's id took %d hops, want %d", hops, len(chain)-1)
			}
			if cur != chain[len(chain)-1] {
				t.Errorf("forward walk ended at %s, want the terminal run %s", cur, chain[len(chain)-1])
			}
		})
	}
}
