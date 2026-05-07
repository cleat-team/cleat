# Cleat Updatable Timer

Port of the Temporal [updatable-timer](https://github.com/temporalio/samples-go/tree/main/updatabletimer) sample to the Cleat Go SDK (`github.com/rcownie/cleat`).

## What It Demonstrates

A durable timer that can be dynamically updated while sleeping. The workflow starts with a long timer, but external signals can change the wake-up time at any point.

Key patterns:

- **Durable sleep with signal interrupt**: `AwaitSignals` with a calculated timeout replaces Temporal's `Selector{AddFuture(timer)+AddReceive(channel)}` pattern.
- **Signal-based timer updates**: External callers send `UpdateWakeUpTime` signals with a new `time.Time` payload.
- **Query handler**: External callers can query the current wake-up time at any point via the `GetWakeUpTime` query handler.
- **Update handler**: An alternative to signals for updating the timer, registered via `RegisterUpdateHandler`.
- **Deterministic time**: Uses `h.Now()` instead of `time.Now()` for replay-safe time calculations.

## Architecture

The core structure is `UpdatableTimer`, a helper that maintains a `wakeUpTime` field and implements a `SleepUntil` loop:

```
for {
    remaining = wakeUpTime - h.Now()
    if remaining <= 0 { return }  // timer fired

    result = h.AwaitSignals(["UpdateWakeUpTime"], remaining)

    if result.TimedOut  { return }  // timer fired
    if signal arrives   { wakeUpTime = parse(result.Payload); continue }
}
```

This avoids the history-bloat problem of polling-based approaches (like `DurableSleep` in a loop with `PollSignal`) because `AwaitSignals` produces a single journal entry per wait cycle, not one per poll iteration.

## Files

| File | Purpose |
|------|---------|
| `workflow.go` | Workflow logic: `UpdatableTimer` helper and the main `Workflow` entry point |
| `workflow_test.go` | Unit tests using `durabletest.TestEnv` |
| `go.mod` | Go module definition with `replace` directive for local Cleat SDK |

## Prerequisites

- Go 1.26+
- The Cleat Go SDK (`github.com/rcownie/cleat`) at `../../durable` relative to this project

## Running Tests

```bash
go test ./... -v -count=1
```

## Key Differences from Temporal

| Temporal | Cleat |
|----------|-------|
| `workflow.NewTimer(ctx, d)` | `h.DurableSleep(d)` |
| `workflow.NewSelector(ctx).AddFuture(timer, fn).AddReceive(ch, fn).Select(ctx)` | `h.AwaitSignals(names, timeout)` in a loop |
| `workflow.GetSignalChannel(ctx, name)` | `h.AwaitSignals([name], timeout)` |
| `workflow.SetQueryHandler(ctx, name, fn)` | `h.RegisterQueryHandler(name, fn)` |
| `workflow.SetUpdateHandler(ctx, name, fn, val)` | `h.RegisterUpdateHandler(name, fn, val)` |
| `workflow.Now(ctx)` | `h.Now()` |
| `ctx.Err()` / `ctx.Done()` | `h.PollCancellation()` |
| `time.Now()` | `h.Now()` (deterministic) |
