# How to test workflows

## Overview

The `cleattest` package provides a mock `HostCalls` implementation for testing workflows without compiling to WASM or running a full worker. Tests run as normal Go tests against your workflow source code directly. No database is needed.

Testing with `cleattest` catches logic errors, incorrect signal handling, and unexpected plugin call patterns before you deploy.

## Setting up a test

Import the test package and create a `TestEnv`:

```go
package myworkflow_test

import (
    "testing"
    "github.com/rcownie/cleat/cleat/cleattest"
)

func TestMyWorkflow(t *testing.T) {
    env := cleattest.NewTestEnv()
    defer env.Close() // not strictly required but good practice

    h := env.H() // returns the HostCalls interface
    // ... run your workflow via h
}
```

If your workflow entry point has the signature `func MyWorkflow(h cleat.HostCalls, input string) error`, you call it directly:

```go
err := MyWorkflow(env.H(), `{"key": "value"}`)
if err != nil {
    t.Fatalf("workflow failed: %v", err)
}
```

## Mocking DurableCall

Use `env.OnCall(service, operation, requestMatcher).Return(response, error)` to stub `DurableCall` invocations:

```go
env.OnCall("payments", "Charge", nil).Return(`{"status":"ok","txn_id":"txn_123"}`, nil)
env.OnCall("inventory", "CheckStock", `{"sku":"WIDGET-A"}`).Return(`{"available":10}`, nil)
```

The third argument to `OnCall` is a request matcher:

- `nil` -- matches any request string
- A string -- matches only that exact request string
- A `func(string) bool` -- calls the function to determine a match

```go
// Match by predicate.
env.OnCall("dispatch", "AssignDriver", func(s string) bool {
    return strings.Contains(s, "zone_downtown")
}).Return(`{"driver_id":"drv_99"}`, nil)
```

### ReturnJSON helper

For structured responses, use `ReturnJSON` to avoid manual marshaling:

```go
env.OnCall("payments", "Charge", nil).ReturnJSON(map[string]interface{}{
    "status": "ok",
    "txn_id": "txn_123",
}, nil)
```

## Mocking plugin calls

Use `env.OnPluginCall(pluginName, functionName)` to stub `h.PluginCall(...)`:

```go
env.OnPluginCall("llm", "chat").Return(
    `{"content":"Order summary generated","model":"claude-sonnet-4-20250514","usage":{"input_tokens":50,"output_tokens":20}}`,
    nil,
)
```

## Testing signals

Use `env.Signal(name, payload)` to deliver a signal at the current simulated time. Use `env.AfterSignal(delay, name, payload)` to schedule a signal for later.

```go
func TestApprovalWorkflow(t *testing.T) {
    env := cleattest.NewTestEnv()
    h := env.H()

    // Start the workflow in a goroutine; it will block on AwaitSignals.
    go func() {
        err := ApprovalWorkflow(h, `{"amount": 5000}`)
        if err != nil {
            t.Errorf("workflow failed: %v", err)
        }
    }()

    // Simulate a manager approving the request.
    env.Signal("approved", `{"reviewer": "alice", "note": "looks good"}`)
}
```

If your workflow uses `AwaitSignals` with a timeout, advance the clock past the timeout to test the time-out branch:

```go
func TestApprovalTimeout(t *testing.T) {
    env := cleattest.NewTestEnv()
    h := env.H()

    go func() {
        err := ApprovalWorkflow(h, `{"amount": 5000}`)
        if err == nil {
            t.Error("expected timeout error, got nil")
        }
    }()

    // Advance past the 24-hour approval window.
    env.AdvanceTime(25 * time.Hour)
}
```

## Testing timeouts with AdvanceTime

`env.AdvanceTime(duration)` advances the simulated clock, fires any `DurableSleep` timers that expire, and delivers signals that become due. Use it to test timer-based behavior without real wall-clock waiting.

```go
func TestPollingWorkflow(t *testing.T) {
    env := cleattest.NewTestEnv()
    env.OnCall("dispatch", "CheckStatus", nil).Return(`{"status":"pending"}`, nil)

    h := env.H()

    // Run the workflow (polls every 30s, deadline 5min).
    err := PollForDelivery(h, `{"order_id":"ord_1"}`)
    // Expect timeout after 5 minutes of polling.
    if err == nil || !strings.Contains(err.Error(), "timed out") {
        t.Fatalf("expected timeout, got: %v", err)
    }

    // Verify the polling interval: ~10 calls in 5 minutes.
    calls := env.CallHistory()
    if len(calls) < 8 {
        t.Errorf("expected at least 8 poll calls, got %d", len(calls))
    }
}
```

Note that `AdvanceTime` only advances the clock. If your workflow calls `h.PollCancellation()`, use `env.SetCancelled(reason)` to simulate cancellation between poll cycles:

```go
env.SetCancelled("manual override")
```

## Assertions

### Asserting calls were made

```go
env.AssertCalled(t, "payments", "Charge")
env.AssertNotCalled(t, "payments", "Refund")
```

### Inspecting call history

```go
history := env.CallHistory()
if len(history) != 2 {
    t.Fatalf("expected 2 calls, got %d", len(history))
}
if history[0].Service != "payments" {
    t.Errorf("expected first call to payments, got %s", history[0].Service)
}
```

`CallRecord` fields:

| Field       | Type   | Description                              |
|-------------|--------|------------------------------------------|
| Service     | string | Service name passed to DurableCall       |
| Operation   | string | Operation name passed to DurableCall     |
| Request     | string | Request JSON                             |
| Response    | string | Response JSON from the stub              |
| Err         | error  | Error returned, if any                   |
| RetryCount  | int    | Number of retries before success (0 if none) |

### Query state assertions

```go
err := MyWorkflow(env.H(), `{"order_id":"ord_1"}`)
if err != nil {
    t.Fatalf("workflow failed: %v", err)
}

status, ok := env.QueryState("order_status")
if !ok {
    t.Fatal("expected query state 'order_status' to be set")
}
if status != "confirmed" {
    t.Errorf("expected 'confirmed', got %q", status)
}
```

### Version assertions

```go
env.SetVersion(2)
err := MyWorkflow(env.H(), input)
// ... assertions ...

env.AssertCalled(t, "new_service", "NewOp")
env.AssertNotCalled(t, "old_service", "LegacyOp")
```

### ContinueAsNew assertions

For workflows that use `h.ContinueAsNew`, you can verify the restart input:

```go
env.AssertContinued(t, `{"user_id":"usr_1","items":["item_3","item_4"]}`)
```

## Complete example: approval workflow test

```go
package myworkflow_test

import (
    "encoding/json"
    "strings"
    "testing"
    "time"

    "github.com/rcownie/cleat/cleat/cleattest"
)

func TestApprovalWorkflow_Success(t *testing.T) {
    env := cleattest.NewTestEnv()
    defer env.Close()

    // Mock the notification plugin.
    env.OnPluginCall("slack-notify", "send_message").Return(
        `{"ok":true,"channel":"C01234","ts":"1712345678.000100"}`, nil,
    )
    // Mock the ledger update.
    env.OnCall("ledger", "RecordApproval", nil).Return(`{"status":"ok"}`, nil)

    h := env.H()

    go func() {
        err := ApprovalWorkflow(h, `{"amount":5000,"requested_by":"user_42"}`)
        if err != nil {
            t.Errorf("workflow failed: %v", err)
        }
    }()

    // Advance time slightly and send approval signal.
    time.Sleep(10 * time.Millisecond) // yield to goroutine scheduler
    env.Signal("approved", `{"reviewer":"alice","note":"approved"}`)

    // Verify ledger was updated.
    env.AssertCalled(t, "ledger", "RecordApproval")
}

func TestApprovalWorkflow_Timeout(t *testing.T) {
    env := cleattest.NewTestEnv()
    env.OnPluginCall("slack-notify", "send_message").Return(
        `{"ok":true}`, nil,
    )

    h := env.H()

    go func() {
        err := ApprovalWorkflow(h, `{"amount":5000,"requested_by":"user_42"}`)
        if err == nil {
            t.Error("expected timeout error")
        } else if !strings.Contains(err.Error(), "timed out") {
            t.Errorf("unexpected error: %v", err)
        }
    }()

    // Advance past the 24-hour approval window.
    env.AdvanceTime(25 * time.Hour)
}

func TestApprovalWorkflow_Rejected(t *testing.T) {
    env := cleattest.NewTestEnv()
    env.OnPluginCall("slack-notify", "send_message").Return(
        `{"ok":true}`, nil,
    )

    h := env.H()

    go func() {
        err := ApprovalWorkflow(h, `{"amount":5000,"requested_by":"user_42"}`)
        if err != nil {
            t.Errorf("workflow failed: %v", err)
        }
    }()

    time.Sleep(10 * time.Millisecond)
    env.Signal("rejected", `{"reviewer":"bob","reason":"budary limit exceeded"}`)

    // Verify no ledger update was made on rejection.
    env.AssertNotCalled(t, "ledger", "RecordApproval")
}
```

## Replay testing

For advanced test scenarios, `cleattest` supports recording and replay:

```go
func TestReplayWorkflow(t *testing.T) {
    env := cleattest.NewTestEnv()

    // Phase 1: record.
    env.EnableReplay()
    err := MyWorkflow(env.H(), input)
    if err != nil {
        t.Fatalf("recording run failed: %v", err)
    }

    // Phase 2: replay.
    env.StartReplay()
    err = MyWorkflow(env.H(), input)
    if err != nil {
        t.Fatalf("replay run failed: %v", err)
    }

    // Ensure replay produced the same call pattern.
    env.AssertReplayDivergence(t, 0)
}
```

## Running tests

```bash
# Run all workflow tests.
go test ./...

# Run tests in a specific package with verbose output.
go test -v ./myworkflow/

# Run a specific test.
go test -v -run TestApprovalWorkflow_Success ./myworkflow/

# With race detection (tests use goroutines for signal delivery).
go test -race ./...
```

Because `cleattest` tests run against your Go source code directly (not compiled WASM), they execute at full native speed. No worker process or database is needed.

## Next steps

- See the [common patterns guide](common-patterns.md) for workflow patterns to test
- See the [cleattest package documentation](https://pkg.go.dev/github.com/rcownie/cleat/cleat/cleattest) for the full API reference
- See the [test suite](https://github.com/rcownie/cleat/tree/main/cleat/cleattest) for more examples
