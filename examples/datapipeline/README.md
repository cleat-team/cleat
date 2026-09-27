# Data Pipeline

Demonstrates a fan-out/fan-in data pipeline using typed child workflows and concurrent await.

## What it shows

- `ChildWorkflowTyped` for type-safe child workflow fan-out across items
- `AwaitAllChildren` for concurrent fan-in (all children awaited concurrently)
- `DurableCallTypedWithHeartbeat` for long-running steps that must keep their claim alive
- `SetQueryState` for tracking pipeline and per-item progress
- `DurableCall` for post-processing (notifications on completion)
- `DurableLog` for structured audit logging

## Build

```bash
cleat build -o /tmp/out ./examples/datapipeline/
```

## Run

```bash
cleat deploy --name datapipeline /tmp/out/process_item.wasm
cleat run --wasm /tmp/out/process_item.wasm --entry-point run_pipeline \
  --input '{"job_id":"job-001","items":["item1","item2","item3"],"batch_id":"batch-1"}'
```

## Key files

- `pipeline.go` — parent workflow (`RunPipeline`) and child workflow (`ProcessItem`)
