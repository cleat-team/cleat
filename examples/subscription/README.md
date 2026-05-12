# Subscription Billing

Demonstrates a recurring subscription billing workflow with retry logic, a grace period, and clean monthly recurrence via `ContinueAsNew`.

## What it shows

- `DurableCallWithRetry` for payment retries with exponential backoff
- `DurableSleep` for monthly billing cycles and daily grace-period retries
- `PollCancellation` to handle cancellation signals mid-workflow
- `ContinueAsNew` for clean monthly recurrence with bounded event history
- `SetQueryState` for external subscription status queries
- Grace period with multi-day retry loop before cancellation
- Best-effort notification sending (invoice, reminders, expiry notices)

## Build

```bash
cleat build -o /tmp/out ./examples/subscription/
```

## Run

```bash
cleat deploy subscription /tmp/out/subscription.wasm
cleat run ManageSubscription '{"user_id":"usr_001","plan_id":"pro","amount_usd":1999}'
```

## Key files

- `billing.go` — workflow entry point (`ManageSubscription`), retry and grace-period helpers
