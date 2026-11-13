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

// @cleatEntry(name="submit_expense")
func SubmitExpense(h cleat.HostCalls, input string) (string, error) {
    var report ReportInput
    if err := json.Unmarshal([]byte(input), &report); err != nil {
        return "", fmt.Errorf(
            `input must be {"employee_id": ..., "amount_cents": ..., "description": ...}: %w`, err)
    }

    h.DurableLog(fmt.Sprintf(
        "expense report received: employee=%s amount=%d description=%s",
        report.EmployeeID, report.AmountCents, report.Description,
    ))

    // Persist the report ID so it can be queried externally.
    reportID := fmt.Sprintf("EXP-%d", h.Now().UnixMilli())
    h.SetQueryState("report_id", reportID)
    h.SetQueryState("status", "pending_approval")

    // Check if this report even needs approval.
    if report.AmountCents < 10000 { // under $100 -- auto-approve
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

Three details of the entry point are not optional:

- **The function is exported, and its name is the entry point's name.**
  `cleat build` finds entry points among exported functions that take
  `cleat.HostCalls` first, and names each by its function name in snake case:
  `SubmitExpense` is `submit_expense`, which is what `cleat.yaml`'s
  `entry_points` must list. A lower-case name is not found at all
  (`no workflow entry points found`).
- **The `// @cleatEntry(name="submit_expense")` comment is the scaffold's
  convention, and the Go build does not read it.** Changing the `name=` does not
  rename the entry point (cleat#3792), so keep it equal to the snake-case
  function name or leave it out.
- **The input is one `string`**, which receives the whole input JSON as written,
  and the function decodes it. A parameter of any other type binds by its Go
  *name* (a parameter called `input` would need the run started with
  `{"input": {"input": {...}}}`), and a run started the natural way fails with
  `unmarshal input: unexpected end of JSON input` before any of your code runs.
  `cleat build` prints a `W003` warning about the single `string` parameter;
  for this shape it is expected.

### Build and deploy it

This assumes the worker and key from the [quick start](quick-start.md): Postgres
running, the schema applied, the worker listening on `:8080`, and
`CLEAT_API_KEY` set in the shell you run the commands below in.

```bash
cleat init expense-workflow
cd expense-workflow
# Replace main.go with the program above. In cleat.yaml, change the
# entry_points list to:  - submit_expense
cleat build -o out .
cleat deploy \
    --db "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" \
    --name expense-workflow \
    out/expense-workflow.wasm
```

### Breaking it down

| Line(s) | What it does |
|---------|--------------|
| `AwaitSignals([]string{"approval"}, 72*time.Hour)` | Pauses the workflow until the `approval` signal arrives, or 72 hours pass |
| `res.Err` | Set when the wait failed. Test it **first** — a call the runtime rejects sets both this and `TimedOut` |
| `res.TimedOut` | Set when the window closes with no signal, and also on a rejected call, which is why `Err` is tested first |
| `json.Unmarshal([]byte(res.Payload), &status)` | Deserialises the signal payload into the `ApprovalStatus` struct |
| `h.SetQueryState(...)` | Stores queryable state so external systems can check workflow status without polling the signal |

### Auto-approval for small amounts

The workflow checks `report.AmountCents < 10000` and skips the signal wait
entirely for expenses under $100. This is a common pattern: only involve a
human when the risk warrants it.

## Sending signals

While the workflow is paused waiting for the `approval` signal, send it via
the REST API. Like every `/api/workflows/*` route it needs your API key, and
**`payload` is a string**: the JSON the workflow will decode goes inside it, so
it is JSON inside JSON and its quotes are escaped. `RUN_ID` is the `id` that
starting the run returned (the next section shows how to capture it).

```bash
curl -X POST "http://localhost:8080/api/workflows/$RUN_ID/signal" \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{
        "signal_name": "approval",
        "payload": "{\"approved\": true, \"reviewer\": \"jane.manager\", \"comment\": \"Looks reasonable\"}"
    }'
# {"idempotent_replay":false,"status":"delivered"}
```

A payload that is a JSON object rather than a string is refused with
`400 invalid JSON: json: cannot unmarshal object into Go struct field .payload of type string`.

To reject:

```bash
curl -X POST "http://localhost:8080/api/workflows/$RUN_ID/signal" \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{
        "signal_name": "approval",
        "payload": "{\"approved\": false, \"reviewer\": \"jane.manager\", \"comment\": \"Missing receipts\"}"
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

**Test `Err` before `TimedOut`.** When the window merely closes, `Err` is nil
and `TimedOut` is set — but that rejection sets **both**, so `TimedOut` alone
cannot tell "nobody decided" from "the call was refused". The example above
checks `Err` first for that reason: the ordering is load-bearing, not style.

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

Here is the full lifecycle of an expense report, as one script. It captures the
run's `id` with `sed` (so it needs nothing beyond `curl`), and the `sleep`s give
the worker a moment to pick the run up and to resume it:

```bash
# 1. Submit the expense report
RUN_ID=$(curl -s -X POST http://localhost:8080/api/workflows/expense-workflow/start \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{
        "input": {
            "employee_id": "emp_42",
            "amount_cents": 50000,
            "description": "Team lunch with client"
        }
    }' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "run: $RUN_ID"
sleep 3

# 2. Check status -- should be "pending_approval"
curl -s -H "Authorization: Bearer $CLEAT_API_KEY" \
    "http://localhost:8080/api/workflows/$RUN_ID/query?key=status"
# {"key":"status","value":"pending_approval"}

# 3. Send the approval signal
curl -s -X POST "http://localhost:8080/api/workflows/$RUN_ID/signal" \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{
        "signal_name": "approval",
        "payload": "{\"approved\": true, \"reviewer\": \"jane.manager\", \"comment\": \"Approved\"}"
    }'
# {"idempotent_replay":false,"status":"delivered"}
sleep 3

# 4. Verify the workflow completed
curl -s -H "Authorization: Bearer $CLEAT_API_KEY" \
    "http://localhost:8080/api/workflows/$RUN_ID/query?key=status"
# {"key":"status","value":"approved"}

curl -s -H "Authorization: Bearer $CLEAT_API_KEY" \
    "http://localhost:8080/api/workflows/$RUN_ID/query?key=approved_by"
# {"key":"approved_by","value":"jane.manager"}
```

The start response is `{"id":"<run id>","idempotent_replay":false}`. The query
URLs are quoted because an unquoted `?` is a glob character in zsh. Each query
reads one key, by design.

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
