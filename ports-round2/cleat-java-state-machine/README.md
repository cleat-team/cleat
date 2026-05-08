# Cleat Java Payment State Machine

Port of the Restate Java Payment State Machines example to use the Cleat durable
execution Java SDK.

## Source

The original Restate example lives at
`ports-round2/examples/java/patterns-use-cases/src/main/java/my/example/statemachinepayments/`.
It uses Restate's Virtual Object abstraction with three handlers (`makePayment`,
`cancelPayment`, `expire`) and a companion `Account` Virtual Object.

## Port Overview

| Restate Concept | Cleat Mapping |
|---|---|
| `@VirtualObject` | No equivalent. State is stored in query state with manual key prefixes. |
| `@Handler` method | `@CleatEntry` public static method with `HostCalls` as first param. |
| `ObjectContext.get(StateKey)` | `HostCalls.getQueryState(String key)` |
| `ObjectContext.set(StateKey, val)` | `HostCalls.setQueryState(String key, String val)` |
| `ctx.key()` | `HostCalls.currentWorkflowId()` (or paymentId from input JSON) |
| `AccountClient.fromContext()` then `.withdraw().await()` | `HostCalls.cleatCall("account", "withdraw", json)` |
| `send().expire(Duration)` | Not supported. Expiry must be triggered externally. |
| `TerminalException` | Error JSON returned from workflow method instead. |
| `Math.random()` | `HostCalls.random()` for deterministic replay. |
| `ObjectContext.clearAll()` | `HostCalls.setQueryState(key, "")` for each key. |

## Project Structure

```
cleat-java-state-machine/
  settings.gradle.kts          -- Multi-project: includes :cleat-java
  build.gradle.kts             -- TeaVM WASM build config
  src/main/java/cleatexample/statemachine/
    WorkflowEntry.java         -- TeaVM main entry (tree-shaking root)
    PaymentProcessor.java      -- Three @CleatEntry workflows
    Account.java               -- Three @CleatEntry account operations
    types/
      PaymentStatus.java       -- NEW / COMPLETED_SUCCESSFULLY / CANCELLED
      Payment.java             -- accountId + amountCents POJO
      Result.java              -- success + reason POJO
```

## State Machine Transitions

```
       NEW ─────► COMPLETED_SUCCESSFULLY ──► CANCELLED ──► (expired)
        │                                          │
        └──────────► CANCELLED ─────────────────────┘
```

- **makePayment**: NEW -> COMPLETED_SUCCESSFULLY (on success). Stores status and
  payment details in query state. Calls `account:withdraw` via cleatCall.
- **cancelPayment**: NEW -> CANCELLED (prevents future payment).
  COMPLETED_SUCCESSFULLY -> CANCELLED + refund via `account:deposit`.
- **expirePayment**: Clears all query keys for the payment.

## Key-scoped State (Virtual Object replacement)

Cleat has no Virtual Object concept. State keys are manually prefixed:

```
payment:<paymentId>:status        -> "COMPLETED_SUCCESSFULLY"
payment:<paymentId>:details       -> JSON of the original payment request
account_balance:<accountId>       -> "123456" (long as string)
```

## Building

Requirements: JDK 11+ (tested with JDK 17), Gradle 8.x.

```bash
./gradlew clean build
```

To produce the WASM binary:

```bash
./gradlew teavm
```

The WASM binary is written to `build/wasm/workflow.wasm`.

## Build Result

**Status:** Could not verify -- no JDK is available in the build environment.
The code was written to be compatible with the Cleat Java SDK (Java 11 source
level, TeaVM 0.10.2). Expected issues and gaps are documented in `ISSUES.md`.
