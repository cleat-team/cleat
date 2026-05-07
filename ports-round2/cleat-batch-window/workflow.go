// Package cleatbatchwindow ports the Temporal batch-sliding-window sample to the
// Cleat durable execution Go SDK. It demonstrates batch processing with a sliding
// window of child workflows, continue-as-new for pagination, and signal-based
// coordination between child and parent workflows.
//
// Source: samples-go/batch-sliding-window/ (Temporal Go SDK sample)
package cleatbatchwindow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rcownie/durable/durable"
)

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

// ProcessBatchWorkflowInput is the top-level input for the batch workflow.
type ProcessBatchWorkflowInput struct {
	PageSize          int `json:"page_size"`
	SlidingWindowSize int `json:"sliding_window_size"`
	Partitions        int `json:"partitions"`
	RecordCount       int `json:"record_count"`
}

// SlidingWindowWorkflowInput is the input for each sliding window partition.
type SlidingWindowWorkflowInput struct {
	PageSize          int          `json:"page_size"`
	SlidingWindowSize int          `json:"sliding_window_size"`
	Offset            int          `json:"offset"`             // inclusive start
	MaximumOffset     int          `json:"maximum_offset"`     // exclusive end
	Progress          int          `json:"progress"`           // records completed so far
	CurrentRecords    map[int]bool `json:"current_records"`    // record IDs still in-flight
}

// SingleRecord holds the data for one record to process.
// ParentWorkflowID is injected so the child can signal its parent.
type SingleRecord struct {
	Id              int    `json:"id"`
	ParentWorkflowID string `json:"parent_wf_id"`
}

// SlidingWindowState is the state exposed via SetQueryState.
type SlidingWindowState struct {
	CurrentRecords []int `json:"current_records"`
	Offset         int   `json:"offset"`
	Progress       int   `json:"progress"`
}

// ---- helpers ----

func mustJSON(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic("cleatbatchwindow: json marshal failed: " + err.Error())
	}
	return string(data)
}

// getRecords simulates loading records from an external data source.
// Returns up to pageSize records starting at offset.
func getRecords(offset, pageSize, maxOffset int) []SingleRecord {
	limit := offset + pageSize
	if limit > maxOffset {
		limit = maxOffset
	}
	records := make([]SingleRecord, 0, limit-offset)
	for i := offset; i < limit; i++ {
		records = append(records, SingleRecord{Id: i})
	}
	return records
}

// divideIntoPartitions splits `number` into `n` approximately equal parts.
// Used to distribute records and sliding window capacity across partitions.
func divideIntoPartitions(number, n int) []int {
	base := number / n
	remainder := number % n
	parts := make([]int, n)
	for i := 0; i < n; i++ {
		parts[i] = base
	}
	for i := 0; i < remainder; i++ {
		parts[i]++
	}
	return parts
}

// ---------------------------------------------------------------------------
// RecordProcessorWorkflow
// ---------------------------------------------------------------------------

// RecordProcessorWorkflow processes a single record and signals the parent
// workflow upon completion. This mirrors the Temporal sample where the child
// signals the parent with "ReportCompletion".
//
// The parent workflow ID is included in SingleRecord.ParentWorkflowID so the
// child can route the signal correctly, even across ContinueAsNew boundaries.
func RecordProcessorWorkflow(h durable.HostCalls, inputJSON string) (string, error) {
	var record SingleRecord
	if err := json.Unmarshal([]byte(inputJSON), &record); err != nil {
		return "", fmt.Errorf("RecordProcessorWorkflow: invalid input: %w", err)
	}

	// Simulate record processing with a deterministic random sleep.
	// Cleat's Random() is seeded from event history, ensuring deterministic replay.
	randomMs := (h.Random() % 500) + 100 // 100-599ms
	h.DurableSleep(time.Duration(randomMs) * time.Millisecond)

	logMsg := fmt.Sprintf("Processed record %d (slept %dms)", record.Id, randomMs)
	h.DurableLog(logMsg)

	// Signal the parent workflow that this record is complete.
	// The parent's WorkflowID remains stable across ContinueAsNew runs.
	err := h.SignalWorkflow(record.ParentWorkflowID, "ReportCompletion", strconv.Itoa(record.Id))
	if err != nil {
		return "", fmt.Errorf("RecordProcessorWorkflow: signal parent: %w", err)
	}

	return mustJSON(map[string]interface{}{
		"status":   "completed",
		"record_id": record.Id,
	}), nil
}

// ---------------------------------------------------------------------------
// SlidingWindowWorkflow
// ---------------------------------------------------------------------------

// slidingWindow holds the mutable state for one run of the sliding window workflow.
type slidingWindow struct {
	input           SlidingWindowWorkflowInput
	currentRecords  map[int]bool   // record IDs of in-flight children
	offset          int             // next record to process
	progress        int             // completed records
}

// SlidingWindowWorkflow processes a range of records while limiting the number
// of concurrently-running child workflows to SlidingWindowSize. After starting
// PageSize children it calls ContinueAsNew to keep the event history bounded.
//
// Children signal completion via "ReportCompletion". A signal pump loop
// (using PollSignal + AwaitSignals) drains signals and starts new children
// as slots free up.
func SlidingWindowWorkflow(h durable.HostCalls, inputJSON string) (string, error) {
	var input SlidingWindowWorkflowInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("SlidingWindowWorkflow: invalid input: %w", err)
	}

	wf := &slidingWindow{
		input:          input,
		currentRecords: input.CurrentRecords,
		offset:         input.Offset,
		progress:       input.Progress,
	}
	if wf.currentRecords == nil {
		wf.currentRecords = make(map[int]bool)
	}

	// Expose state for external query.
	wf.setState(h)

	return wf.execute(h)
}

func (w *slidingWindow) setState(h durable.HostCalls) {
	if h == nil {
		return
	}
	ids := make([]int, 0, len(w.currentRecords))
	for id := range w.currentRecords {
		ids = append(ids, id)
	}
	state := SlidingWindowState{
		CurrentRecords: ids,
		Offset:         w.offset,
		Progress:       w.progress,
	}
	h.SetQueryState("state", mustJSON(state))
}

func (w *slidingWindow) execute(h durable.HostCalls) (string, error) {
	workflowID := h.WorkflowID()
	logMsg := fmt.Sprintf("SlidingWindowExecute offset=%d maxOffset=%d progress=%d inFlight=%d",
		w.offset, w.input.MaximumOffset, w.progress, len(w.currentRecords))
	h.DurableLog(logMsg)

	// Phase 1: load a page of records and start processing them.
	totalStarted := 0

	// Keep loading and processing pages until we exhaust the range or
	// hit the ContinueAsNew threshold.
	for w.offset < w.input.MaximumOffset {
		pageRecords := getRecords(w.offset, w.input.PageSize, w.input.MaximumOffset)
		if len(pageRecords) == 0 {
			break
		}

		for _, record := range pageRecords {
			// Wait for a slot in the sliding window.
			if err := w.waitForSlot(h); err != nil {
				return "", err
			}

			// If we've started PageSize children in this run, ContinueAsNew.
			if totalStarted >= w.input.PageSize {
				return w.continueAsNew(h)
			}

			// Prepare child input with the parent workflow ID so the child
			// can signal back across ContinueAsNew boundaries.
			childInput := SingleRecord{
				Id:               record.Id,
				ParentWorkflowID: workflowID,
			}

			_, err := h.ChildWorkflow("RecordProcessorWorkflow", mustJSON(childInput))
			if err != nil {
				return "", fmt.Errorf("start child for record %d: %w", record.Id, err)
			}

			w.currentRecords[record.Id] = true
			totalStarted++
			w.offset++
			w.setState(h)
		}
	}

	// Phase 2: all records in this run have been started.
	// Wait for all in-flight children to complete.
	if err := w.drainChildren(h); err != nil {
		return "", err
	}

	// Phase 3: if more records remain (should not happen with correct bounds),
	// ContinueAsNew. Otherwise return the final progress.
	if w.offset < w.input.MaximumOffset {
		return w.continueAsNew(h)
	}

	h.DurableLog(fmt.Sprintf("SlidingWindowWorkflow done: progress=%d", w.progress))
	w.setState(h)
	return strconv.Itoa(w.progress), nil
}

// waitForSlot blocks until a child slot opens up (len(currentRecords) < SlidingWindowSize).
// It drains pending completion signals first, then waits with AwaitSignals.
func (w *slidingWindow) waitForSlot(h durable.HostCalls) error {
	for len(w.currentRecords) >= w.input.SlidingWindowSize {
		// Drain any pending signals without blocking.
		if w.drainOneSignal(h) {
			continue
		}
		// No pending signals and window is full -- block until one arrives.
		result := h.AwaitSignals([]string{"ReportCompletion"}, 24*time.Hour)
		if result.Err != nil {
			return fmt.Errorf("waitForSlot signal error: %w", result.Err)
		}
		if !result.TimedOut {
			w.handleCompletion(result.Payload)
		}
	}
	return nil
}

// drainOneSignal attempts to receive one pending "ReportCompletion" signal
// without blocking. Returns true if a signal was processed.
func (w *slidingWindow) drainOneSignal(h durable.HostCalls) bool {
	payload, found, err := h.PollSignal("ReportCompletion")
	if err != nil || !found {
		return false
	}
	w.handleCompletion(payload)
	return true
}

// handleCompletion processes a "ReportCompletion" signal payload.
// The payload is the record ID as a string.
func (w *slidingWindow) handleCompletion(payload string) {
	recordID, err := strconv.Atoi(strings.TrimSpace(payload))
	if err != nil {
		return // ignore malformed signals
	}
	if _, ok := w.currentRecords[recordID]; ok {
		delete(w.currentRecords, recordID)
		w.progress++
	}
}

// drainChildren waits for all in-flight children to complete.
func (w *slidingWindow) drainChildren(h durable.HostCalls) error {
	for len(w.currentRecords) > 0 {
		// First try to drain any pending signals.
		if w.drainOneSignal(h) {
			continue
		}
		// Block until a completion signal arrives.
		result := h.AwaitSignals([]string{"ReportCompletion"}, 24*time.Hour)
		if result.Err != nil {
			return fmt.Errorf("drainChildren signal error: %w", result.Err)
		}
		if !result.TimedOut {
			w.handleCompletion(result.Payload)
		}
	}
	return nil
}

// continueAsNew drains pending signals, packages the current state into
// a new SlidingWindowWorkflowInput, and calls ContinueAsNew.
func (w *slidingWindow) continueAsNew(h durable.HostCalls) (string, error) {
	// Drain any pending signals before ContinueAsNew to avoid signal loss.
	for w.drainOneSignal(h) {
	}

	newInput := SlidingWindowWorkflowInput{
		PageSize:          w.input.PageSize,
		SlidingWindowSize: w.input.SlidingWindowSize,
		Offset:            w.offset,
		MaximumOffset:     w.input.MaximumOffset,
		Progress:          w.progress,
		CurrentRecords:    w.currentRecords,
	}

	h.DurableLog(fmt.Sprintf("ContinueAsNew offset=%d progress=%d inFlight=%d",
		w.offset, w.progress, len(w.currentRecords)))

	// In production, ContinueAsNew creates a new workflow run and the
	// current run stops. In the test environment it is a no-op that
	// returns nil, so execution continues.
	err := h.ContinueAsNew(mustJSON(newInput))
	if err != nil {
		return "", err
	}

	// Fallthrough for test environments where ContinueAsNew is a no-op.
	return strconv.Itoa(w.progress), nil
}

// ---------------------------------------------------------------------------
// ProcessBatchWorkflow
// ---------------------------------------------------------------------------

// ProcessBatchWorkflow is the top-level entry point. It partitions the record
// range across multiple parallel SlidingWindowWorkflow instances, each running
// with a portion of the total sliding window capacity.
//
// This mirrors the Temporal ProcessBatchWorkflow which fans out to N
// SlidingWindowWorkflow children and aggregates their results.
func ProcessBatchWorkflow(h durable.HostCalls, inputJSON string) (string, error) {
	var input ProcessBatchWorkflowInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("ProcessBatchWorkflow: invalid input: %w", err)
	}

	if input.RecordCount <= 0 {
		return "0", nil
	}
	if input.SlidingWindowSize < input.Partitions {
		return "", fmt.Errorf("SlidingWindowSize (%d) cannot be less than Partitions (%d)",
			input.SlidingWindowSize, input.Partitions)
	}

	partitions := divideIntoPartitions(input.RecordCount, input.Partitions)
	windowSizes := divideIntoPartitions(input.SlidingWindowSize, input.Partitions)

	h.DurableLog(fmt.Sprintf("ProcessBatch partitions=%v windowSizes=%v", partitions, windowSizes))

	runIDs := make([]string, 0, input.Partitions)
	offset := 0
	for i := 0; i < input.Partitions; i++ {
		maxOffset := offset + partitions[i]
		if maxOffset > input.RecordCount {
			maxOffset = input.RecordCount
		}
		childInput := SlidingWindowWorkflowInput{
			PageSize:          input.PageSize,
			SlidingWindowSize: windowSizes[i],
			Offset:            offset,
			MaximumOffset:     maxOffset,
			Progress:          0,
			CurrentRecords:    nil,
		}
		runID, err := h.ChildWorkflow("SlidingWindowWorkflow", mustJSON(childInput))
		if err != nil {
			return "", fmt.Errorf("ProcessBatch: start partition %d: %w", i, err)
		}
		runIDs = append(runIDs, runID)
		offset += partitions[i]
	}

	// AwaitAllChildren waits for all child workflows concurrently and
	// returns results in the same order as runIDs.
	results, err := h.AwaitAllChildren(runIDs)
	if err != nil {
		return "", fmt.Errorf("ProcessBatch: await children: %w", err)
	}

	total := 0
	for i, r := range results {
		if r.Error != "" {
			return "", fmt.Errorf("partition %d failed: %s", i, r.Error)
		}
		count, convErr := strconv.Atoi(r.Result)
		if convErr != nil {
			return "", fmt.Errorf("partition %d bad result %q: %w", i, r.Result, convErr)
		}
		total += count
	}

	return strconv.Itoa(total), nil
}
