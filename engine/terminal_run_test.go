package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// TestGetTerminalRunFollowsTheChainAndGetWorkflowByIDDoesNot is cleat#887.
//
// Two halves, in one test on purpose, because the guarantee is the PAIR: the
// new call follows the chain AND the existing one still does not. Split across
// two tests, someone could satisfy the first by making GetWorkflowByID follow —
// which is option B on #887, the option that was considered and rejected
// because four call sites and the admin dashboard read through it and want the
// row they named. Asserting both here means that change fails a test rather
// than passing one.
func TestGetTerminalRunFollowsTheChainAndGetWorkflowByIDDoesNot(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			truncateAll(t, store)
			ctx := context.Background()

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: "terminal-wf", Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			// --- an id that names nothing ---
			missing, err := store.GetTerminalRun(ctx, "no-such-run")
			if err != nil {
				t.Fatalf("GetTerminalRun(unknown id): %v", err)
			}
			if missing != nil {
				t.Errorf("GetTerminalRun(unknown id) = %v, want nil -- it must match "+
					"GetWorkflowByID rather than inventing a row", missing)
			}

			// --- a workflow that never continued is its own terminal run ---
			if _, _, err := store.StartNewRun(ctx, "", "terminal-wf", 1,
				json.RawMessage(`{"n":0}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun (lone): %v", err)
			}
			lone, err := store.ClaimWorkflow(ctx, "worker-lone")
			if err != nil || lone == nil {
				t.Fatalf("ClaimWorkflow (lone): wf=%v err=%v", lone, err)
			}
			solo, err := store.GetTerminalRun(ctx, lone.ID)
			if err != nil {
				t.Fatalf("GetTerminalRun (lone): %v", err)
			}
			if solo == nil || solo.ID != lone.ID {
				t.Errorf("a workflow that never continued must be its own terminal run; "+
					"got %v, want %s", solo, lone.ID)
			}
			if solo != nil && solo.ContinuedFrom != "" {
				t.Errorf("lone run reports ContinuedFrom = %q, want empty", solo.ContinuedFrom)
			}

			// --- a three-run chain ---
			if _, _, err := store.StartNewRun(ctx, "", "terminal-wf", 1,
				json.RawMessage(`{"n":3}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun (chain): %v", err)
			}
			var chain []string
			for i := 0; i < 3; i++ {
				wf, err := store.ClaimWorkflow(ctx, "worker-chain")
				if err != nil || wf == nil {
					t.Fatalf("ClaimWorkflow (chain %d): wf=%v err=%v", i, wf, err)
				}
				if wf.ID == lone.ID {
					// The lone run above is claimed and running, so it should
					// not come back; if it does the rest of this test is
					// measuring the wrong workflow.
					t.Fatalf("claim returned the lone run %s while building the chain", wf.ID)
				}
				chain = append(chain, wf.ID)
				if i == 2 {
					break
				}
				if _, err := store.ContinueAsNew(ctx, wf.ID, "worker-chain", wf.Generation,
					"terminal-wf", 1, json.RawMessage(`{"n":0}`), nil, "", nil, 0); err != nil {
					t.Fatalf("ContinueAsNew (%d): %v", i, err)
				}
			}
			head, tail := chain[0], chain[len(chain)-1]

			// THE NEW CALL FOLLOWS.
			term, err := store.GetTerminalRun(ctx, head)
			if err != nil {
				t.Fatalf("GetTerminalRun (chain head): %v", err)
			}
			if term == nil {
				t.Fatalf("GetTerminalRun(%s) returned nil for a run that exists", head)
			}
			if term.ID != tail {
				t.Errorf("GetTerminalRun(%s) = %s, want the terminal run %s -- the caller "+
					"polls the id they started and this is the call that has to reach the "+
					"run carrying the result", head, term.ID, tail)
			}

			// THE EXISTING CALL DOES NOT. This is the half that keeps option B
			// from being introduced by accident.
			named, err := store.GetWorkflowByID(ctx, head)
			if err != nil {
				t.Fatalf("GetWorkflowByID (chain head): %v", err)
			}
			if named == nil {
				t.Fatalf("GetWorkflowByID(%s) returned nil for a run that exists", head)
			}
			if named.ID != head {
				t.Errorf("GetWorkflowByID(%s) returned %s. It must return THE ROW WITH THAT ID: "+
					"four call sites and the admin dashboard read through it and want the row "+
					"they named. Following the chain here is #887 option B, which was rejected.",
					head, named.ID)
			}

			// --- the field is populated on the read path ---
			if named.ContinuedFrom != "" {
				t.Errorf("the head of a chain reports ContinuedFrom = %q, want empty",
					named.ContinuedFrom)
			}
			second, err := store.GetWorkflowByID(ctx, chain[1])
			if err != nil {
				t.Fatalf("GetWorkflowByID (chain[1]): %v", err)
			}
			if second == nil || second.ContinuedFrom != head {
				got := ""
				if second != nil {
					got = second.ContinuedFrom
				}
				t.Errorf("GetWorkflowByID(%s).ContinuedFrom = %q, want %s", chain[1], got, head)
			}

			// Walking from the MIDDLE also reaches the end -- a walk that only
			// works from the head would pass every assertion above.
			fromMiddle, err := store.GetTerminalRun(ctx, chain[1])
			if err != nil {
				t.Fatalf("GetTerminalRun (middle): %v", err)
			}
			if fromMiddle == nil || fromMiddle.ID != tail {
				t.Errorf("GetTerminalRun from the middle of a chain must still reach %s, got %v",
					tail, fromMiddle)
			}

			// THE CHAIN IS STILL RUNNING, and that is the case this test was
			// already in without asserting anything about it: chain[2] was
			// claimed above and never continued, so it is 'running' with no
			// result. GetTerminalRun still answers, and what it answers is the
			// last link SO FAR -- not an outcome.
			//
			// #904: the interface doc claimed this returned "the one carrying
			// the result the caller is waiting for". A port-suite test believed
			// that, read .Result immediately, and dereferenced a nil. Asserting
			// the honest semantics here is what stops the sentence coming back.
			if term.Status != "running" {
				t.Errorf("mid-chain, GetTerminalRun returned status %q; the last link is the "+
					"one currently executing, so this is the case a caller must handle",
					term.Status)
			}
			if term.Result != "" {
				t.Errorf("mid-chain, GetTerminalRun returned result %q; a still-running chain "+
					"has no outcome to carry, and a caller that reads Result without checking "+
					"Status gets nothing", term.Result)
			}

			// The corollary, and the reason polling the id you started is not
			// enough: the FIRST run reached a terminal status the moment it
			// continued. "The run I started is done" is true almost at once and
			// says nothing about the chain.
			if named.Status != "done" {
				t.Errorf("the head of a chain reports status %q, want \"done\" -- it completes "+
					"as soon as it continues, which is exactly why waiting on it is not "+
					"waiting for the work", named.Status)
			}

			// And the terminal run is its own terminal run.
			atEnd, err := store.GetTerminalRun(ctx, tail)
			if err != nil {
				t.Fatalf("GetTerminalRun (tail): %v", err)
			}
			if atEnd == nil || atEnd.ID != tail {
				t.Errorf("GetTerminalRun(%s) on the last run = %v, want itself", tail, atEnd)
			}
		})
	}
}
