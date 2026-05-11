package host

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Group 6 — Signals
// ---------------------------------------------------------------------------

func TestDeliverSignal(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "deliver-signal-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			if err := store.DeliverSignal(ctx, runID, "my-signal", `{"data":"hello"}`); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			payload, found, err := store.PollSignal(ctx, runID, "my-signal")
			if err != nil {
				t.Fatalf("PollSignal: %v", err)
			}
			if !found {
				t.Fatal("PollSignal: expected found=true")
			}
			if payload != `{"data":"hello"}` {
				t.Fatalf("PollSignal: expected payload %q, got %q", `{"data":"hello"}`, payload)
			}
		})
	}
}

func TestPollAndClaimSignal(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "poll-claim-signal-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			if err := store.DeliverSignal(ctx, runID, "sig-1", "payload-1"); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			// First call should find and claim the signal.
			payload, found, err := store.PollAndClaimSignal(ctx, runID, "sig-1")
			if err != nil {
				t.Fatalf("PollAndClaimSignal (first): %v", err)
			}
			if !found {
				t.Fatal("PollAndClaimSignal (first): expected found=true")
			}
			if payload != "payload-1" {
				t.Fatalf("PollAndClaimSignal (first): expected payload %q, got %q", "payload-1", payload)
			}

			// Second call should return found=false — signal already consumed.
			_, found, err = store.PollAndClaimSignal(ctx, runID, "sig-1")
			if err != nil {
				t.Fatalf("PollAndClaimSignal (second): %v", err)
			}
			if found {
				t.Fatal("PollAndClaimSignal (second): expected found=false (signal already consumed)")
			}
		})
	}
}

func TestPollSignal_NotDelivered(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "poll-notfound-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			_, found, err := store.PollSignal(ctx, runID, "never-delivered")
			if err != nil {
				t.Fatalf("PollSignal: %v", err)
			}
			if found {
				t.Fatal("PollSignal for undelivered signal: expected found=false")
			}
		})
	}
}

func TestPollSignal_NonDestructive(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "poll-nondestructive-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			if err := store.DeliverSignal(ctx, runID, "nd-sig", "nd-payload"); err != nil {
				t.Fatalf("DeliverSignal: %v", err)
			}

			// First PollSignal should find the signal.
			p1, found1, err := store.PollSignal(ctx, runID, "nd-sig")
			if err != nil {
				t.Fatalf("PollSignal (first): %v", err)
			}
			if !found1 {
				t.Fatal("PollSignal (first): expected found=true")
			}
			if p1 != "nd-payload" {
				t.Fatalf("PollSignal (first): expected payload %q, got %q", "nd-payload", p1)
			}

			// Second PollSignal must also find the signal — PollSignal is non-destructive.
			p2, found2, err := store.PollSignal(ctx, runID, "nd-sig")
			if err != nil {
				t.Fatalf("PollSignal (second): %v", err)
			}
			if !found2 {
				t.Fatal("PollSignal (second): expected found=true (non-destructive)")
			}
			if p2 != "nd-payload" {
				t.Fatalf("PollSignal (second): expected payload %q, got %q", "nd-payload", p2)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 7 — Child Workflows
// ---------------------------------------------------------------------------

func TestStartChildWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"parent":true}`), "child-parent-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if parentID == "" {
				t.Fatal("StartNewRun returned empty parentID")
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{"from":"parent"}`, 1, "abandon")
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			if childID == "" {
				t.Fatal("StartChildWorkflow returned empty runID")
			}
			if childID == parentID {
				t.Fatal("child runID must not equal parent runID")
			}

			childWF, err := store.GetWorkflowByID(ctx, childID)
			if err != nil {
				t.Fatalf("GetWorkflowByID for child: %v", err)
			}
			if childWF == nil {
				t.Fatal("GetWorkflowByID for child returned nil")
			}
			if childWF.DefName != "test-workflow" {
				t.Fatalf("child def name: expected %q, got %q", "test-workflow", childWF.DefName)
			}
		})
	}
}

func TestGetChildResult_Completed(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"parent":true}`), "child-result-parent-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if parentID == "" {
				t.Fatal("StartNewRun returned empty parentID")
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{"from":"parent"}`, 1, "abandon")
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			if childID == "" {
				t.Fatal("StartChildWorkflow returned empty childID")
			}

			// Claim the child so it transitions from "ready" to "running".
			claimed, err := store.ClaimWorkflow(ctx, "test-worker", "default")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if claimed == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			// Complete the claimed workflow.
			if err := store.CompleteWorkflow(ctx, claimed.ID, "test-worker", `{"child":"done"}`, nil); err != nil {
				t.Fatalf("CompleteWorkflow: %v", err)
			}

			// GetChildResult on the completed workflow ID.
			resultJSON, completed, err := store.GetChildResult(ctx, claimed.ID)
			if err != nil {
				t.Fatalf("GetChildResult: %v", err)
			}
			if !completed {
				t.Fatal("GetChildResult: expected completed=true")
			}
			if resultJSON != `{"child":"done"}` {
				t.Fatalf("GetChildResult: expected result %q, got %q", `{"child":"done"}`, resultJSON)
			}
		})
	}
}

func TestGetChildResult_NotCompleted(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"parent":true}`), "child-notcomplete-parent-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if parentID == "" {
				t.Fatal("StartNewRun returned empty parentID")
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{"from":"parent"}`, 1, "abandon")
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}
			if childID == "" {
				t.Fatal("StartChildWorkflow returned empty childID")
			}

			// Do NOT claim or complete the child — it should still be pending.
			_, completed, err := store.GetChildResult(ctx, childID)
			if err != nil {
				t.Fatalf("GetChildResult: %v", err)
			}
			if completed {
				t.Fatal("GetChildResult: expected completed=false (child was not completed)")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 8 — Reaping
// ---------------------------------------------------------------------------

func TestReapStaleInstances(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "reap-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			// Claim the workflow to transition it to "running".
			claimed, err := store.ClaimWorkflow(ctx, "reap-worker", "default")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if claimed == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			// Reap with a 1-nanosecond timeout. The just-claimed workflow's
			// heartbeat_at should already be stale at this granularity.
			count, err := store.ReapStaleInstances(ctx, time.Nanosecond)
			if err != nil {
				t.Fatalf("ReapStaleInstances(1ns): %v", err)
			}
			if count < 0 {
				t.Fatalf("ReapStaleInstances(1ns) returned negative count: %d", count)
			}

			// Reap with a zero timeout (reclaim any running workflow).
			count, err = store.ReapStaleInstances(ctx, 0)
			if err != nil {
				t.Fatalf("ReapStaleInstances(0): %v", err)
			}
			if count < 0 {
				t.Fatalf("ReapStaleInstances(0) returned negative count: %d", count)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 9 — List and Search
// ---------------------------------------------------------------------------

func TestListWorkflows_ByStatus(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create two ready workflows.
			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"seq":1}`), "list-by-status-1")
			if err != nil {
				t.Fatalf("StartNewRun 1: %v", err)
			}
			_, _, err = store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"seq":2}`), "list-by-status-2")
			if err != nil {
				t.Fatalf("StartNewRun 2: %v", err)
			}

			// Claim one workflow to transition it to "running".
			claimed, err := store.ClaimWorkflow(ctx, "list-worker", "default")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}
			if claimed == nil {
				t.Fatal("ClaimWorkflow returned nil")
			}

			// List ready workflows — should have at least 1.
			readyResults, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "ready"})
			if err != nil {
				t.Fatalf("ListWorkflows(status=ready): %v", err)
			}
			if len(readyResults) < 1 {
				t.Fatal("ListWorkflows(status=ready): expected at least 1 result")
			}

			// List running workflows — should have at least 1.
			runningResults, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "running"})
			if err != nil {
				t.Fatalf("ListWorkflows(status=running): %v", err)
			}
			if len(runningResults) < 1 {
				t.Fatal("ListWorkflows(status=running): expected at least 1 result")
			}

			// List with a nonexistent status — should return 0 results.
			nonexistentResults, err := store.ListWorkflows(ctx, WorkflowFilter{Status: "nonexistent"})
			if err != nil {
				t.Fatalf("ListWorkflows(status=nonexistent): %v", err)
			}
			if len(nonexistentResults) != 0 {
				t.Fatalf("ListWorkflows(status=nonexistent): expected 0 results, got %d", len(nonexistentResults))
			}
		})
	}
}

func TestListWorkflows_Pagination(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create 5 workflows with distinct idempotency keys.
			for i := 1; i <= 5; i++ {
				key := "paginate-key-" + itoa(i)
				_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"n":"`+itoa(i)+`"}`), key)
				if err != nil {
					t.Fatalf("StartNewRun %d: %v", i, err)
				}
			}

			// Get total count first (no limit filter → default 100).
			allResults, err := store.ListWorkflows(ctx, WorkflowFilter{})
			if err != nil {
				t.Fatalf("ListWorkflows(all): %v", err)
			}
			total := len(allResults)
			if total < 5 {
				t.Fatalf("ListWorkflows(all): expected at least 5 results, got %d", total)
			}

			// Page 1: offset=0, limit=2 → should return min(2, total).
			const pageSize = 2
			r1, err := store.ListWorkflows(ctx, WorkflowFilter{Offset: 0, Limit: pageSize})
			if err != nil {
				t.Fatalf("ListWorkflows(offset=0,limit=2): %v", err)
			}
			if len(r1) != pageSize {
				t.Fatalf("ListWorkflows(offset=0,limit=2): expected %d results, got %d", pageSize, len(r1))
			}

			// Page 2: offset=2 → should return min(2, total-2) results.
			r2, err := store.ListWorkflows(ctx, WorkflowFilter{Offset: 2, Limit: pageSize})
			if err != nil {
				t.Fatalf("ListWorkflows(offset=2,limit=2): %v", err)
			}
			exp2 := pageSize
			if total-2 < pageSize {
				exp2 = total - 2
			}
			if exp2 < 0 {
				exp2 = 0
			}
			if len(r2) != exp2 {
				t.Fatalf("ListWorkflows(offset=2,limit=2): expected %d results, got %d (total=%d)", exp2, len(r2), total)
			}

			// Verify no overlap between pages.
			page1IDs := make(map[string]bool)
			for _, wf := range r1 {
				page1IDs[wf.ID] = true
			}
			for _, wf := range r2 {
				if page1IDs[wf.ID] {
					t.Fatalf("ListWorkflows pagination: page 2 returned workflow %s already in page 1", wf.ID)
				}
			}

			// Page beyond end: offset=total, limit=5 → 0 results.
			r4, err := store.ListWorkflows(ctx, WorkflowFilter{Offset: total, Limit: 5})
			if err != nil {
				t.Fatalf("ListWorkflows(offset=%d,limit=5): %v", total, err)
			}
			if len(r4) != 0 {
				t.Fatalf("ListWorkflows(offset=%d,limit=5): expected 0 results, got %d", total, len(r4))
			}
		})
	}
}

func TestListWorkflows_Search(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "list-search-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			results, err := store.ListWorkflows(ctx, WorkflowFilter{Search: "test-workflow"})
			if err != nil {
				t.Fatalf("ListWorkflows(search=test-workflow): %v", err)
			}
			if len(results) < 1 {
				t.Fatal("ListWorkflows(search=test-workflow): expected at least 1 result")
			}
		})
	}
}

func TestListWorkflows_InputContains(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create workflows with distinctive JSON input.
			_, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"needle":"find-me-12345"}`), "input-filter-1")
			if err != nil {
				t.Fatalf("StartNewRun 1: %v", err)
			}
			_, _, err = store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{"other":"value"}`), "input-filter-2")
			if err != nil {
				t.Fatalf("StartNewRun 2: %v", err)
			}

			// Filter by a substring unique to the first workflow.
			results, err := store.ListWorkflows(ctx, WorkflowFilter{
				InputContains: "find-me-12345",
			})
			if err != nil {
				t.Fatalf("ListWorkflows(InputContains): %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("ListWorkflows(InputContains): expected 1 result, got %d", len(results))
			}

			// Filter by a substring that matches nothing.
			empty, err := store.ListWorkflows(ctx, WorkflowFilter{
				InputContains: "no-such-string-99999",
			})
			if err != nil {
				t.Fatalf("ListWorkflows(InputContains none): %v", err)
			}
			if len(empty) != 0 {
				t.Fatalf("ListWorkflows(InputContains none): expected 0, got %d", len(empty))
			}
		})
	}
}

func TestGetWorkflowByID(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{"key":"val"}`), "get-by-id-test")
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			if runID == "" {
				t.Fatal("StartNewRun returned empty runID")
			}

			// Look up the existing workflow.
			wf, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if wf == nil {
				t.Fatal("GetWorkflowByID returned nil for an existing workflow")
			}
			if wf.ID != runID {
				t.Fatalf("GetWorkflowByID: expected ID %q, got %q", runID, wf.ID)
			}
			if wf.DefName != "test-workflow" {
				t.Fatalf("GetWorkflowByID: expected DefName %q, got %q", "test-workflow", wf.DefName)
			}

			// Look up a nonexistent ID.
			wf, err = store.GetWorkflowByID(ctx, "nonexistent-id-12345")
			if err == nil && wf != nil {
				t.Fatal("GetWorkflowByID for nonexistent ID: expected nil or error")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Group 10 — Schedules
// ---------------------------------------------------------------------------

func TestCreateSchedule(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			sch := Schedule{
				Name:           "test-create-schedule",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "* * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
			}

			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}
		})
	}
}

func TestGetDueSchedules(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// setupTestData should have created "test-schedule" with NextRunAt
			// in the past, so GetDueSchedules includes it.
			schedules, err := store.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules: %v", err)
			}
			if len(schedules) < 1 {
				t.Fatal("GetDueSchedules: expected at least 1 schedule")
			}

			// Verify all returned schedules have NextRunAt <= now.
			now := time.Now()
			foundTestSchedule := false
			for _, s := range schedules {
				if s.NextRunAt.After(now) {
					t.Fatalf("GetDueSchedules: schedule %q has NextRunAt %v in the future", s.Name, s.NextRunAt)
				}
				if s.Name == "test-schedule" {
					foundTestSchedule = true
				}
			}
			if !foundTestSchedule {
				t.Fatal("GetDueSchedules: expected 'test-schedule' to be included")
			}
		})
	}
}

func TestUpdateScheduleNextRun(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			sch := Schedule{
				Name:           "test-update-schedule",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "0 * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			futureTime := time.Now().Add(24 * time.Hour)
			if err := store.UpdateScheduleNextRun(ctx, "test-update-schedule", futureTime); err != nil {
				t.Fatalf("UpdateScheduleNextRun: %v", err)
			}

			// Verify the schedule still exists via ListSchedules.
			schedules, err := store.ListSchedules(ctx)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			found := false
			for _, s := range schedules {
				if s.Name == "test-update-schedule" {
					found = true
					break
				}
			}
			if !found {
				t.Fatal("ListSchedules: expected 'test-update-schedule' to exist after UpdateScheduleNextRun")
			}
		})
	}
}

func TestSetScheduleEnabled(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			// Create a schedule with NextRunAt in the past so it would be due
			// if enabled.
			sch := Schedule{
				Name:           "test-disable-schedule",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "0 * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(-time.Hour),
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			// Disable the schedule.
			if err := store.SetScheduleEnabled(ctx, "test-disable-schedule", false); err != nil {
				t.Fatalf("SetScheduleEnabled: %v", err)
			}

			// GetDueSchedules should NOT include the disabled schedule.
			schedules, err := store.GetDueSchedules(ctx)
			if err != nil {
				t.Fatalf("GetDueSchedules: %v", err)
			}
			for _, s := range schedules {
				if s.Name == "test-disable-schedule" {
					t.Fatal("GetDueSchedules: disabled schedule should not appear")
				}
			}
		})
	}
}

func TestDeleteSchedule(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			t.Parallel()
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)

			ctx := context.Background()

			sch := Schedule{
				Name:           "test-delete-schedule",
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "0 * * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
			}
			if err := store.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			if err := store.DeleteSchedule(ctx, "test-delete-schedule"); err != nil {
				t.Fatalf("DeleteSchedule: %v", err)
			}
		})
	}
}

// itoa is a minimal int-to-string helper used to build unique idempotency keys
// without importing strconv or fmt (standard library only via "testing").
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
