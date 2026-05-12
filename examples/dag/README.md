# DAG Pipeline

Demonstrates a diamond dependency-graph pipeline using the `dagplugin` package: extract, classify + translate (parallel), then summarize.

## What it shows

- `dagplugin.NewDAG` / `AddTask` for building a directed acyclic graph of tasks
- `dagplugin.(*DAG).Execute` for level-by-level execution via `ChildWorkflow` / `AwaitAllChildren`
- `ParentOutput` for flowing results from upstream to downstream tasks
- `dagplugin.(*DAG).Output` for retrieving individual task results
- `DurableCall` for calling external services at each pipeline step

## Build

```bash
cleat build -o /tmp/out ./examples/dag/
```

## Run

```bash
cleat deploy dag /tmp/out/dag.wasm
cleat run Pipeline '{"text":"raw document content","lang":"en"}'
```

## Key files

- `pipeline.go` — pipeline entry point and task functions (extract, classify, translate, summarize)
- `pipeline.json` — workflow definition metadata
- `pipeline_test.go` — unit tests for the DAG workflow
