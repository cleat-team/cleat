# How to debug workflows

## Overview

`cleatctl debug` provides step-through replay for workflow event histories, helping you diagnose stuck workflows, unexpected results, and determinism issues. It replays the workflow's recorded events one at a time and lets you inspect query state after each step.

The debugger is **read-only** — it never modifies the database or event history.

Two modes are available:

- **Step-through mode** (default): pause after each event, inspect state, and control execution.
- **Watch mode** (`--watch`): tail new events as they arrive in a running workflow.

## Quick start

```bash
cleatctl --db "postgres://user:pass@localhost:5432/cleat" debug wf-abc123 --entry-point HandleLead
```

Output:

```
Workflow: wf-abc123
  Definition: lead-pipeline (v3)
  Status: running
  Entry point: HandleLead
  Events: 15
  WASM size: 245760 bytes

Starting step-through replay...
Commands: next (n), continue (c), state (s), events (e), help (h), quit (q)

── Step 1/15 ── type=call ── service=payments ── op=Charge
  request:   {"amount":999}
  response:  {"charge_id":"ch_123"}
  query_state: {"cart_total": "999"}
debug>
```

## Step-through mode

In step-through mode, the engine replays the workflow event by event. After each event the debugger pauses and waits for a command.

### Per-step display

Each step shows:
- Step number (1-based) and total event count
- Event type and contextual metadata (service/op, signal name, state operation, etc.)
- Input/output payloads (request, response, error, signal payload)
- Current query state as a JSON map

```
── Step 3/15 ── type=signal_received ── signal=order_shipped
  payload:   {"tracking":"TRK-1"}
  query_state: {"status": "shipped", "user_id": "u1"}
debug>
```

### Step-through session example

```
debug> n
── Step 2/15 ── type=call ── service=email ── op=send
  request:   {"to":"user@example.com","subject":"Welcome"}
  response:  {"id":"msg_42"}
  query_state: {"user_email": "user@example.com"}

debug> s
query_state:
  user_email = user@example.com
  cart_total = 999
  status = pending

debug> e
Remaining events:
  [1] step=2 type=signal_received signal=order_shipped
  [2] step=3 type=call service=shipping op=CreateLabel
  ...

debug> c
(continues silently through remaining events)

Replay complete.
```

### Interactive commands

| Command | Shortcut | Description |
|---------|----------|-------------|
| `next` | `n`, Enter | Advance one event and display step info |
| `continue` | `c` | Run remaining events without pausing (final results only) |
| `state` | `s` | Dump full query_state key-value map |
| `events` | `e` | List remaining event types with indices and summaries |
| `help` | `h` | Show available commands |
| `quit` | `q` | Exit debugger cleanly |

## Watch mode

Watch mode tails new events as they arrive for a running workflow. It polls every 2 seconds and displays each new event.

```bash
cleatctl --db "postgres://..." debug wf-abc123 --watch
```

Output:

```
Watching workflow wf-abc123 (12 events so far)...
(Ctrl+C to stop)
  [12] type=call service=email op=send request={"to":"u@e.com"} response={"id":"m1"}
  [13] type=signal_received signal=wake payload={"at":"noon"}
```

Watch mode exits when:
- You press Ctrl+C
- No new events arrive for 60 seconds
- The workflow reaches a terminal state and stops producing events
- The DB connection fails

## Common scenarios

### Debugging a stuck workflow

A workflow that's been running for hours without progressing may be waiting on a signal that was never sent:

```bash
cleatctl debug wf-stuck-123 --entry-point Handle
# Step through to see where it's blocked
debug> c
# If replay ends in a suspend for await_signals, check the signal names
```

### Verifying determinism

If a workflow produces different results on replay, step through to find where divergence occurs:

```bash
cleatctl debug wf-divergent-456 --entry-point Process
debug> n   # advance one at a time
debug> s   # check query state at each step
# Compare against expected state to find where it diverged
```

### Inspecting side effects

Side effects are recorded in the event history. Step through to verify their values:

```bash
cleatctl debug wf-side-effect-789 --entry-point Run
debug> n
── Step 4/12 ── type=side_effect ── ... 
  response: {"generated_id":"abc-123"}
```

## Limitations

- **Read-only**: the debugger replays existing event history and never modifies the database.
- **Requires DB access**: you must have a working PostgreSQL connection with the `--db` flag or `CLEAT_DB_URL` environment variable.
- **Replay only**: the debugger replays recorded history, it does not run fresh execution. If the replay diverges (causing the WASM module to make a call not in the history), the debugger reports the divergence error.
- **Module must be available**: the WASM binary for the workflow's definition and version must still exist in the database.
- **Large histories**: for workflows with more than ~10K events, consider using watch mode or replay instead.
