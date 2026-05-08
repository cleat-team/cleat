// Data pipeline — fan-out/fan-in with child workflows and AwaitAllChildren.
//
// Demonstrates:
//   - ChildWorkflowTyped for type-safe child workflow fan-out
//   - AwaitAllChildren for concurrent fan-in (all children awaited concurrently)
//   - DurableCallTypedWithHeartbeat for long-running steps with progress
//   - SetQueryState for tracking pipeline progress
//
// Build:
//
//	cleat build -o /tmp/out ./examples/datapipeline/
package datapipeline

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rcownie/cleat/durable"
)

var h cleat.HostCalls

// ---- Domain types ----

type PipelineInput struct {
	JobID   string   `json:"job_id"`
	Items   []string `json:"items"`
	BatchID string   `json:"batch_id"`
}

type PipelineResult struct {
	JobID      string              `json:"job_id"`
	TotalItems int                 `json:"total_items"`
	Succeeded  int                 `json:"succeeded"`
	Failed     int                 `json:"failed"`
	Results    []cleat.ChildResult `json:"results,omitempty"`
}

// ---- Parent workflow: fan-out/fan-in ----

func RunPipeline(h cleat.HostCalls, input PipelineInput) (*PipelineResult, error) {
	if len(input.Items) == 0 {
		return nil, fmt.Errorf("no items to process")
	}

	h.SetQueryState("job_id", input.JobID)
	h.SetQueryState("status", "running")
	h.SetQueryState("total", fmt.Sprintf("%d", len(input.Items)))
	h.DurableLog(fmt.Sprintf("Pipeline starting: job=%s items=%d", input.JobID, len(input.Items)))

	// Fan-out: start a typed child workflow for each item.
	var runIDs []string
	for i, item := range input.Items {
		childInput := ChildInput{Item: item, JobID: input.JobID, Index: i, BatchID: input.BatchID}
		runID, err := h.ChildWorkflowTyped("process_item", childInput)
		if err != nil {
			h.DurableLog(fmt.Sprintf("Failed to start child for %s: %v", item, err))
			continue
		}
		runIDs = append(runIDs, runID)
	}

	h.SetQueryState("children", fmt.Sprintf("%d", len(runIDs)))
	h.DurableLog(fmt.Sprintf("Started %d child workflows", len(runIDs)))

	// Fan-in: await all children concurrently.
	results, err := h.AwaitAllChildren(runIDs)
	if err != nil {
		return nil, fmt.Errorf("await children failed: %w", err)
	}

	// Count outcomes.
	var succeeded, failed int
	for _, r := range results {
		if r.Error == "" {
			succeeded++
		} else {
			failed++
		}
	}

	h.DurableCall("notifications", "PipelineComplete", toJSON(map[string]interface{}{
		"job_id":    input.JobID,
		"succeeded": succeeded,
		"failed":    failed,
	}))

	h.SetQueryState("status", "complete")
	h.SetQueryState("succeeded", fmt.Sprintf("%d", succeeded))
	h.SetQueryState("failed", fmt.Sprintf("%d", failed))
	h.DurableLog(fmt.Sprintf("Pipeline complete: job=%s succeeded=%d failed=%d",
		input.JobID, succeeded, failed))

	return &PipelineResult{
		JobID:      input.JobID,
		TotalItems: len(input.Items),
		Succeeded:  succeeded,
		Failed:     failed,
		Results:    results,
	}, nil
}

// ---- Child workflow types ----

type ChildInput struct {
	Item    string `json:"item"`
	JobID   string `json:"job_id"`
	Index   int    `json:"index"`
	BatchID string `json:"batch_id"`
}

type ChildResult struct {
	Item   string `json:"item"`
	Output string `json:"output"`
	Bytes  int    `json:"bytes"`
}

// ProcessItem is the child workflow entry point.
func ProcessItem(h cleat.HostCalls, input ChildInput) (*ChildResult, error) {
	h.DurableLog(fmt.Sprintf("Processing item %d/%s: %s", input.Index, input.BatchID, input.Item))

	// Step 1: Fetch data (with heartbeat for long downloads).
	h.SetQueryState("stage", "fetching")

	var fetchData struct {
		Raw  string `json:"raw"`
		Size int    `json:"size"`
	}
	if err := h.DurableCallTypedWithHeartbeat(
		"data", "Fetch",
		map[string]string{"item": input.Item},
		&fetchData,
		5*time.Second,
		func(progressJSON string) {
			var p struct{ Percent int `json:"percent"` }
			if json.Unmarshal([]byte(progressJSON), &p) == nil {
				h.SetQueryState("fetch_progress", fmt.Sprintf("%d%%", p.Percent))
			}
		},
	); err != nil {
		return nil, fmt.Errorf("fetch failed for %s: %w", input.Item, err)
	}

	// Step 2: Transform data.
	h.SetQueryState("stage", "transforming")
	transformResult, err := h.DurableCall("transform", "Process", toJSON(map[string]string{
		"item": input.Item,
		"raw":  fetchData.Raw,
	}))
	if err != nil {
		return nil, fmt.Errorf("transform failed for %s: %w", input.Item, err)
	}

	var transformData struct {
		Output string `json:"output"`
		Bytes  int    `json:"bytes"`
	}
	json.Unmarshal([]byte(transformResult), &transformData)

	// Step 3: Store result.
	h.SetQueryState("stage", "storing")
	if _, err := h.DurableCall("storage", "Put", toJSON(map[string]interface{}{
		"item":   input.Item,
		"output": transformData.Output,
		"job_id": input.JobID,
	})); err != nil {
		return nil, fmt.Errorf("store failed for %s: %w", input.Item, err)
	}

	h.SetQueryState("stage", "done")
	return &ChildResult{
		Item:   input.Item,
		Output: transformData.Output,
		Bytes:  transformData.Bytes,
	}, nil
}

func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
