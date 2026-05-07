# Cleat Batch Sliding Window Port

Port of the [Temporal batch-sliding-window](https://github.com/temporalio/samples-go/tree/main/batch-sliding-window) Go sample to the Cleat durable execution Go SDK.

## Purpose

This port tests the Cleat Go SDK's support for batch processing patterns:

- **Continue-as-new**: Pagination via `HostCalls.ContinueAsNew` to keep event history bounded across large batch runs.
- **Child workflow fan-out**: Starting many child workflows and tracking their completion.
- **Sliding window concurrency**: Limiting concurrent child workflows to `N` at a time, using signal-based coordination.
- **Signal-based child-to-parent coordination**: Children signal the parent on completion, enabling the parent to start new children as slots free up.
- **Partitioned batch processing**: Top-level workflow slices the record space across parallel sliding window instances.

## Architecture

```
ProcessBatchWorkflow
  |
  +-- SlidingWindowWorkflow (partition 0)
  |     |-- RecordProcessorWorkflow (record 0..pageSize)
  |     |-- RecordProcessorWorkflow (record ..)
  |     +-- ContinueAsNew --> SlidingWindowWorkflow (next page)
  |
  +-- SlidingWindowWorkflow (partition 1)
  |
  +-- SlidingWindowWorkflow (partition 2)
```

### Workflows

1. **ProcessBatchWorkflow** (`ProcessBatchWorkflow`): Entry point. Divides the total record count into N partitions, starts one `SlidingWindowWorkflow` per partition, and aggregates results.

2. **SlidingWindowWorkflow** (`SlidingWindowWorkflow`): Processes a range of records with a sliding window concurrency limit. Loads a page of records, starts child workflows up to `SlidingWindowSize`, and uses `AwaitSignals`/`PollSignal` to detect completions and start new children. After `PageSize` children, calls `ContinueAsNew` with the current offset, progress, and in-flight record set.

3. **RecordProcessorWorkflow** (`RecordProcessorWorkflow`): Processes a single record (simulated with a deterministic random sleep), then signals the parent workflow via `SignalWorkflow` with the record ID.

### Key Cleat APIs Used

| API | Usage |
|-----|-------|
| `HostCalls.ChildWorkflow` | Fan-out to child workflows |
| `HostCalls.AwaitChild` / `AwaitAllChildren` | Wait for child workflow completion |
| `HostCalls.ContinueAsNew` | Paginate to keep history bounded |
| `HostCalls.SignalWorkflow` | Child signals parent on completion |
| `HostCalls.AwaitSignals` | Parent blocks waiting for completion signals |
| `HostCalls.PollSignal` | Non-blocking signal drain |
| `HostCalls.SetQueryState` | Expose sliding window state for queries |
| `HostCalls.DurableSleep` | Simulate record processing time |
| `HostCalls.Random` | Deterministic random for replay-safe processing |
| `HostCalls.DurableLog` | Structured logging |
| `HostCalls.WorkflowID` | Self-identify for child-to-parent signal routing |

## Files

- `workflow.go` - All workflow logic (ProcessBatchWorkflow, SlidingWindowWorkflow, RecordProcessorWorkflow)
- `workflow_test.go` - Tests for all workflows and utility functions
- `ISSUES.md` - Gap analysis and issues found
- `go.mod` - Module with dependency on `github.com/rcownie/durable`

## Running Tests

```bash
cd cleat-batch-window
go test ./... -v
```
