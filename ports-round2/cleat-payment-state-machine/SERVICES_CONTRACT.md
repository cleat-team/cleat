# Cleat Payment State Machine - Host Services Contract

This document defines the host-side services required to run the payment state machine workflow on the Cleat platform.

## Registered Durable Entry Points

The following `@durable_entry` functions are registered and must be routable by the Cleat runtime:

| Entry Point Name | Source | Input | Output | Description |
|---|---|---|---|---|
| `account.deposit` | `accounts.py` | `DepositRequest` | `None` | Deposit funds into an account |
| `account.withdraw` | `accounts.py` | `WithdrawRequest` | `Result` | Withdraw funds from an account |
| `PaymentProcessor.makePayment` | `workflow.py` | `PaymentRequest` | `str` | Process a payment through the state machine |
| `PaymentProcessor.cancelPayment` | `workflow.py` | `CancelRequest` | `None` | Cancel a payment, refunding if already completed |
| `PaymentProcessor.expire` | `workflow.py` | `dict` | `None` | Clean up expired payment state |

## Required Host Runtime Capabilities

### 1. State Management (required)

The runtime MUST provide durable key-value state that survives workflow recovery.

- **API**: `HostCalls.set_state(key, value)` and `HostCalls.get_state(key)`
- **Semantics**: Writes are durably recorded and replayed on recovery.
- **Keys used**:
  - `account:{account_id}:balance` -- account balance (integer cents)
  - `payment:{payment_id}:status` -- payment state machine status
  - `payment:{payment_id}:payment` -- serialized Payment object

### 2. Service Invocation (required)

The runtime MUST support durable service-to-service calls.

- **API**: `HostCalls.durable_call(service, operation, request)`
- **Services called**:
  - `account.deposit` (from `PaymentProcessor.cancelPayment` for refunds)
  - `account.withdraw` (from `PaymentProcessor.makePayment` for charges)
- **Semantics**: Results are durably cached. On recovery, previously completed calls are replayed rather than re-executed.

### 3. Durable Timers (required)

The runtime MUST support durable timers for the expiry mechanism.

- **API**: `HostCalls.durable_sleep(ms)`
- **Usage**: 24-hour timer before state cleanup
- **Semantics**: Timer continues running across workflow recovery. Remaining duration is preserved.

### 4. Terminal Error Support (required)

The runtime MUST support non-retryable error handling.

- **Exception**: `cleat_sdk.TerminalError`
- **Semantics**: When a workflow raises TerminalError, the runtime should NOT retry the failed step. The error should propagate to the caller.

### 5. Workflow Identity (required)

The runtime MUST assign a unique workflow ID to each execution.

- **API**: `HostCalls.key()` (aliased from `workflow_id`)
- **Usage**: Used as the payment ID for state isolation.

### 6. Saga Orchestration (nice-to-have)

The runtime encourages use of the Saga class for compensation logic.

- **API**: `cleat_sdk.Saga` with `add_step(action, compensate)` and `execute()`
- **Usage**: The payment workflow uses Saga to automatically refund the account if the payment fails after withdrawal.
- **Note**: The Saga class is a pure user-space utility and does not require runtime support beyond the standard durable_call primitive.

## External Dependencies

The application is self-contained -- all "external" services (account management) are implemented as cleat `@durable_entry` functions. No external databases or third-party APIs are required.

## Invocation Pattern

In production, external callers would invoke workflows via the Cleat runtime's HTTP API:

```
POST /workflow/PaymentProcessor.makePayment
{
    "payment_id": "pay-123",
    "account_id": "acct-456",
    "amount_cents": 5000
}

POST /workflow/PaymentProcessor.cancelPayment
{
    "payment_id": "pay-123"
}
```

The exact HTTP interface depends on the Cleat runtime implementation.
