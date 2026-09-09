package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// cleat#1091: every dialect's GetWorkflowByID SELECTed completed_at, scanned it
// into a local, and never assigned it. The value was read from the database on
// every fetch and dropped, so no client could see when a run finished.
//
// Against real databases on every dialect, because the claim is about what a
// SELECT and a Scan do together. A mock returns whatever it is told and can
// only restate the assertion.
func TestACompletedRunReportsWhenItFinished(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "completed-at", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			// Before it finishes, the absence has to be legible as an absence.
			running, err := store.GetWorkflowByID(ctx, id)
			if err != nil || running == nil {
				t.Fatalf("GetWorkflowByID (running): %v %v", running, err)
			}
			if running.CompletedAt != nil {
				t.Errorf("a run that has not finished reports CompletedAt = %v, want nil.\n\n"+
					"An unfinished run carrying a completion time is worse than one "+
					"carrying none: a client can parse it and compare it.", *running.CompletedAt)
			}

			claimed, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || claimed == nil {
				t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
			}
			if err := store.FinalizeWorkflowSegment(ctx, id, "worker-1", claimed.Generation,
				nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("finalize: %v", err)
			}

			done, err := store.GetWorkflowByID(ctx, id)
			if err != nil || done == nil {
				t.Fatalf("GetWorkflowByID (done): %v %v", done, err)
			}
			if done.CompletedAt == nil {
				t.Fatalf("a run with status %q reports no completion time.\n\n"+
					"completed_at is set on the row -- finalize_workflow_status writes "+
					"it -- and this SELECT asks for it. It was scanned into a local "+
					"and never assigned, which is cleat#1091.", done.Status)
			}

			// NO wall-clock window. completed_at is stamped by the DATABASE
			// clock and time.Now() here is the WORKER clock (see children.go's
			// note on exactly this pair), so any tolerance would be measuring
			// clock skew rather than the fix.
			//
			// The ordering of two database-stamped values is clock-free and is
			// the property that actually distinguishes "the real column" from
			// "some timestamp": >= rather than >, because both can land on one
			// tick at second resolution. This is the assertion #1090 quotes
			// upstream making, for the same reason.
			if done.CompletedAt.Before(done.CreatedAt) {
				t.Errorf("completed_at %v is before created_at %v.\n\n"+
					"Both are stamped by the database, so this ordering cannot be "+
					"broken by clock skew between processes.", *done.CompletedAt, done.CreatedAt)
			}
		})
	}
}

// The pointer is the load-bearing part of the fix, so it gets its own check.
//
// No database: this is a statement about the wire shape, and a marshal is the
// whole of it. It runs on every configuration, including one with no DSN set.
func TestAnUnfinishedRunOmitsCompletedAtRatherThanDatingItToYearOne(t *testing.T) {
	b, err := json.Marshal(&WorkflowInstance{ID: "wf-1", Status: "running"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)

	if strings.Contains(got, "completed_at") {
		t.Errorf("a running workflow serialises completed_at:\n\n  %s\n\n"+
			"With a plain time.Time this field is a struct, which `omitempty` "+
			"does not omit, so every in-flight run would report a completion "+
			"time of 0001-01-01T00:00:00Z -- parseable, comparable, and wrong. "+
			"Nil is the only spelling of \"not finished\" a JSON client cannot "+
			"mistake for a value.", got)
	}
	// Scoped to this field on purpose. A bare `strings.Contains(got,
	// "0001-01-01")` fails on develop AND on the fix, because next_wake_at and
	// created_at are plain time.Time and already serialise as year one for an
	// unstarted run:
	//
	//   "next_wake_at":"0001-01-01T00:00:00Z","created_at":"0001-01-01T00:00:00Z"
	//
	// That is not this PR's business to change -- created_at is never absent on
	// a real row, and next_wake_at is its own question -- but it is worth
	// recording as the reason the pointer is not a stylistic preference. The
	// failure mode being avoided is already present twice in this struct.
	if i := strings.Index(got, "completed_at"); i >= 0 && strings.Contains(got[i:min(i+64, len(got))], "0001-01-01") {
		t.Errorf("a running workflow dates completed_at to year one:\n\n  %s", got)
	}
}
