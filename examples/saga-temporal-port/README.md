# Saga Temporal Port

A cleat Go SDK port of the Temporal `samples-go` money transfer saga, validating saga compensation pattern coverage.

## What it shows

- `cleat.NewSaga` / `AddStep` with forward and compensate callbacks
- `DurableCallWithOptions` with custom retry policy (exponential backoff)
- Saga compensation: withdraw, deposit, then reverse on failure
- `DurableCallTyped` for type-safe service invocations
- Retry policy matching the original Temporal example's configuration
- Inline step functions for forward and compensate closures

## Build

```bash
cleat build -o /tmp/out ./examples/saga-temporal-port/
```

## Run

```bash
cleat deploy saga-temporal-port /tmp/out/saga-temporal-port.wasm
cleat run TransferMoney '{"amount":100,"from_account":"account-a","to_account":"account-b","reference_id":"ref-001"}'
```

## Key files

- `workflow.go` — saga entry point (`TransferMoney`) and retry helpers
- `workflow_test.go` — workflow tests
- `ISSUES.md` — documented porting issues and SDK differences from Temporal
