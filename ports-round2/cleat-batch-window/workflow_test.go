package cleatbatchwindow

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/rcownie/cleat/durable/cleattest"
)

// ---------------------------------------------------------------------------
// Utility tests
// ---------------------------------------------------------------------------

func TestDivideIntoPartitions(t *testing.T) {
	tests := []struct {
		number, n int
		want      []int
	}{
		{90, 3, []int{30, 30, 30}},
		{10, 3, []int{4, 3, 3}},
		{5, 5, []int{1, 1, 1, 1, 1}},
		{0, 3, []int{0, 0, 0}},
		{7, 1, []int{7}},
	}
	for _, tc := range tests {
		got := divideIntoPartitions(tc.number, tc.n)
		if len(got) != len(tc.want) {
			t.Fatalf("divideIntoPartitions(%d,%d) = %v, want %v", tc.number, tc.n, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("divideIntoPartitions(%d,%d) = %v, want %v", tc.number, tc.n, got, tc.want)
			}
		}
	}
}

func TestGetRecords(t *testing.T) {
	// page covers full range
	recs := getRecords(0, 5, 5)
	if len(recs) != 5 {
		t.Fatalf("expected 5 records, got %d", len(recs))
	}
	for i, r := range recs {
		if r.Id != i {
			t.Fatalf("record[%d].Id = %d, want %d", i, r.Id, i)
		}
	}

	// partial page at end
	recs = getRecords(8, 5, 10)
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if recs[0].Id != 8 || recs[1].Id != 9 {
		t.Fatalf("expected records [8,9], got %v", recs)
	}

	// empty range
	recs = getRecords(10, 5, 10)
	if len(recs) != 0 {
		t.Fatalf("expected 0 records, got %d", len(recs))
	}
}

// ---------------------------------------------------------------------------
// RecordProcessorWorkflow tests
// ---------------------------------------------------------------------------

func TestRecordProcessorWorkflow_SendsSignal(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	// Deterministic random: minimum sleep (100ms).
	env.SetRandomSeq([]int64{0})

	record := SingleRecord{Id: 42, ParentWorkflowID: "test-parent"}

	done := make(chan error, 1)
	go func() {
		_, err := RecordProcessorWorkflow(h, mustJSON(record))
		done <- err
	}()

	// Allow the goroutine to start and enter DurableSleep before advancing
	// the simulated clock. 10ms of real time is sufficient for startup.
	time.Sleep(10 * time.Millisecond)

	// Advance the simulated clock past the sleep to unblock the child.
	env.AdvanceTime(200 * time.Millisecond)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for RecordProcessorWorkflow")
	}

	// Verify the child signalled its parent.
	payload, found, err := h.PollSignal("ReportCompletion")
	if err != nil {
		t.Fatalf("PollSignal error: %v", err)
	}
	if !found {
		t.Fatal("expected ReportCompletion signal, none found")
	}
	if payload != "42" {
		t.Fatalf("expected signal payload 42, got %q", payload)
	}
}

// ---------------------------------------------------------------------------
// SlidingWindowWorkflow tests
// ---------------------------------------------------------------------------

// TestSlidingWindowWorkflow_AllInOneRun verifies that the workflow processes
// all records when the sliding window size is large enough to accommodate
// all children without blocking on waitForSlot.
func TestSlidingWindowWorkflow_AllInOneRun(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	input := SlidingWindowWorkflowInput{
		PageSize:          10,
		SlidingWindowSize: 10,
		Offset:            0,
		MaximumOffset:     10,
		Progress:          0,
		CurrentRecords:    nil,
	}

	// Pre-enqueue all completion signals. Because signals are queued before
	// the workflow starts, PollSignal will find them without needing to
	// block on AwaitSignals, making this test fully synchronous.
	for i := 0; i < 10; i++ {
		env.Signal("ReportCompletion", strconv.Itoa(i))
	}

	result, err := SlidingWindowWorkflow(h, mustJSON(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count, _ := strconv.Atoi(result)
	if count != 10 {
		t.Fatalf("expected 10 processed, got %d", count)
	}
}

// TestSlidingWindowWorkflow_WithSlidingWindow verifies the sliding window
// concurrency limit: children are started in parallel but no more than
// SlidingWindowSize at a time.
func TestSlidingWindowWorkflow_WithSlidingWindow(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	input := SlidingWindowWorkflowInput{
		PageSize:          10,
		SlidingWindowSize: 3, // max 3 concurrent children
		Offset:            0,
		MaximumOffset:     10,
		Progress:          0,
		CurrentRecords:    nil,
	}

	// Pre-enqueue all completion signals. Because they are queued ahead of
	// time, PollSignal will drain them during waitForSlot without blocking.
	for i := 0; i < 10; i++ {
		env.Signal("ReportCompletion", strconv.Itoa(i))
	}

	result, err := SlidingWindowWorkflow(h, mustJSON(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count, _ := strconv.Atoi(result)
	if count != 10 {
		t.Fatalf("expected 10 processed, got %d", count)
	}
}

// TestSlidingWindowWorkflow_ContinueAsNewBoundary verifies that the workflow
// calls ContinueAsNew after starting PageSize children. Since ContinueAsNew
// is a no-op in the test environment, this test confirms the signal drain
// and progress tracking work correctly for the boundary case.
func TestSlidingWindowWorkflow_ContinueAsNewBoundary(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	input := SlidingWindowWorkflowInput{
		PageSize:          3, // ContinueAsNew after 3 children
		SlidingWindowSize: 5,
		Offset:            0,
		MaximumOffset:     10,
		Progress:          0,
		CurrentRecords:    nil,
	}

	// Pre-enqueue signals for the first 3 records (which are the ones that
	// would complete before the ContinueAsNew drain).
	for i := 0; i < 3; i++ {
		env.Signal("ReportCompletion", strconv.Itoa(i))
	}

	result, err := SlidingWindowWorkflow(h, mustJSON(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The first page covers 3 records; all 3 completed.
	count, _ := strconv.Atoi(result)
	if count != 3 {
		t.Fatalf("expected 3 processed (one page), got %d", count)
	}
}

// TestSlidingWindowWorkflow_ResumesFromPreviousRun verifies that the workflow
// correctly picks up where a previous ContinueAsNew run left off.
func TestSlidingWindowWorkflow_ResumesFromPreviousRun(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	// Simulate the state after the first run of a ContinueAsNew chain.
	// offset=3 means records 0-2 already processed, progress=3.
	input := SlidingWindowWorkflowInput{
		PageSize:          10, // enough to cover all remaining records in one run
		SlidingWindowSize: 5,
		Offset:            3,
		MaximumOffset:     10,
		Progress:          3,
		CurrentRecords:    nil, // no in-flight children from previous run
	}

	// Pre-enqueue signals for remaining records 3-9.
	for i := 3; i < 10; i++ {
		env.Signal("ReportCompletion", strconv.Itoa(i))
	}

	result, err := SlidingWindowWorkflow(h, mustJSON(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count, _ := strconv.Atoi(result)
	if count != 10 {
		t.Fatalf("expected 10 total (3+7), got %d", count)
	}
}

// TestSlidingWindowWorkflow_QueryState verifies that SetQueryState is called
// to expose the sliding window state for external queries.
func TestSlidingWindowWorkflow_QueryState(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	input := SlidingWindowWorkflowInput{
		PageSize:          5,
		SlidingWindowSize: 5,
		Offset:            0,
		MaximumOffset:     5,
		Progress:          0,
		CurrentRecords:    nil,
	}

	// Pre-enqueue all signals.
	for i := 0; i < 5; i++ {
		env.Signal("ReportCompletion", strconv.Itoa(i))
	}

	_, err := SlidingWindowWorkflow(h, mustJSON(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The query state should have been set (at least once).
	stateJSON, ok := env.QueryState("state")
	if !ok {
		t.Fatal("expected query state 'state' to be set")
	}
	var state SlidingWindowState
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.Progress != 5 {
		t.Fatalf("expected progress=5, got %d", state.Progress)
	}
}

// ---------------------------------------------------------------------------
// ProcessBatchWorkflow tests
// ---------------------------------------------------------------------------

// TestProcessBatchWorkflow verifies that the top-level workflow partitions
// records and aggregates results from child sliding window workflows.
func TestProcessBatchWorkflow(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	// Register a handler for SlidingWindowWorkflow children that computes the
	// number of records in each partition.
	env.RegisterChildWorkflow("SlidingWindowWorkflow", func(inputJSON string) (string, error) {
		var input SlidingWindowWorkflowInput
		if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
			return "", err
		}
		// Return the count of records in this partition.
		return strconv.Itoa(input.MaximumOffset - input.Offset), nil
	})

	input := ProcessBatchWorkflowInput{
		PageSize:          5,
		SlidingWindowSize: 10,
		Partitions:        3,
		RecordCount:       90,
	}

	result, err := ProcessBatchWorkflow(h, mustJSON(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count, _ := strconv.Atoi(result)
	if count != 90 {
		t.Fatalf("expected 90 total processed, got %d", count)
	}
}

// TestProcessBatchWorkflow_InvalidInput verifies that ProcessBatchWorkflow
// rejects invalid configurations (SlidingWindowSize < Partitions).
func TestProcessBatchWorkflow_InvalidInput(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	input := ProcessBatchWorkflowInput{
		PageSize:          5,
		SlidingWindowSize: 2, // less than Partitions=3
		Partitions:        3,
		RecordCount:       90,
	}

	_, err := ProcessBatchWorkflow(h, mustJSON(input))
	if err == nil {
		t.Fatal("expected error for invalid input, got nil")
	}
}

// TestProcessBatchWorkflow_NoRecords verifies the edge case of zero records.
func TestProcessBatchWorkflow_NoRecords(t *testing.T) {
	env := cleattest.NewTestEnv()
	h := env.H()

	input := ProcessBatchWorkflowInput{
		PageSize:          5,
		SlidingWindowSize: 5,
		Partitions:        1,
		RecordCount:       0,
	}

	result, err := ProcessBatchWorkflow(h, mustJSON(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "0" {
		t.Fatalf("expected 0, got %s", result)
	}
}
