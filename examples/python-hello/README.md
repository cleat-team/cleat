# Python Hello World

Simplest Cleat Python/WASM workflow.

## Quickstart

```bash
# 1. Build to WASM
cleat build --target python --entry hello_workflow.py:hello

# 2. Run
cleat run --wasm hello.wasm --entry-point Hello --input '{"name": "World"}'
```

## What This Demonstrates

- `@cleat_entry` marks the workflow entry point
- `h.call("greeter", "greet", ...)` makes a recorded API call
- The WASM binary can be loaded by the cleat worker
- On crash recovery, the workflow replays deterministically
