package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// setMaxHistoryLength writes workflow_defs.max_history_length directly.
//
// Direct SQL because there is no other way: WorkflowDef carries no
// MaxHistoryLength field and DeployWorkflowDef never writes the column, so an
// operator setting a per-definition cap today does it exactly like this. That
// gap is on the WRITE side and is deliberately not closed here; cleat#889 is
// about LoadWorkflowConfig being read by nothing, and this test documents the
// remaining half rather than hiding it behind a helper that does not exist.
func setMaxHistoryLength(t *testing.T, store WorkflowStore, defName string, version, limit int) {
	t.Helper()
	d := dialectOf(t, store)
	q := "UPDATE workflow_defs SET max_history_length = " + d.placeholder(1) +
		" WHERE name = " + d.placeholder(2) + " AND version = " + d.placeholder(3)
	if _, err := rawDBOf(t, store).Exec(q, limit, defName, version); err != nil {
		t.Fatalf("setting max_history_length for %s v%d: %v", defName, version, err)
	}

	// Read it back rather than trusting RowsAffected. MySQL counts rows
	// CHANGED, not matched, so setting the column to the value it already
	// holds -- which the limit=0 case does -- reports 0 affected and is
	// indistinguishable from "no such definition". A read-back answers the
	// question actually being asked, on all three dialects, and verifies the
	// write instead of the statement.
	var got int
	rq := "SELECT max_history_length FROM workflow_defs WHERE name = " + d.placeholder(1) +
		" AND version = " + d.placeholder(2)
	if err := rawDBOf(t, store).QueryRow(rq, defName, version).Scan(&got); err != nil {
		t.Fatalf("reading back max_history_length for %s v%d (was the definition deployed?): %v",
			defName, version, err)
	}
	if got != limit {
		t.Fatalf("max_history_length for %s v%d is %d, want %d -- the write did not take, "+
			"so this test would measure nothing", defName, version, got, limit)
	}
}

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

			for _, name := range []string{"capped-wf", "uncapped-wf"} {
				if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
					Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
					ABIVersion: 1, MinVersion: 1,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef(%s): %v", name, err)
				}
			}
			// capped-wf compacts at 4 events; uncapped-wf keeps the default 0,
			// which means "use the global".
			setMaxHistoryLength(t, store, "capped-wf", 1, 4)
			setMaxHistoryLength(t, store, "uncapped-wf", 1, 0)

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
