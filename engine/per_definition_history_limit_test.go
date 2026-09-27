package engine

import (
	"context"
	"encoding/json"
	"testing"
)

func appendNEvents(t *testing.T, ctx context.Context, store WorkflowStore, wfID string, n int) {
	t.Helper()
	recs := make([]EventRecord, 0, n)
	for i := 0; i < n; i++ {
		recs = append(recs, EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: "op"})
	}
	if err := store.AppendEventHistoryBatch(ctx, wfID, recs); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}
}

// TestAPerDefinitionHistoryLimitIsBothSelectedAndHonoured is cleat#889.
//
// `workflow_defs.max_history_length` was readable by LoadWorkflowConfig and
// applied by nothing: a per-definition cap could be set in the schema and never
// took effect.
//
// BOTH HALVES ARE ASSERTED HERE, and that is the point. Compaction has two
// gates and a cap wired into only one of them is inert:
//
//	GetCompactionCandidates   decides what is CONSIDERED
//	CompactWorkflowHistory    decides what is COMPACTED
//
// Wiring only the second leaves a cap LOWER than the global threshold doing
// nothing at all, because the workflow never becomes a candidate at its own
// limit -- and that is the direction a chatty workflow would actually use it.
// Wiring only the first compacts to the wrong depth. The reachability guard
// cannot tell: it answers "is LoadWorkflowConfig called", which the narrow
// version satisfies.
//
// THE CAP IS SET THE WAY AN OPERATOR SETS IT, through the deploy payload
// (#924). It used to be written here with a direct UPDATE, because when this
// test was first written nothing else could write the column. That left a seam
// no test crossed once #924 landed: #924 asserts a deployed value round-trips
// back out of LoadWorkflowConfig, and this test asserted the two gates honour
// a column value, and NOTHING asserted that the value a deploy writes is the
// value the gates read. The two gates do not even reach the column the same
// way -- GetCompactionCandidates joins workflow_defs in SQL, CompactWorkflowHistory
// calls LoadWorkflowConfig -- so "same column, therefore same answer" was an
// inference, not a measurement. Deploying the cap here makes it a measurement.
func TestAPerDefinitionHistoryLimitIsBothSelectedAndHonoured(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			truncateAll(t, store)
			ctx := context.Background()

			// A global threshold far above what either workflow will hold, so
			// nothing here is eligible on the global rule alone.
			const global = 100
			const events = 6

			// capped-wf compacts at 4 events; uncapped-wf leaves the field
			// unset, which is the column default 0 and means "use the global".
			// The cap arrives THROUGH THE DEPLOY PAYLOAD, which is what makes
			// this test span the seam -- see the doc comment above.
			for name, limit := range map[string]int{"capped-wf": 4, "uncapped-wf": 0} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1, MaxHistoryLength: limit,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
				}
				// Confirm the deploy actually carried the value before
				// measuring anything with it. Without this a write half that
				// silently dropped the column would make the capped case look
				// exactly like the uncapped one, and the assertions below
				// would be reporting on a cap that was never set.
				got, err := store.LoadWorkflowConfig(ctx, name, 1)
				if err != nil {
					t.Fatalf("LoadWorkflowConfig(%s): %v", name, err)
				}
				if got != limit {
					t.Fatalf("deploying %s with MaxHistoryLength=%d stored %d -- the deploy "+
						"payload did not reach the column, so this test measures nothing",
						name, limit, got)
				}
			}

			capped, _, err := store.StartNewRun(ctx, "", "capped-wf", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (capped): %v", err)
			}
			uncapped, _, err := store.StartNewRun(ctx, "", "uncapped-wf", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (uncapped): %v", err)
			}
			appendNEvents(t, ctx, store, capped, events)
			appendNEvents(t, ctx, store, uncapped, events)

			// --- HALF ONE: selection ---
			ids, err := store.GetCompactionCandidates(ctx, global, 10)
			if err != nil {
				t.Fatalf("GetCompactionCandidates: %v", err)
			}
			inCandidates := func(id string) bool {
				for _, got := range ids {
					if got == id {
						return true
					}
				}
				return false
			}
			if !inCandidates(capped) {
				t.Errorf("a workflow with %d events and a per-definition cap of 4 is NOT a "+
					"compaction candidate at global threshold %d.\n\n"+
					"This is the half that did nothing before #889: the candidate query "+
					"filtered on the global threshold alone, so a cap LOWER than global -- "+
					"the direction anyone would actually set it -- never made the workflow "+
					"eligible, and CompactWorkflowHistory was never even called for it.",
					events, global)
			}
			if inCandidates(uncapped) {
				t.Errorf("a workflow with %d events and NO per-definition cap (0) is a "+
					"candidate at global threshold %d. 0 must mean \"use the global\", so "+
					"this is a regression in the default path, not a new feature.",
					events, global)
			}

			// --- HALF TWO: the compaction decision ---
			if err := CompactWorkflowHistory(ctx, store, capped, global, nil); err != nil {
				t.Fatalf("CompactWorkflowHistory (capped): %v", err)
			}
			after, err := store.LoadEventHistory(ctx, capped)
			if err != nil {
				t.Fatalf("LoadEventHistory (capped): %v", err)
			}
			if len(after) >= events {
				t.Errorf("after compaction the capped workflow still has %d events (was %d). "+
					"CompactWorkflowHistory compared against the GLOBAL threshold, so it "+
					"returned early for a workflow the candidate query had already selected "+
					"on its own cap -- the two gates disagreeing.", len(after), events)
			}

			// The uncapped one must be left alone by the same call.
			if err := CompactWorkflowHistory(ctx, store, uncapped, global, nil); err != nil {
				t.Fatalf("CompactWorkflowHistory (uncapped): %v", err)
			}
			untouched, err := store.LoadEventHistory(ctx, uncapped)
			if err != nil {
				t.Fatalf("LoadEventHistory (uncapped): %v", err)
			}
			if len(untouched) != events {
				t.Errorf("the uncapped workflow lost events (%d, was %d): with no override "+
					"the global threshold of %d applies and nothing should have compacted",
					len(untouched), events, global)
			}
		})
	}
}
