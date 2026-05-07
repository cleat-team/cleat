# Issues Found During Porting: Updatable Timer

## 1. `RegisterUpdateHandler` is Fully Implemented in the Go SDK

**Status: No issue found.** Unlike the Python SDK where `register_update_handler` is reportedly a stub (see Python issue #52), the Go SDK's `RegisterUpdateHandler` is fully functional. It stores handlers in an internal map, supports typed variants via `RegisterTypedUpdateHandler`, and validates payloads through a separate validator function.

However, there is no `HandleUpdate` method on `durabletest.TestEnv`, which means:
- Update handlers cannot be tested in isolation within the test environment.
- Only integration tests against a running Cleat runtime can exercise them.
- The tests in this port verify registration succeeds (no panic), not end-to-end invocation.

**Recommendation**: Add a `HandleUpdate(name, payloadJSON string)` method to `TestEnv`, analogous to the existing `HandleQuery`, so update handlers can be tested in unit tests.

## 2. `AwaitSignals` with Timeout is the Right Pattern for "Sleep Until X or Signal"

**Status: Works well, but requires manual remaining-time calculation.** The natural Cleat pattern for the updatable timer is:

```go
remaining := wakeUpTime.Sub(h.Now())
result := h.AwaitSignals([]string{signalName}, remaining)
```

This is much more efficient than a polling loop with `DurableSleep` + `PollSignal`, because `AwaitSignals` produces one journal entry per wait cycle regardless of how long it blocks. A polling loop would produce one entry per poll interval.

**Recommendation**: Consider adding a `DurableSleepUntil(absoluteTime time.Time)` or `AwaitSignalsUntil(names []string, deadline time.Time)` helper to reduce boilerplate and avoid common mistakes (e.g., negative durations, timezone issues).

## 3. Signal vs. Update Handler: Two Separate Mechanisms

**Status: Architectural difference to note.** In Temporal, updates and signals both feed into the same `Selector` pattern that can race them against timers. In Cleat:

- **Signals** (`AwaitSignals` / `PollSignal`): Can interrupt a blocking sleep. This is the correct mechanism for the updatable timer.
- **Update handlers** (`RegisterUpdateHandler`): Modify workflow state but do NOT interrupt a blocking `AwaitSignals` call. They only take effect on the next loop iteration after `AwaitSignals` returns.

This means: if you want the timer update to actually shorten a currently-blocking sleep, you MUST use signals, not updates. The update handler modifies state for the next iteration but doesn't wake the goroutine.

**Recommendation**: Document this clearly. Users coming from Temporal will expect updates to be able to interrupt waiting.

## 4. No Built-in Way to Set PollCancellation in TestEnv

**Status: Missing testability.** `PollCancellation()` always returns `(false, "")` in the test environment. There is no `SetCancelled()` or `CancelWorkflow()` method on `TestEnv`, making it impossible to test cancellation handling in unit tests.

**Recommendation**: Add `env.SetCancelled(reason string)` or a `WithCancellation` option to `NewTestEnv`, so workflows that check `PollCancellation()` can be tested.

## 5. `HandleQuery` Works Correctly on TestEnv

**Status: Works well.** The `TestEnv.HandleQuery` method correctly invokes query handlers registered via `h.RegisterQueryHandler`. Queries are deterministic and return the current workflow state even while the workflow is blocked on `AwaitSignals`.

**Note**: There is a minor data race risk: query handlers and signal handlers both access `timer.wakeUpTime`. In the real runtime this is safe (single-threaded per workflow instance), but in test goroutines care is needed.

## 6. Deterministic Time via `h.Now()`

**Status: Works well.** `h.Now()` returns the test environment's simulated time, which advances deterministically with `env.AdvanceTime()`. Using this instead of `time.Now()` ensures replay safety.

## 7. Sleep Duration Calculation Edge Cases

When calculating `wakeUpTime.Sub(h.Now())`:
- If the workflow replays from history, `h.Now()` returns the *original* time, so the calculated remaining duration is correct for replay.
- If a signal arrives after the wake-up time has already passed, `remaining` will be <= 0 and the loop exits immediately (timer considered "fired").
- There is no explicit safeguard against negative durations, but the `if remaining <= 0 { return }` check handles this correctly.

## Summary

| Concern | Status |
|---------|--------|
| `RegisterUpdateHandler` in Go SDK | Fully implemented |
| Sleep-until-X-or-signal pattern | Works via `AwaitSignals` with remaining-time calculation |
| Polling loop history bloat | Avoided -- `AwaitSignals` is O(1) per wait cycle |
| Timer querying | Works via `RegisterQueryHandler` + `HandleQuery` |
| Update handler interrupt | Updates do NOT interrupt blocking `AwaitSignals` (architectural) |
| Cancellation testing | Not possible in `TestEnv` -- missing API |
| Deterministic time | Works via `h.Now()` + `env.AdvanceTime()` |
