package engine

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pure functions
// ---------------------------------------------------------------------------

func TestCompactJSONString_Empty(t *testing.T) {
	result := compactJSONString("")
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestCompactJSONString_ValidJSON(t *testing.T) {
	result := compactJSONString(`{"a": 1,  "b": 2}`)
	if result != `{"a":1,"b":2}` {
		t.Errorf("expected compact JSON, got %q", result)
	}
}

func TestCompactJSONString_InvalidJSON_Fallback(t *testing.T) {
	result := compactJSONString("not-json")
	if result != "not-json" {
		t.Errorf("expected fallback to original, got %q", result)
	}
}

func TestCompactJSONString_AlreadyCompact(t *testing.T) {
	result := compactJSONString(`{"a":1}`)
	if result != `{"a":1}` {
		t.Errorf("expected unchanged, got %q", result)
	}
}

func TestCompactJSONString_NestedSpacing(t *testing.T) {
	result := compactJSONString(`{"x": [1,  2,   3], "y": {"z":  "val"}}`)
	if result != `{"x":[1,2,3],"y":{"z":"val"}}` {
		t.Errorf("got %q", result)
	}
}

func TestPercentile_ExactBoundaries(t *testing.T) {
	sorted := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if p := percentile(sorted, 0.50); p != 50 {
		t.Errorf("P50: expected 50, got %d", p)
	}
	if p := percentile(sorted, 0.90); p != 90 {
		t.Errorf("P90: expected 90, got %d", p)
	}
	if p := percentile(sorted, 0.10); p != 10 {
		t.Errorf("P10: expected 10, got %d", p)
	}
}

func TestPercentile_EdgeCases(t *testing.T) {
	if p := percentile(nil, 0.50); p != 0 {
		t.Errorf("nil: expected 0, got %d", p)
	}
	if p := percentile([]int64{}, 0.50); p != 0 {
		t.Errorf("empty: expected 0, got %d", p)
	}
	if p := percentile([]int64{42}, 0.10); p != 42 {
		t.Errorf("single p10: expected 42, got %d", p)
	}
	if p := percentile([]int64{42}, 0.99); p != 42 {
		t.Errorf("single p99: expected 42, got %d", p)
	}
}

func TestPercentile_Rounding(t *testing.T) {
	sorted := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if p := percentile(sorted, 0.50); p != 6 {
		t.Errorf("P50: expected 6, got %d", p)
	}
}

func TestPercentile_P1_P99(t *testing.T) {
	sorted := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63, 64, 65, 66, 67, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82, 83, 84, 85, 86, 87, 88, 89, 90, 91, 92, 93, 94, 95, 96, 97, 98, 99, 100}
	if p := percentile(sorted, 0.01); p != 1 {
		t.Errorf("P01: expected 1, got %d", p)
	}
	if p := percentile(sorted, 0.99); p != 99 {
		t.Errorf("P99: expected 99, got %d", p)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newMySQLStoreForTest creates a MySQLStore backed by a mock DB.
func newMySQLStoreForTest(t *testing.T, rowsCfg []mockRowsResult, execCfg []mockExecResult) *MySQLStore {
	t.Helper()
	db := newMockDBForPostgres(t, rowsCfg, execCfg)
	t.Cleanup(func() { db.Close() })
	return NewMySQLStore(db)
}

// execOk is a default success exec result (1 row affected).
var execOk = mockExecResult{affected: 1}

// queryRowOk returns a single-row query result with the given values.
func queryRowOk(match string, vals ...driver.Value) mockRowsResult {
	return mockRowsResult{match: match, data: [][]driver.Value{vals}}
}

// ---------------------------------------------------------------------------
// Promises
// ---------------------------------------------------------------------------

func TestMySQLStore_CreatePromise(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "INSERT IGNORE INTO workflow_promises", affected: 1},
	})
	err := store.CreatePromise(testCtx, "wf-1", "my-promise", "promise-uuid")
	if err != nil {
		t.Fatalf("CreatePromise: %v", err)
	}
}

func TestMySQLStore_ResolvePromise(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status = ?, result = ?", affected: 1},
		{match: "UPDATE workflow_instances SET next_wake_at", affected: 1},
	})
	err := store.ResolvePromise(testCtx, "wf-1", "promise-uuid", `{"ok":true}`)
	if err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}
}

func TestMySQLStore_RejectPromise(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_promises SET status = ?, error_msg = ?", affected: 1},
		{match: "UPDATE workflow_instances SET next_wake_at", affected: 1},
	})
	err := store.RejectPromise(testCtx, "wf-1", "promise-uuid", "something went wrong")
	if err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}
}

func TestMySQLStore_GetPromise_Found(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT status, CAST(result AS CHAR), error_msg FROM workflow_promises",
			"resolved", `{"ok":true}`, ""),
	}, nil)
	status, result, errMsg, err := store.GetPromise(testCtx, "wf-1", "p-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "resolved" {
		t.Errorf("expected resolved, got %q", status)
	}
	if result != `{"ok":true}` {
		t.Errorf("expected compact JSON, got %q", result)
	}
	if errMsg != "" {
		t.Errorf("expected empty errMsg, got %q", errMsg)
	}
}

func TestMySQLStore_GetPromise_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	status, result, errMsg, err := store.GetPromise(testCtx, "wf-1", "p-1")
	if err != nil {
		t.Fatalf("GetPromise: %v", err)
	}
	if status != "pending" {
		t.Errorf("expected pending for not found, got %q", status)
	}
	if result != "" || errMsg != "" {
		t.Errorf("expected empty result/errMsg, got %q / %q", result, errMsg)
	}
}

func TestMySQLStore_GetPromise_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT status, CAST(result AS CHAR), error_msg FROM workflow_promises", err: sql.ErrConnDone},
	}, nil)
	_, _, _, err := store.GetPromise(testCtx, "wf-1", "p-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMySQLStore_ListPromises_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	promises, err := store.ListPromises(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("ListPromises: %v", err)
	}
	if len(promises) != 0 {
		t.Errorf("expected empty, got %d", len(promises))
	}
}

func TestMySQLStore_ListPromises_WithRows(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	resolvedAt := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT promise_id, promise_name, status",
			data: [][]driver.Value{
				{"p-1", "promise-a", "resolved", "ok", "", createdAt, resolvedAt},
				{"p-2", "promise-b", "pending", "", "", createdAt, nil},
			},
		},
	}, nil)
	promises, err := store.ListPromises(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("ListPromises: %v", err)
	}
	if len(promises) != 2 {
		t.Fatalf("expected 2 promises, got %d", len(promises))
	}
	if promises[0].PromiseID != "p-1" || promises[0].Status != "resolved" {
		t.Errorf("unexpected first: %+v", promises[0])
	}
	if promises[1].ResolvedAt != nil {
		t.Error("expected nil ResolvedAt")
	}
}

func TestMySQLStore_ListPromises_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT promise_id, promise_name, status", err: sql.ErrConnDone},
	}, nil)
	_, err := store.ListPromises(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// Update Requests
// ---------------------------------------------------------------------------

func TestMySQLStore_CreateUpdateRequest(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "INSERT IGNORE INTO workflow_update_requests", affected: 1},
	})
	err := store.CreateUpdateRequest(testCtx, "wf-1", "update-name", "{}", "promise-1")
	if err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}
}

func TestMySQLStore_GetPendingUpdateRequests_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	reqs, err := store.GetPendingUpdateRequests(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests: %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("expected empty, got %d", len(reqs))
	}
}

func TestMySQLStore_GetPendingUpdateRequests_WithRows(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT workflow_id, update_name",
			data: [][]driver.Value{
				{"wf-1", "update-a", `{}`, "prom-1", "pending", "", "", createdAt},
			},
		},
	}, nil)
	reqs, err := store.GetPendingUpdateRequests(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].UpdateName != "update-a" {
		t.Errorf("unexpected: %+v", reqs)
	}
}

func TestMySQLStore_CompleteUpdateRequest(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_update_requests", affected: 1},
	})
	err := store.CompleteUpdateRequest(testCtx, "wf-1", "update-name", "ok", "")
	if err != nil {
		t.Fatalf("CompleteUpdateRequest: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrency Keys
// ---------------------------------------------------------------------------

func TestMySQLStore_AcquireConcurrencyKey_Success(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT workflow_id FROM concurrency_keys WHERE key_hash", "wf-1"),
	}, []mockExecResult{
		{match: "DELETE FROM concurrency_keys WHERE key_hash", affected: 0},
		{match: "INSERT IGNORE INTO concurrency_keys", affected: 1},
	})
	acquired, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if !acquired {
		t.Error("expected acquired=true")
	}
}

func TestMySQLStore_AcquireConcurrencyKey_AlreadyHeld(t *testing.T) {
	// Default mock behavior returns empty rows, which means insert succeeded
	// but verify returns ErrNoRows -> not acquired.
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys WHERE key_hash", affected: 0},
		{match: "INSERT IGNORE INTO concurrency_keys", affected: 1},
	})
	acquired, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-2", 30*time.Second)
	if err != nil {
		t.Fatalf("AcquireConcurrencyKey: %v", err)
	}
	if acquired {
		t.Error("expected acquired=false")
	}
}

func TestMySQLStore_AcquireConcurrencyKey_CleanupError(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys WHERE key_hash", err: sql.ErrConnDone},
	})
	_, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err == nil {
		t.Fatal("expected error from cleanup failure")
	}
}

func TestMySQLStore_AcquireConcurrencyKey_InsertError(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys WHERE key_hash", affected: 0},
		{match: "INSERT IGNORE INTO concurrency_keys", err: sql.ErrConnDone},
	})
	_, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err == nil {
		t.Fatal("expected error from insert failure")
	}
}

func TestMySQLStore_AcquireConcurrencyKey_VerifyError(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT workflow_id FROM concurrency_keys WHERE key_hash", err: sql.ErrConnDone},
	}, []mockExecResult{
		{match: "DELETE FROM concurrency_keys WHERE key_hash", affected: 0},
		{match: "INSERT IGNORE INTO concurrency_keys", affected: 1},
	})
	_, err := store.AcquireConcurrencyKey(testCtx, "my-key", "wf-1", 30*time.Second)
	if err == nil {
		t.Fatal("expected error from verify failure")
	}
}

func TestMySQLStore_ReleaseConcurrencyKey(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys WHERE key_hash", affected: 1},
	})
	err := store.ReleaseConcurrencyKey(testCtx, "my-key")
	if err != nil {
		t.Fatalf("ReleaseConcurrencyKey: %v", err)
	}
}

func TestMySQLStore_ReapExpiredConcurrencyKeys(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE FROM concurrency_keys WHERE expires_at", affected: 5},
	})
	n, err := store.ReapExpiredConcurrencyKeys(testCtx)
	if err != nil {
		t.Fatalf("ReapExpiredConcurrencyKeys: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

func TestMySQLStore_CreateSchedule(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_schedules", affected: 1},
	})
	sch := Schedule{
		Name:           "daily",
		DefName:        "backup",
		EntryPoint:     "main",
		CronExpression: "0 2 * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC),
	}
	err := store.CreateSchedule(testCtx, sch)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
}

func TestMySQLStore_ListSchedules_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	scheds, err := store.ListSchedules(testCtx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(scheds) != 0 {
		t.Errorf("expected empty, got %d", len(scheds))
	}
}

func TestMySQLStore_ListSchedules_WithRows(t *testing.T) {
	nextRunAt := time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC)
	lastRunAt := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT name, def_name, entry_point",
			data: [][]driver.Value{
				{"sched-1", "wf-a", "main", "0 2 * * *", []byte(`{}`), true, nextRunAt, lastRunAt, "UTC", "00000000-0000-0000-0000-000000000000"},
				{"sched-2", "wf-b", "handler", "*/5 * * * *", []byte(`{"x":1}`), false, nextRunAt, nil, "America/New_York", "33333333-3333-3333-3333-333333333333"},
			},
		},
	}, nil)
	scheds, err := store.ListSchedules(testCtx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(scheds) != 2 {
		t.Fatalf("expected 2, got %d", len(scheds))
	}
	if scheds[0].Name != "sched-1" || !scheds[0].Enabled {
		t.Errorf("unexpected first: %+v", scheds[0])
	}
	if scheds[1].LastRunAt != nil {
		t.Error("expected nil LastRunAt")
	}
	// Different zones per row on purpose: a Scan that dropped the column
	// would leave both empty and still return two schedules.
	if scheds[0].Timezone != "UTC" {
		t.Errorf("first schedule timezone = %q, want UTC", scheds[0].Timezone)
	}
	if scheds[1].Timezone != "America/New_York" {
		t.Errorf("second schedule timezone = %q, want America/New_York", scheds[1].Timezone)
	}
}

func TestMySQLStore_DeleteSchedule(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_schedules", affected: 1},
	})
	err := store.DeleteSchedule(testCtx, "daily")
	if err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
}

func TestMySQLStore_SetScheduleEnabled(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_schedules SET enabled", affected: 1},
	})
	err := store.SetScheduleEnabled(testCtx, "daily", false)
	if err != nil {
		t.Fatalf("SetScheduleEnabled: %v", err)
	}
}

func TestMySQLStore_GetDueSchedules_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	scheds, err := store.GetDueSchedules(testCtx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(scheds) != 0 {
		t.Errorf("expected empty, got %d", len(scheds))
	}
}

func TestMySQLStore_GetDueSchedules_WithRows(t *testing.T) {
	nextRunAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT name, def_name, entry_point",
			data: [][]driver.Value{
				{"due-sched", "wf-a", "main", "0 2 * * *", []byte(`{}`), true, nextRunAt, nil, "Asia/Tokyo", "33333333-3333-3333-3333-333333333333"},
			},
		},
	}, nil)
	scheds, err := store.GetDueSchedules(testCtx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	if len(scheds) != 1 || scheds[0].Name != "due-sched" {
		t.Fatalf("unexpected: %+v", scheds)
	}
	// The scheduler computes the next firing from this field; without it
	// every schedule silently reverts to the UTC wall clock.
	if scheds[0].Timezone != "Asia/Tokyo" {
		t.Errorf("timezone = %q, want Asia/Tokyo", scheds[0].Timezone)
	}
	if scheds[0].TenantID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("tenant = %q, want 33333333-3333-3333-3333-333333333333", scheds[0].TenantID)
	}
}

func TestMySQLStore_GetDueSchedules_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT name, def_name, entry_point", err: sql.ErrConnDone},
	}, nil)
	_, err := store.GetDueSchedules(testCtx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMySQLStore_UpdateScheduleNextRun(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_schedules SET next_run_at", affected: 1},
	})
	err := store.UpdateScheduleNextRun(testCtx, "daily", time.Date(2025, 1, 2, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("UpdateScheduleNextRun: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

func TestMySQLStore_GetCompactionCandidates_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	candidates, err := store.GetCompactionCandidates(testCtx, 100, 10)
	if err != nil {
		t.Fatalf("GetCompactionCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected empty, got %d", len(candidates))
	}
}

func TestMySQLStore_GetCompactionCandidates_WithRows(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT w.id",
			data:  [][]driver.Value{{"wf-1"}, {"wf-2"}},
		},
	}, nil)
	candidates, err := store.GetCompactionCandidates(testCtx, 100, 10)
	if err != nil {
		t.Fatalf("GetCompactionCandidates: %v", err)
	}
	if len(candidates) != 2 || candidates[0] != "wf-1" || candidates[1] != "wf-2" {
		t.Errorf("unexpected: %v", candidates)
	}
}

func TestMySQLStore_LoadCompactionState_Found(t *testing.T) {
	csJSON := []byte(`{"version":1,"compacted_step":50}`)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT compaction_state, compaction_step FROM workflow_instances", csJSON, int64(50)),
	}, nil)
	cs, err := store.LoadCompactionState(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs == nil || cs.Version != 1 || cs.CompactedStep != 50 {
		t.Errorf("unexpected: %+v", cs)
	}
}

func TestMySQLStore_LoadCompactionState_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	cs, err := store.LoadCompactionState(testCtx, "nonexistent")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs != nil {
		t.Error("expected nil for not found")
	}
}

func TestMySQLStore_LoadCompactionState_NullState(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT compaction_state, compaction_step FROM workflow_instances", nil, nil),
	}, nil)
	cs, err := store.LoadCompactionState(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs != nil {
		t.Error("expected nil for null state")
	}
}

func TestMySQLStore_LoadCompactionState_InvalidJSON(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT compaction_state, compaction_step FROM workflow_instances", []byte("invalid"), int64(0)),
	}, nil)
	_, err := store.LoadCompactionState(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestMySQLStore_LoadCompactionState_QueryError(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT compaction_state, compaction_step FROM workflow_instances", err: sql.ErrConnDone},
	}, nil)
	_, err := store.LoadCompactionState(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected query error")
	}
}

func TestMySQLStore_CompactHistory_Success(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT generation FROM workflow_instances WHERE id = ? AND tenant_id = ?", int64(1)),
	}, []mockExecResult{
		{match: "DELETE FROM event_history WHERE workflow_id", affected: 10},
		{match: "UPDATE workflow_instances", affected: 1},
	})
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 50, 50)
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
}

func TestMySQLStore_CompactHistory_WorkflowGone(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 50, 50)
	if err != nil {
		t.Fatalf("CompactHistory (gone): %v", err)
	}
}

func TestMySQLStore_CompactHistory_GenerationError(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT generation FROM workflow_instances WHERE id = ? AND tenant_id = ?", err: sql.ErrConnDone},
	}, nil)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 50, 50)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMySQLStore_CompactHistory_DeleteError(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT generation FROM workflow_instances WHERE id = ? AND tenant_id = ?", int64(1)),
	}, []mockExecResult{
		{match: "DELETE FROM event_history WHERE workflow_id", err: sql.ErrConnDone},
	})
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 50, 50)
	if err == nil {
		t.Fatal("expected delete error")
	}
}

func TestMySQLStore_CompactHistory_UpdateError(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT generation FROM workflow_instances WHERE id = ? AND tenant_id = ?", int64(1)),
	}, []mockExecResult{
		{match: "DELETE FROM event_history WHERE workflow_id", affected: 1},
		{match: "UPDATE workflow_instances", err: sql.ErrConnDone},
	})
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 50, 50)
	if err == nil {
		t.Fatal("expected update error")
	}
}

func TestMySQLStore_CompactHistory_CommitError(t *testing.T) {
	db := newMockDBWithErrors(t,
		[]mockRowsResult{queryRowOk("SELECT generation FROM workflow_instances WHERE id = ? AND tenant_id = ?", int64(1))},
		[]mockExecResult{
			{match: "DELETE FROM event_history WHERE workflow_id", affected: 1},
			{match: "UPDATE workflow_instances", affected: 1},
		},
		nil, errors.New("commit failed"),
	)
	defer db.Close()
	store := NewMySQLStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 50, 50)
	if err == nil {
		t.Fatal("expected commit error")
	}
}

func TestMySQLStore_CompactHistory_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()
	store := NewMySQLStore(db)
	err := store.CompactHistory(testCtx, "wf-1", []byte(`{}`), 50, 50)
	if err == nil {
		t.Fatal("expected begin error")
	}
}

// ---------------------------------------------------------------------------
// ListWorkflows
// ---------------------------------------------------------------------------

// testWorkflowRow returns a mock row for ListWorkflows/GetWorkflowByID.
func testWorkflowRow(id, name string, version int64, status string, assignedTo string) [][]driver.Value {
	nextWakeAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	return [][]driver.Value{{
		id, name, version, status, []byte(`{"in":1}`), assignedTo,
		nextWakeAt, nil, nil, nil, nil, int64(0), int64(0), "",
	}}
}

func TestMySQLStore_ListWorkflows_All(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: fmt.Sprintf("SELECT %s FROM workflow_instances WHERE tenant_id = ?", DialectMySQL.workflowInstanceColumns()), data: testWorkflowRow("wf-1", "test-wf", 1, "running", "")},
	}, nil)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 || wfs[0].ID != "wf-1" {
		t.Errorf("unexpected: %+v", wfs)
	}
}

func TestMySQLStore_ListWorkflows_WithStatus(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT id, def_name, def_version", data: testWorkflowRow("wf-1", "test-wf", 1, "running", "")},
	}, nil)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Status: "running", Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 || wfs[0].Status != "running" {
		t.Errorf("unexpected: %+v", wfs)
	}
}

func TestMySQLStore_ListWorkflows_WithLimitOffset(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT id, def_name, def_version", data: testWorkflowRow("wf-1", "test-wf", 1, "running", "")},
	}, nil)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 10, Offset: 5})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Errorf("expected 1, got %d", len(wfs))
	}
}

func TestMySQLStore_ListWorkflows_DefaultLimit(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT id, def_name, def_version", data: testWorkflowRow("wf-1", "test-wf", 1, "running", "")},
	}, nil)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Errorf("expected 1, got %d", len(wfs))
	}
}

func TestMySQLStore_ListWorkflows_ClampLimit(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT id, def_name, def_version", data: testWorkflowRow("wf-1", "test-wf", 1, "running", "")},
	}, nil)
	wfs, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 2000})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Errorf("expected 1, got %d", len(wfs))
	}
}

func TestMySQLStore_ListWorkflows_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT id, def_name, def_version", err: sql.ErrConnDone},
	}, nil)
	_, err := store.ListWorkflows(testCtx, WorkflowFilter{Limit: 10})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// GetWorkflowByID
// ---------------------------------------------------------------------------

func TestMySQLStore_GetWorkflowByID_Found(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT id, def_name, def_version, status, input",
			"wf-1", "test-wf", int64(1), "done", []byte(`{"input":"data"}`),
			"worker-1", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC),
			`{"result":"ok"}`, "", nil, nil, int64(0), int64(0), "", "tenant-1",
		),
	}, nil)
	wf, err := store.GetWorkflowByID(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil || wf.ID != "wf-1" || wf.TraceID != "" {
		t.Errorf("unexpected: %+v", wf)
	}
}

func TestMySQLStore_GetWorkflowByID_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	wf, err := store.GetWorkflowByID(testCtx, "nonexistent")
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf != nil {
		t.Error("expected nil")
	}
}

func TestMySQLStore_GetWorkflowByID_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT id, def_name, def_version", err: sql.ErrConnDone},
	}, nil)
	_, err := store.GetWorkflowByID(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// Workflow Definitions
// ---------------------------------------------------------------------------

func TestMySQLStore_LoadWASM_Found(t *testing.T) {
	expected := []byte("some-wasm-bytes")
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT wasm_bytes FROM workflow_defs", expected),
	}, nil)
	wasm, err := store.LoadWASM(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadWASM: %v", err)
	}
	if string(wasm) != string(expected) {
		t.Errorf("expected %q, got %q", expected, wasm)
	}
}

func TestMySQLStore_LoadWASM_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	_, err := store.LoadWASM(testCtx, "nonexistent", 1)
	if err == nil || !strings.Contains(err.Error(), "wasm not found") {
		t.Errorf("expected 'wasm not found' error, got: %v", err)
	}
}

func TestMySQLStore_GetWASMLength(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT LENGTH(wasm_bytes) FROM workflow_defs", int64(4096)),
	}, nil)
	length, err := store.GetWASMLength(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("GetWASMLength: %v", err)
	}
	if length != 4096 {
		t.Errorf("expected 4096, got %d", length)
	}
}

func TestMySQLStore_GetWASMLength_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT LENGTH(wasm_bytes) FROM workflow_defs", err: sql.ErrConnDone},
	}, nil)
	_, err := store.GetWASMLength(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMySQLStore_ListVersions_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	versions, err := store.ListVersions(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("expected empty, got %d", len(versions))
	}
}

func TestMySQLStore_ListVersions_WithRows(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT version FROM workflow_defs",
			data:  [][]driver.Value{{int64(3)}, {int64(2)}, {int64(1)}},
		},
	}, nil)
	versions, err := store.ListVersions(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 || versions[0] != 3 {
		t.Errorf("unexpected: %v", versions)
	}
}

func TestMySQLStore_LoadWorkflowConfig_Found(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT max_history_length FROM workflow_defs", int64(500)),
	}, nil)
	maxHist, err := store.LoadWorkflowConfig(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadWorkflowConfig: %v", err)
	}
	if maxHist != 500 {
		t.Errorf("expected 500, got %d", maxHist)
	}
}

func TestMySQLStore_LoadWorkflowConfig_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	_, err := store.LoadWorkflowConfig(testCtx, "nonexistent", 1)
	if err == nil || !strings.Contains(err.Error(), "workflow def not found") {
		t.Errorf("expected 'workflow def not found', got: %v", err)
	}
}

func TestMySQLStore_LoadDAGSpec_Found(t *testing.T) {
	specJSON := json.RawMessage(`{"steps":["a","b"]}`)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT dag_spec FROM workflow_defs", []byte(specJSON)),
	}, nil)
	spec, err := store.LoadDAGSpec(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("LoadDAGSpec: %v", err)
	}
	if string(spec) != string(specJSON) {
		t.Errorf("expected %q, got %q", specJSON, spec)
	}
}

func TestMySQLStore_LoadDAGSpec_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	_, err := store.LoadDAGSpec(testCtx, "nonexistent", 1)
	if err == nil || !strings.Contains(err.Error(), "workflow def not found") {
		t.Errorf("expected 'workflow def not found', got: %v", err)
	}
}

func TestMySQLStore_TraceWorkflow(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances SET trace_id", affected: 1},
	})
	err := store.TraceWorkflow(testCtx, "wf-1", "trace-abc")
	if err != nil {
		t.Fatalf("TraceWorkflow: %v", err)
	}
}

func TestMySQLStore_DeployWorkflowDef(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_defs", affected: 1},
	})
	def := &WorkflowDef{Name: "wf", Version: 1, WASMBytes: []byte("wasm"), ABIVersion: 1}
	err := store.DeployWorkflowDef(testCtx, def)
	if err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
}

func TestMySQLStore_DeployWorkflowDef_NilPluginDeps(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_defs", affected: 1},
	})
	def := &WorkflowDef{Name: "wf", Version: 1, WASMBytes: []byte("wasm"), ABIVersion: 1, PluginDeps: nil}
	err := store.DeployWorkflowDef(testCtx, def)
	if err != nil {
		t.Fatalf("DeployWorkflowDef (nil deps): %v", err)
	}
}

func TestMySQLStore_DeployWorkflowDef_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_defs", err: sql.ErrConnDone},
	})
	def := &WorkflowDef{Name: "wf", Version: 1}
	err := store.DeployWorkflowDef(testCtx, def)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMySQLStore_ListWorkflowDefs_All(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT name, version, abi_version",
			data: [][]driver.Value{
				{"wf-a", int64(2), int64(1), int64(0), []byte(`{}`), createdAt, false},
			},
		},
	}, nil)
	defs, err := store.ListWorkflowDefs(testCtx, "")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "wf-a" || defs[0].Version != 2 {
		t.Errorf("unexpected: %+v", defs[0])
	}
}

func TestMySQLStore_ListWorkflowDefs_ByName(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT name, version, abi_version",
			data: [][]driver.Value{
				{"wf-a", int64(1), int64(1), int64(0), []byte(`{}`), createdAt, false},
			},
		},
	}, nil)
	defs, err := store.ListWorkflowDefs(testCtx, "wf-a")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(defs) != 1 || defs[0].PluginDeps == nil {
		t.Errorf("unexpected: %+v", defs[0])
	}
}

func TestMySQLStore_GetWorkflowDef_Found(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT name, version, wasm_bytes",
			"test-wf", int64(2), []byte("wasm-data"), int64(1), int64(0),
			[]byte(`{"p":"1.0"}`), createdAt, false,
		),
	}, nil)
	def, err := store.GetWorkflowDef(testCtx, "test-wf", 2)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def == nil || def.Name != "test-wf" || def.Version != 2 || def.PluginDeps["p"] != "1.0" {
		t.Errorf("unexpected: %+v", def)
	}
}

func TestMySQLStore_GetWorkflowDef_NotFound(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	def, err := store.GetWorkflowDef(testCtx, "nonexistent", 999)
	if err != nil {
		t.Fatalf("GetWorkflowDef: %v", err)
	}
	if def != nil {
		t.Error("expected nil")
	}
}

func TestMySQLStore_MarkVersionDeprecated(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_defs SET deprecated", affected: 1},
	})
	err := store.MarkVersionDeprecated(testCtx, "wf", 1, true)
	if err != nil {
		t.Fatalf("MarkVersionDeprecated: %v", err)
	}
}

func TestMySQLStore_PurgeWorkflowDef(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE FROM workflow_defs", affected: 1},
	})
	err := store.PurgeWorkflowDef(testCtx, "wf", 1)
	if err != nil {
		t.Fatalf("PurgeWorkflowDef: %v", err)
	}
}

func TestMySQLStore_CountActiveInstances(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT COUNT(*) FROM workflow_instances", int64(7)),
	}, nil)
	count, err := store.CountActiveInstances(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("CountActiveInstances: %v", err)
	}
	if count != 7 {
		t.Errorf("expected 7, got %d", count)
	}
}

func TestMySQLStore_ResolveLatestVersion(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT COALESCE", int64(3)),
	}, nil)
	version, err := store.ResolveLatestVersion(testCtx, "test-wf")
	if err != nil {
		t.Fatalf("ResolveLatestVersion: %v", err)
	}
	if version != 3 {
		t.Errorf("expected 3, got %d", version)
	}
}

func TestMySQLStore_ResolveLatestVersion_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT COALESCE", err: sql.ErrConnDone},
	}, nil)
	_, err := store.ResolveLatestVersion(testCtx, "test-wf")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMySQLStore_ValidateVersion_Valid(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT COUNT(*) FROM workflow_defs", int64(1)),
	}, nil)
	valid, err := store.ValidateVersion(testCtx, "test-wf", 1)
	if err != nil {
		t.Fatalf("ValidateVersion: %v", err)
	}
	if !valid {
		t.Error("expected valid=true")
	}
}

func TestMySQLStore_ValidateVersion_Invalid(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT COUNT(*) FROM workflow_defs", int64(0)),
	}, nil)
	valid, err := store.ValidateVersion(testCtx, "test-wf", 999)
	if err != nil {
		t.Fatalf("ValidateVersion: %v", err)
	}
	if valid {
		t.Error("expected valid=false")
	}
}

func TestMySQLStore_ValidateVersion_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT COUNT(*) FROM workflow_defs", err: sql.ErrConnDone},
	}, nil)
	_, err := store.ValidateVersion(testCtx, "test-wf", 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMySQLStore_GetActiveInstanceCountsByVersion(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT def_name, def_version, COUNT(*) as cnt",
			data: [][]driver.Value{
				{"test-wf", int64(1), int64(5)},
				{"other-wf", int64(2), int64(3)},
			},
		},
	}, nil)
	counts, err := store.GetActiveInstanceCountsByVersion(testCtx)
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if counts["test-wf:1"] != 5 || counts["other-wf:2"] != 3 {
		t.Errorf("unexpected: %v", counts)
	}
}

// ---------------------------------------------------------------------------
// Memory Stats
// ---------------------------------------------------------------------------

func TestMySQLStore_RecordWorkflowMemorySample(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "INSERT INTO workflow_memory_samples (def_name, sample_bytes)", affected: 1},
		{match: "INSERT INTO workflow_memory_stats", affected: 1},
	})
	err := store.RecordWorkflowMemorySample(testCtx, "wf-def", int64(4096))
	if err != nil {
		t.Fatalf("RecordWorkflowMemorySample: %v", err)
	}
}

func TestMySQLStore_RecordWorkflowMemorySample_BeginError(t *testing.T) {
	db := newMockDBWithErrors(t, nil, nil, errors.New("begin failed"), nil)
	defer db.Close()
	store := NewMySQLStore(db)
	err := store.RecordWorkflowMemorySample(testCtx, "wf-def", int64(4096))
	if err == nil {
		t.Fatal("expected begin error")
	}
}

func TestMySQLStore_LoadMemoryEstimates_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	estimates, err := store.LoadMemoryEstimates(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryEstimates: %v", err)
	}
	if len(estimates) != 0 {
		t.Errorf("expected empty, got %d", len(estimates))
	}
}

func TestMySQLStore_LoadMemoryEstimates_WithRows(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT def_name, mean_bytes",
			data:  [][]driver.Value{{"wf-a", float64(4096.5)}, {"wf-b", float64(8192.0)}},
		},
	}, nil)
	estimates, err := store.LoadMemoryEstimates(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryEstimates: %v", err)
	}
	if estimates["wf-a"] != 4096.5 || estimates["wf-b"] != 8192.0 {
		t.Errorf("unexpected: %v", estimates)
	}
}

func TestMySQLStore_LoadMemoryStats_Empty(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	stats, err := store.LoadMemoryStats(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty, got %d", len(stats))
	}
}

func TestMySQLStore_LoadMemoryStats_WithData(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT def_name, sample_bytes",
			data: [][]driver.Value{
				{"wf-a", int64(100)}, {"wf-a", int64(200)}, {"wf-a", int64(300)},
			},
		},
	}, nil)
	stats, err := store.LoadMemoryStats(testCtx)
	if err != nil {
		t.Fatalf("LoadMemoryStats: %v", err)
	}
	if len(stats) != 1 || stats[0].DefName != "wf-a" || stats[0].SampleCount != 3 {
		t.Errorf("unexpected: %+v", stats[0])
	}
}

func TestMySQLStore_LoadMemoryStats_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT def_name, sample_bytes", err: sql.ErrConnDone},
	}, nil)
	_, err := store.LoadMemoryStats(testCtx)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// Maintenance
// ---------------------------------------------------------------------------

func TestMySQLStore_QueueDepth(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT COUNT(*) FROM workflow_instances WHERE status = 'ready'", int64(42)),
	}, nil)
	depth, err := store.QueueDepth(testCtx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 42 {
		t.Errorf("expected 42, got %d", depth)
	}
}

func TestMySQLStore_TerminateWorkflow(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "UPDATE workflow_instances", affected: 1},
	})
	err := store.TerminateWorkflow(testCtx, "wf-1", "manual termination")
	if err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}
}

func TestMySQLStore_GetChildCount(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT COUNT(*) FROM workflow_instances", int64(3)),
	}, nil)
	count, err := store.GetChildCount(testCtx, "parent-wf")
	if err != nil {
		t.Fatalf("GetChildCount: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestMySQLStore_GetConcurrencyKeyCount(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT COUNT(*) FROM concurrency_keys", int64(2)),
	}, nil)
	count, err := store.GetConcurrencyKeyCount(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

func TestMySQLStore_GetEventCount(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT event_count FROM workflow_instances", int64(100)),
	}, nil)
	count, err := store.GetEventCount(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetEventCount: %v", err)
	}
	if count != 100 {
		t.Errorf("expected 100, got %d", count)
	}
}

func TestMySQLStore_CleanupMemorySamples_NoDefs(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, nil)
	n, err := store.CleanupMemorySamples(testCtx, 100)
	if err != nil {
		t.Fatalf("CleanupMemorySamples: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestMySQLStore_CleanupMemorySamples_WithDefs(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{
			match: "SELECT DISTINCT def_name FROM workflow_memory_samples",
			data:  [][]driver.Value{{"wf-a"}, {"wf-b"}},
		},
	}, []mockExecResult{
		{match: "DELETE FROM workflow_memory_samples", affected: 3},
	})
	n, err := store.CleanupMemorySamples(testCtx, 100)
	if err != nil {
		t.Fatalf("CleanupMemorySamples: %v", err)
	}
	if n != 6 {
		t.Errorf("expected 6 (3 per def * 2 defs), got %d", n)
	}
}

func TestMySQLStore_CleanupMemorySamples_ScanError(t *testing.T) {
	// First query (distinct def_names) returns error.
	store := newMySQLStoreForTest(t, []mockRowsResult{
		{match: "SELECT DISTINCT def_name FROM workflow_memory_samples", err: sql.ErrConnDone},
	}, nil)
	_, err := store.CleanupMemorySamples(testCtx, 100)
	if err == nil {
		t.Fatal("expected query error")
	}
}

func TestMySQLStore_DeleteExpiredEvents_FirstBatch(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE e FROM event_history e", affected: 0},
		{match: "UPDATE workflow_instances", affected: 0},
	})
	n, err := store.DeleteExpiredEvents(testCtx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteExpiredEvents: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestMySQLStore_DeleteDeadLetteredWorkflows_FirstBatch(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE w FROM workflow_instances", affected: 0},
	})
	n, err := store.DeleteDeadLetteredWorkflows(testCtx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteDeadLetteredWorkflows: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestMySQLStore_DeleteDeadLetteredWorkflows_Error(t *testing.T) {
	store := newMySQLStoreForTest(t, nil, []mockExecResult{
		{match: "DELETE w FROM workflow_instances", err: sql.ErrConnDone},
	})
	_, err := store.DeleteDeadLetteredWorkflows(testCtx, time.Now().Add(-30*24*time.Hour))
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// GetWorkflowByID — null optionals
// ---------------------------------------------------------------------------

func TestMySQLStore_GetWorkflowByID_NullOptionals(t *testing.T) {
	store := newMySQLStoreForTest(t, []mockRowsResult{
		queryRowOk("SELECT id, def_name, def_version, status, input",
			"wf-1", "test-wf", int64(1), "running", []byte(`{}`),
			nil, nil, nil, nil,
			nil, nil, nil, nil,
			int64(0), int64(0), "", "tenant-1",
		),
	}, nil)
	wf, err := store.GetWorkflowByID(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("GetWorkflowByID (null): %v", err)
	}
	if wf == nil {
		t.Fatal("expected non-nil")
	}
	if wf.AssignedTo != "" {
		t.Errorf("expected empty assigned_to, got %q", wf.AssignedTo)
	}
}
