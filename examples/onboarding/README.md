# User Onboarding

Demonstrates a sign-up workflow with email verification, a 24-hour timeout window, and graceful cleanup on expiry.

## What it shows

- `AwaitSignals` to wait for the email verification click
- `DurableSleep` for the 24-hour verification window timeout
- `SetQueryState` for external status queries (track onboarding progress)
- Conditional branching based on signal vs. timeout
- Saga-style cleanup on timeout (delete pending registration)
- `DurableCall` for creating registrations, sending emails, and creating profiles
- `Now()` for timestamping workflow state

## Build

```bash
cleat build -o /tmp/out ./examples/onboarding/
```

## Run

```bash
cleat deploy onboarding /tmp/out/onboarding.wasm
cleat run RegisterUser '{"email":"alice@example.com","name":"Alice Smith","password":"s3cret"}'
```

## Key files

- `signup.go` — workflow entry point (`RegisterUser`) and timeout handler
