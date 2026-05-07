# Plan: Finish Event-Driven Triggers and DAG Composition

May 2026 — both features are substantially built but not production-ready. This plan finishes them.

---

## Reality Check

The existing `implementation-plan-2026-05.md` defined these as Feature 1 and Feature 7. Both were partially built but the work stalled before wiring them up:

- **Event triggers**: ~80% complete. Publish/subscribe API, filter engine, synchronous dispatch all work. Missing: background retry worker, sharded mode support, dead-letter queue.
- **DAG composition**: ~90% complete. Task/parents/Execute/topological sort all work. Missing: nothing imports it — dead code. No examples, no e2e tests, no CLI.

---

## Part 1: Event-Driven Triggers

### What exists (plugins/eventtriggers/)

- `POST /api/events/publish` — idempotent event ingestion, synchronous subscription matching, `StartWorkflow` dispatch
- `POST/GET/DELETE /api/events/subscriptions` — CRUD for `(event_type, def_name, entry_point, filter_expr, input_template)`
- 826-line filter engine (`filter.go`) — JSON structured (`$eq`, `$gt`, `$in`) and text expression (`event.data.amount > 100`) with full lexer, parser, evaluator. 30+ tests.
- `event_subscriptions` and `ingested_events` tables with `WHERE NOT processed` partial index
- Plugin registers as `"event-triggers"` and loads when `--api-addr` is set

### What's missing

1. **No background retry worker** (critical). `ingested_events` has `processed=false` + `error_msg` + a partial index `WHERE NOT processed`, but the `HasBackground` interface's `Run(ctx)` is not implemented. If `StartWorkflow` fails during publish (transient DB error, missing def), the event is stored and silently abandoned. The schema was designed for a retry loop that was never written.

2. **No dead-letter queue.** Events that fail after max retries have no routing — they stay in `ingested_events` with `processed=false` forever.

3. **No retry configuration on subscriptions.** No `max_retries` or `retry_backoff` columns on `event_subscriptions`.

4. **No HostFunctions for workflows.** Workflows can't await domain events from within workflow code. `HasHostFunctions` is not implemented.

5. **Plugins don't load in sharded mode.** `cmd/durable-worker/main.go` only initializes plugins when `--api-addr` is set. `--shards-file` mode gets no event triggers.

6. **`webhookingest` plugin** has the same missing retry worker. Webhook signal delivery is synchronous-only — if `SignalWorkflow` fails, the signal is lost.

### Work items

#### Wave 1: Background retry worker (2 days)

- **1.1** Implement `HasBackground` on `eventtriggers`. `Run(ctx)` polls `ingested_events` every 30s using the partial index, retries `StartWorkflow`, sets `processed=true` on success, increments `retry_count` on failure. After `max_retries` (from subscription config, default 3), marks `status='dead_letter'`.
- **1.2** Add `retry_count INTEGER DEFAULT 0`, `last_retry_at TIMESTAMPTZ`, `status TEXT DEFAULT 'pending'` columns to `ingested_events` (new migration).
- **1.3** Add `max_retries INTEGER DEFAULT 3` to `event_subscriptions`.
- **1.4** Add Prometheus metrics: `eventtriggers_events_processed_total`, `eventtriggers_events_dead_letter_total`, `eventtriggers_retry_loop_duration_seconds`.

#### Wave 2: Webhook retry + sharded mode (2 days)

- **2.1** Implement `HasBackground` on `webhookingest`. Same pattern: poll unprocessed webhook events, retry `SignalWorkflow`.
- **2.2** Add `retry_count`, `last_retry_at`, `status` columns to `webhook_events`.
- **2.3** Decouple plugin HTTP routes from plugin lifecycle. Plugins should load regardless of `--api-addr` — the HTTP server is an optional consumer of plugin routes, not a prerequisite for plugin initialization.

#### Wave 3: Polish (2 days)

- **3.1** Implement `HasHostFunctions` on `eventtriggers`. Add `h.AwaitEvent(eventType, timeout)` so workflows can wait for domain events directly.
- **3.2** Dead-letter replay endpoint: `POST /api/events/{event_id}/retry`.
- **3.3** Example: `examples/event-driven/` — subscription-driven workflow.

---

## Part 2: DAG Composition

### What exists (plugins/dag/)

- `dag.Task{Name, Parents, Fn}` — task nodes with explicit parent dependency edges (mirrors Hatchet's model)
- `dag.NewDAG()` / `AddTask()` — programmatic DAG construction
- `dag.Execute(h, input)` — Kahn's algorithm topological sort, cycle detection, level-by-level execution via `ChildWorkflow` + `AwaitAllChildren`
- `buildParentOutputs()` — passes parent results to children via JSON
- 14 unit tests: linear chains, diamond, fan-out/fan-in, cycles, validation
- Plugin registered as `"dag"` via `init()`

### What's missing

The package is dead code. `dag.NewDAG()` is never called outside `dag_test.go`.

1. **No wired usage.** No example, no e2e test, no integration with `localdev` or `durabletest`.
2. **No declarative spec.** No YAML/JSON format for defining DAGs. Users must write Go and call `AddTask()`.
3. **No CLI.** `durable dag` doesn't exist. No way to validate or run DAGs from the command line.
4. **No code generator.** No way to produce a deployable workflow from a DAG spec.
5. **No visualization.** Web UI only shows a linear event timeline.

### Work items

#### Wave 1: Wire up and integrate (2 days)

- **1.1** Create `examples/dag/pipeline.go` — a diamond DAG: `extract → classify+translate → summarize`. Uses `dag.NewDAG()` + `AddTask()` + `Execute()`. Also shows `MaxParallelism`.
- **1.2** e2e test using `durabletest.TestEnv`. Verify: tasks execute in topological order, parent outputs flow to children, results correct, cycle detection works, disconnected nodes are handled.
- **1.3** Fix any bugs found during integration. The package has only been exercised in unit tests; real `HostCalls` may surface issues.
- **1.4** Add `ExecuteWithOptions(h, input, opts)` with `MaxParallelism int` to limit concurrent child workflows per topological level.

#### Wave 2: Declarative spec + CLI (3 days)

- **2.1** Define JSON DAG spec format:
  ```json
  {
    "name": "document-pipeline",
    "tasks": [
      {"name": "extract", "fn": "ExtractText"},
      {"name": "classify", "fn": "ClassifyDocument", "parents": ["extract"]},
      {"name": "translate", "fn": "TranslateDocument", "parents": ["extract"]},
      {"name": "summarize", "fn": "SummarizeDocument", "parents": ["classify", "translate"]}
    ]
  }
  ```
- **2.2** `dag.LoadFromJSON(reader, funcRegistry)` — parse spec, resolve task functions from a `map[string]TaskFunc` registry.
- **2.3** `durable dag validate spec.json` — parse, validate (cycles, missing parents, unknown functions), report errors.
- **2.4** `durable dag run spec.json --input '{"doc": "..."}'` — run against localdev for development/testing.
- **2.5** `durable dag generate spec.json > pipeline.go` — generate a complete Go workflow file that embeds the DAG, a `funcRegistry`, and a `#[durable_entry]` function. The generated code compiles to WASM like any other workflow.

#### Wave 3: Visualization (3 days, optional)

- **3.1** DAG graph panel in Web UI. Svelte component rendering the DAG topology using `svelte-flow` or raw SVG.
- **3.2** New endpoint `GET /api/workflows/:id/dag` returning task graph data when the workflow was generated from a DAG spec.
- **3.3** Store DAG spec in `workflow_defs` metadata for runtime retrieval.

---

## Effort Summary

| Part | Wave | Effort |
|------|------|--------|
| Event triggers | 1: Background retry worker | 2 days |
| Event triggers | 2: Webhook retry + sharded | 2 days |
| Event triggers | 3: HostFunctions + replay | 2 days |
| DAG | 1: Wire up + example + e2e | 2 days |
| DAG | 2: Declarative spec + CLI | 3 days |
| DAG | 3: Visualization | 3 days |
| **Total** | | **~3 weeks** |

## Build Order

1. **Event triggers Wave 1** — highest impact. The synchronous path already works; the missing retry worker means transient failures silently drop events. This is a correctness fix, not a feature.
2. **DAG Wave 1** — the example + e2e test proves the dead code works and makes it discoverable.
3. **DAG Wave 2** — declarative spec + CLI makes DAGs accessible without writing Go.
4. **Event triggers Wave 2** — webhook retry + sharded mode.
5. Remaining waves in any order.
