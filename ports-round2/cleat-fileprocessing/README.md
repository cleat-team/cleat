# Cleat Fileprocessing Port

This is a port of the [Temporal fileprocessing sample](https://github.com/temporalio/samples-go/tree/main/fileprocessing) to the [Cleat Go SDK](https://github.com/cleat-ai/durable).

## What It Does

The workflow downloads a file, transforms its content (uppercase), and uploads the result. The entire pipeline is retried on failure (up to 4 retries), mirroring the Temporal original's "retry the whole pipeline on a different host" pattern.

## Architecture Differences

| Aspect | Temporal | Cleat |
|--------|----------|-------|
| **Workflow signature** | `func(ctx workflow.Context, fileName string) error` | `func(h durable.HostCalls, fileName string) error` |
| **Activities** | Separate activity struct registered with worker | Host-side services called via `DurableCall` |
| **Worker** | Worker process registers workflows + activities | No worker; workflow runs in WASM or embedded runner |
| **Sessions** | `CreateSession` / `CompleteSession` for affinity | No equivalent (see ISSUES.md #1) |
| **Heartbeats** | `activity.RecordHeartbeat()` inside activities | `DurableCallWithHeartbeat` with workflow-side callback |
| **Retry policy** | `ActivityOptions.RetryPolicy` on all activities | `CallOptions.Retry` on individual calls |
| **Tests** | `testsuite.WorkflowTestSuite` + `testify/mock` | `durabletest.TestEnv` with `OnCall` stubs |

## File Layout

- `workflow.go` -- workflow function (`SampleFileProcessingWorkflow`)
- `services.go` -- host-side file processing operations
- `workflow_test.go` -- unit tests using `durabletest.TestEnv`
- `main.go` -- embedded runner demo + direct service invocation
- `go.mod`, `go.sum` -- Go module dependencies

## Running

```bash
# Run tests
go test -v ./...

# Run the demo (embedded runner + host services)
go run .
```

## Prerequisites

- Go 1.26+
- The Cleat SDK at `/localssd/rcownie/cleat-agent1` (referenced via `replace` directive in `go.mod`)

## Key Patterns Demonstrated

1. **Multi-step pipeline**: Sequential `DurableCallTyped` calls (download -> process -> upload)
2. **Per-call retry**: `DurableCallTypedWithOptions` with `CallOptions{Retry: ...}` for the download and upload steps
3. **Heartbeat progress reporting**: `DurableCallTypedWithHeartbeat` with a progress callback for the process step
4. **Outer retry loop**: Wraps the entire pipeline to handle transient failures
5. **Structured logging**: `DurableLog` and `LogKV` for workflow-level logging
6. **Host-side services**: File I/O lives outside the WASM sandbox in plain Go functions
