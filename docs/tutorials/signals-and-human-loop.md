# Signals and the human-in-the-loop pattern

This tutorial teaches you how to build workflows that pause and wait for
external input -- a pattern commonly called "human-in-the-loop."

---

## What are signals?

Signals are external messages delivered to a **running** workflow instance.
They let humans, other services, or scheduled jobs send data into a workflow
at a specific point in its execution. Unlike `DurableCall` which calls out to
a service, signals let the outside world call **in**.

Use cases include:

- An approval step where a manager must approve or reject an expense report
- A confirmation step where a user must verify their email address
- An escalation step where a human operator must intervene after an automated
  retry budget is exhausted

Cleat exposes the `cleat.HostCalls.AwaitSignals` method to wait for one or
more signals, with a timeout.

## Signal mechanics

- Signals are **durable**: if the workflow is mid-execution and the worker
  crashes, the pending signal wait is recorded in the event log and
  re-established on replay.
- Signals are **typed by name**: each signal has a string name. A workflow
  can await multiple signal names at once.
- Signals carry a **payload**: an arbitrary JSON string that the workflow
  deserialises.
- Signal delivery is **at-least-once**: the runtime guarantees a signal is
  delivered to the workflow instance at least once, but the workflow's
  deterministic replay deduplicates it.

## A simple approval workflow

Here is a complete workflow that pauses for a human to approve or reject an
expense report:

```go
package main

import (
    "encoding/json"
    "fmt"
    "time"

    "github.com/cleat-team/cleat/cleat"
)

// ReportInput is the data needed to submit an expense report.
type ReportInput struct {
    EmployeeID string `json:"employee_id"`
    AmountCents int  `json:"amount_cents"`
    Description string `json:"description"`
}

// ApprovalStatus is the outcome of the human review.
type ApprovalStatus struct {
    Approved bool   `json:"approved"`
    Reviewer  string `json:"reviewer,omitempty"`
    Comment   string `json:"comment,omitempty"`
}

//go:export submit_expense
func submitExpense(h cleat.HostCalls, input ReportInput) (string, error) {
    h.DurableLog(fmt.Sprintf(
        "expense report received: employee=%s amount=%d description=%s",
        input.EmployeeID, input.AmountCents, input.Description,
    ))

    // Persist the report ID so it can be queried externally.
    reportID := fmt.Sprintf("EXP-%d", h.Now().UnixMilli())
    h.SetQueryState("report_id", reportID)
    h.SetQueryState("status", "pending_approval")

    // Check if this report even needs approval.
    if input.AmountCents < 10000 { // under $100 -- auto-approve
        h.DurableLog("amount below threshold, auto-approved")
        h.SetQueryState("status", "approved")
        return reportID, nil
    }

    // Wait for a human to send an "approval" signal.
    h.DurableLog("waiting for human approval")

    res := h.AwaitSignals([]string{"approval"}, 72*time.Hour)
    if res.Err != nil {
        // A failure while waiting -- distinct from the wait expiring.
        h.SetQueryState("status", "escalated")
        h.DurableLog("approval wait failed, escalating")
        return "", fmt.Errorf("approval wait failed for %s: %w", reportID, res.Err)
    }
    if res.TimedOut {
        // No decision within the window.
        h.SetQueryState("status", "escalated")
        h.DurableLog("approval timed out, escalating")
        return "", fmt.Errorf("approval timeout for %s", reportID)
    }

    var status ApprovalStatus
    if err := json.Unmarshal([]byte(res.Payload), &status); err != nil {
        h.SetQueryState("status", "escalated")
        h.DurableLog("approval payload was not valid JSON, escalating")
        return "", fmt.Errorf("invalid approval payload for %s: %w", reportID, err)
    }

    if !status.Approved {
        h.SetQueryState("status", "rejected")
        h.DurableLog(fmt.Sprintf(
            "expense rejected by %s: %s",
            status.Reviewer, status.Comment,
        ))
        return reportID, nil
    }

    h.SetQueryState("status", "approved")
    h.SetQueryState("approved_by", status.Reviewer)
    h.DurableLog(fmt.Sprintf("expense approved by %s", status.Reviewer))
    return reportID, nil
}
```

### Breaking it down

| Line(s) | What it does |
|---------|--------------|
| `AwaitSignals([]string{"approval"}, 72*time.Hour)` | Pauses the workflow until the `approval` signal arrives, or 72 hours pass |
| `res.Err` | A failure while waiting — distinct from the wait expiring |
| `res.TimedOut` | True when the window closes with no signal |
| `json.Unmarshal([]byte(res.Payload), &status)` | Deserialises the signal payload into the `ApprovalStatus` struct |
| `h.SetQueryState(...)` | Stores queryable state so external systems can check workflow status without polling the signal |

### Auto-approval for small amounts

The workflow checks `input.AmountCents < 10000` and skips the signal wait
entirely for expenses under $100. This is a common pattern: only involve a
human when the risk warrants it.

## Sending signals

While the workflow is paused waiting for the `approval` signal, send it via
the REST API:

```bash
curl -X POST http://localhost:8080/api/workflows/<workflow_id>/signal \
    -H "Content-Type: application/json" \
    -d '{
        "signal_name": "approval",
        "payload": {
            "approved": true,
            "reviewer": "jane.manager",
            "comment": "Looks reasonable"
        }
    }'
```

To reject:

```bash
curl -X POST http://localhost:8080/api/workflows/<workflow_id>/signal \
    -H "Content-Type: application/json" \
    -d '{
        "signal_name": "approval",
        "payload": {
            "approved": false,
            "reviewer": "jane.manager",
            "comment": "Missing receipts"
        }
    }'
```

After the signal is delivered, the workflow resumes, reads the payload, and
continues execution.

## Signal timeouts

The second argument to `AwaitSignals` is how long to wait before giving up.
When the window closes with no signal, the result's `TimedOut` field is set
and `Err` stays nil, so a workflow can tell "nobody decided" apart from "the
wait failed" and take alternative action:

```go
res := h.AwaitSignals([]string{"approval"}, 72*time.Hour)
if res.TimedOut {
    // Escalate to a different channel, notify on-call, etc.
    h.DurableLog("approval timed out, escalating to on-call")
    h.SetQueryState("status", "escalated")
    return "", fmt.Errorf("approval timed out for %s", reportID)
}
```

**The timeout is required, and there is no indefinite wait.** Pass a
`time.Duration` long enough for the decision you are waiting on. A value that
rounds to 0 milliseconds — anything under 1ms — is **rejected** rather than
treated as "wait forever": a 0ms await has no deadline to expire and would
never return, so the runtime returns an error instead of hanging. For a
non-blocking check, use `h.PollSignals` instead.

## Awaiting multiple signals

Pass several signal names in the slice. The call returns when **any one** of
them arrives, and `Name` says which:

```go
res := h.AwaitSignals([]string{"approve", "reject", "escalate"}, 72*time.Hour)
switch res.Name {
case "approve":
    h.DurableLog("approved")
case "reject":
    h.DurableLog("rejected")
case "escalate":
    h.DurableLog("escalated")
}
```

`Name` and `Payload` describe the signal that arrived; `Payload` is the JSON
the sender sent. Check `Err` and `TimedOut` first — when either is set, `Name`
is empty. Use this pattern for workflows with more than two outcomes.

## Complete end-to-end example

Here is the full lifecycle of an expense report workflow:

```bash
# 1. Submit the expense report
curl -X POST http://localhost:8080/api/workflows/expense-workflow/start \
    -H "Content-Type: application/json" \
    -d '{
        "input": {
            "employee_id": "emp_42",
            "amount_cents": 50000,
            "description": "Team lunch with client"
        }
    }'
# Response: {"workflow_id": "wf_exp_abc123", "status": "running"}

# 2. Check status -- should be "pending_approval"
curl http://localhost:8080/api/workflows/wf_exp_abc123/query?key=status
# Response: {"key": "status", "value": "pending_approval"}

# 3. Send the approval signal
curl -X POST http://localhost:8080/api/workflows/wf_exp_abc123/signal \
    -H "Content-Type: application/json" \
    -d '{
        "signal_name": "approval",
        "payload": {
            "approved": true,
            "reviewer": "jane.manager",
            "comment": "Approved"
        }
    }'
# Response: {"status": "signal_delivered"}

# 4. Verify the workflow completed
curl http://localhost:8080/api/workflows/wf_exp_abc123/query?key=status
# Response: {"key": "status", "value": "approved"}

curl http://localhost:8080/api/workflows/wf_exp_abc123/query?key=approved_by
# Response: {"key": "approved_by", "value": "jane.manager"}
```

## Best practices

- **The timeout is required, so choose it deliberately.** It is the longest you
  are willing to let the decision sit unresolved; when it expires, `TimedOut`
  is set and your workflow decides what happens next. A wait nobody is coming
  back to answer holds the run and is easy to forget about.
- **Store queryable state** before and after the signal wait so external
  dashboards can report on workflow status without polling the signal
  infrastructure.
- **Validate signal payloads** inside the workflow. A malformed signal should
  not crash the workflow -- log the error and re-await or escalate.
- **Use descriptive signal names** like `approve`, `reject`, `escalate`
  rather than generic names like `signal1`, `signal2`.

## Next steps

- [Your first workflow: order processing](your-first-workflow.md) -- build a
  multi-step workflow with compensation
- [Common patterns](../how-to/common-patterns.md) -- Saga, fan-out, child
  workflows, retry policies
- [Quick start](quick-start.md) -- get a workflow running in 5 minutes
