# Python Hello World

Simplest Cleat Python/WASM workflow.

## Quickstart

```bash
# 1. Build to WASM
durable build --target python --entry hello_workflow.py:hello

# 2. Run
durable run hello '{"name": "World"}'
```

## What This Demonstrates

- `@durable_entry` marks the workflow entry point
- `h.durable_call("greeter", "greet", ...)` makes a recorded API call
- The WASM binary can be loaded by the cleat worker
- On crash recovery, the workflow replays deterministically
