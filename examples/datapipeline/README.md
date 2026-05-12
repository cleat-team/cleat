# Data Pipeline

Demonstrates a fan-out/fan-in data pipeline using typed child workflows and concurrent await.

## What it shows

- `ChildWorkflowTyped` for type-safe child workflow fan-out across items
- `AwaitAllChildren` for concurrent fan-in (all children awaited concurrently)
- `DurableCallTypedWithHeartbeat` for long-running steps with progress callbacks
- `SetQueryState` for tracking pipeline and per-item progress
- `DurableCall` for post-processing (notifications on completion)
- `DurableLog` for structured audit logging

## Build

```bash
cleat build -o /tmp/out ./examples/datapipeline/
```

## Run

```bash
cleat deploy datapipeline /tmp/out/datapipeline.wasm
cleat run RunPipeline '{"job_id":"job-001","items":["item1","item2","item3"],"batch_id":"batch-1"}'
```

## Key files

- `pipeline.go` — parent workflow (`RunPipeline`) and child workflow (`ProcessItem`)
