# DBOS to Cleat Migration Guide

Migrate your DBOS workflows to the Cleat durable execution framework. This guide
covers conceptual mapping, API differences, code examples, and known gaps.

---

## Conceptual Mapping

| DBOS Concept | Cleat Equivalent | Notes |
|---|---|---|
| `@DBOS.workflow` / `@Workflow` | `@cleat_entry` | Both use decorator-based entry points |
| `@DBOS.step` / `@Step` | `call()` | No step/activity annotation needed |
| `DBOS.sleepSeconds(n)` | `sleep(n * 1000)` | **Milliseconds** in Cleat |
| `DBOS.recv(signalName, timeout)` | `await_signals()` | Similar semantics |
| `DBOS.send(workflowID, signalName, ...)` | `send()` / REST signal API | Similar |
| `DBOS.setEvent(key, value)` | `set_query_state()` | Similar key-value query state |
| `DBOS.getEvent(workflowID, key)` | REST: `GET /api/workflows/:id/query?key=X` | External query |
| `DBOS.startWorkflow` | `child_workflow()` | Similar |
| `DBOS.getWorkflowInput` | Input parameter of entry function | Declared as function parameter |
| `WorkflowContext` | `HostCalls` first parameter | Injected by decorator |
| `ConfiguredInstance` / classes | Stateless functions + `HostCalls` | No class-based configuration |
| `@DBOS.transaction` / `@DBOSStep` | `call("database", "query", ...)` | Externalize DB access via service |
| `DBOS.logger` | `log()` | Similar deterministic logging |
| Authentication / `@DBAuth` | Host-level middleware | API key auth on REST endpoints |
| `DBOS.retry` | `call_with_retry()` | Server-side retry built-in |

---

## API Differences

### Workflow Definition

**DBOS (TypeScript):**
```typescript
import { DBOS } from "@dbos-inc/dbos-sdk";

export class MyWorkflow {
    @DBOS.workflow()
    @DBOS.defaultRequiredRole("user")
    static async myWorkflow(input: MyInput): Promise<MyOutput> {
        // ...
    }
}
```

**Cleat (Python):**
```python
from cleat_sdk import HostCalls, cleat_entry

@cleat_entry
def my_workflow(h: HostCalls, input: MyInput) -> str:
    pass
```

**Cleat (Go):**
```go
//go:cleat_entry
func MyWorkflow(h cleat.HostCalls, input MyInput) (MyOutput, error) {
    // ...
}
```

### External Service Call

**DBOS (TypeScript) — step:**
```typescript
@DBOS.step()
static async chargeCard(amount: number): Promise<ChargeResult> {
    const response = await fetch("https://payment.example.com/charge", {
        method: "POST",
        body: JSON.stringify({ amount }),
    });
    return await response.json();
}

// In workflow:
const result = await MyWorkflow.chargeCard(100);
```

**Cleat (Python) — durable call:**
```python
resp = h.call("payment", "charge", {"amount": 100})
```

**Cleat (Go):**
```go
resp, err := h.Call("payment", "charge", requestJSON)
```

### Timer / Sleep

**DBOS:**
```typescript
await DBOS.sleepSeconds(5);  // 5 seconds
```

**Cleat (Python) — note milliseconds:**
```python
h.sleep(5000)  # 5 seconds
```

**Cleat (Go):**
```go
h.Sleep(5 * time.Second)
```

### Signal Communication

**DBOS — send:**
```typescript
await DBOS.send(workflowID, "approval", { approved: true });
```

**DBOS — receive:**
```typescript
const result = await DBOS.recv("approval", 30);
```

**Cleat — send (Python):**
```python
# Via REST API
client = CleatClient("http://localhost:8080")
client.send_signal(run_id, "approval", '{"approved": true}')

# Or fire-and-forget from within another workflow
h.send("workflows", "signal", {
    "run_id": target_run_id,
    "signal": "approval",
    "payload": '{"approved": true}',
})
```

**Cleat — receive (Python):**
```python
result = h.await_signals(["approval"], 30000)
```

### Starting a Child Workflow

**DBOS:**
```typescript
const handle = await DBOS.startWorkflow(MyChildWorkflow, { workflowID: "child-1" }).someMethod(input);
await handle.getResult();
```

**Cleat (Python):**
```python
run_id = h.child_workflow("my_child", input_data)
result = h.await_child(run_id)
```

### Workflow Context

**DBOS (TypeScript):**
```typescript
@DBOS.workflow()
static async myWorkflow(input: MyInput): Promise<void> {
    // DBOS context is implicitly available via the class
    const wfid = DBOS.workflowID;
    const req = DBOS.request;
}
```

**Cleat (Python):**
```python
@cleat_entry
def my_workflow(h: HostCalls, input: MyInput) -> str:
    wfid = h.current_workflow_id()
    run_id = h.current_run_id()
```

---

## Before/After Examples

### Payment Workflow with Saga

**DBOS (TypeScript) — before:**
```typescript
export class OrderWorkflow {
    @DBOS.workflow()
    static async processOrder(input: OrderInput): Promise<OrderResult> {
        DBOS.logger.info(`Processing order ${input.orderId}`);

        // Step 1: Reserve inventory
        const reserveResult = await DBOS.step(
            async () => {
                const resp = await fetch("http://inventory/reserve", {
                    method: "POST",
                    body: JSON.stringify(input.items),
                });
                return await resp.json();
            },
            { retriesAllowed: true }
        );

        // Step 2: Charge payment
        let chargeResult;
        try {
            chargeResult = await DBOS.step(async () => {
                const resp = await fetch("http://payments/charge", {
                    method: "POST",
                    body: JSON.stringify({ amount: input.total }),
                });
                return await resp.json();
            });
        } catch (error) {
            // Compensate: release inventory
            await DBOS.step(async () => {
                await fetch("http://inventory/release", {
                    method: "POST",
                    body: JSON.stringify({ id: reserveResult.id }),
                });
            });
            throw error;
        }

        // Step 3: Create shipment
        const shipResult = await DBOS.step(async () => {
            const resp = await fetch("http://shipping/create", {
                method: "POST",
                body: JSON.stringify({ orderId: input.orderId }),
            });
            return await resp.json();
        });

        DBOS.setEvent("status", "completed");
        return { status: "shipped", tracking: shipResult.tracking };
    }
}
```

**Cleat (Python) — after:**
```python
from cleat_sdk import HostCalls, cleat_entry, Saga
import json

@cleat_entry
def process_order(h: HostCalls, input: dict) -> str:
    saga = Saga(h)
    order_id = input["orderId"]

    saga.add_step_fn(
        "reserve_inventory",
        action=lambda h: h.call(
            "inventory", "Reserve", {"items": input["items"]}
        ),
        compensate=lambda h: h.call(
            "inventory", "Release", json.dumps({"order_id": order_id})
        ),
    )

    saga.add_step_fn(
        "charge_payment",
        action=lambda h: h.call(
            "payment", "Charge", {"amount": input["total"]}
        ),
        compensate=lambda h: h.call(
            "payment", "Refund", json.dumps({"order_id": order_id})
        ),
    )

    saga.add_step_fn(
        "create_shipment",
        action=lambda h: h.call(
            "shipping", "CreateShipment", {"order_id": order_id}
        ),
        compensate=None,  # best-effort
    )

    results = saga.execute()
    h.set_query_state("status", '"completed"')
    return json.dumps({"status": "shipped"})
```

### Retry Configuration

**DBOS:**
```typescript
await DBOS.step(
    async () => fetch("http://service/call"),
    {
        retriesAllowed: true,
        intervalSeconds: 1,
        maxIntervalSeconds: 30,
        backOffRate: 2.0,
    }
);
```

**Cleat (Python):**
```python
h.call_with_retry(
    "service", "call", {},
    RetryPolicy(
        max_attempts=3,
        initial_interval_ms=1000,
        backoff_coefficient=2.0,
        max_interval_ms=30000,
    )
)
```

**Cleat (Go):**
```go
h.CallWithOptions(cleat.CallOptions{
    Retry: &cleat.RetryPolicy{
        MaxAttempts:        3,
        InitialInterval:    1 * time.Second,
        BackoffCoefficient: 2.0,
        MaxInterval:        30 * time.Second,
    },
}, "service", "call", requestJSON)
```

---

## Known Gaps and Workarounds

### 1. No Class-Based Configuration

DBOS uses classes with decorators for workflow definitions, step configuration,
and authentication. Cleat uses plain functions with `@cleat_entry`.

- **Gap**: No declarative per-step retry configuration or authentication roles.
- **Workaround**: Configure retry at the call site with `call_with_retry()`.
  Authentication is handled at the host level (API keys, middleware), not in
  workflow code.

### 2. No `@DBOS.transaction` Equivalent

DBOS can wrap database transactions directly within workflow code. Cleat workflows
run in WASM and cannot access databases directly.

- **Gap**: Cannot embed SQL or ORM calls directly in workflow code.
- **Workaround**: Externalize database access through a "database" service and
  call it via `h.call("database", "query", ...)`. The database service
  runs outside WASM and can use full SQL/ORM capabilities.

### 3. Unit Differences

- **Gap**: `DBOS.sleepSeconds(n)` takes seconds; Cleat `sleep(ms)` takes
  milliseconds.
- **Workaround**: Multiply by 1000: `h.sleep(dbos_seconds * 1000)`.
  In Go, use `h.Sleep(n * time.Second)`.

### 4. No `DBOS.getWorkflowInput` Equivalent

DBOS provides `DBOS.getWorkflowInput()` for accessing workflow input. Cleat passes
input as function parameters.

- **Gap**: No runtime input introspection.
- **Workaround**: Input is available directly as function parameters. Use standard
  Python/Go patterns for accessing it.

### 5. No Transactional Outbox

DBOS's core innovation is the transactional outbox pattern: workflow state changes
and side effects are committed atomically. Cleat uses an event-sourced model where
each host call is recorded as an event.

- **Gap**: Different durability model. DBOS guarantees exactly-once execution for
  the entire workflow function; Cleat guarantees exactly-once for each recorded
  event (host call) with deterministic replay.
- **Workaround**: Design workflows to be idempotent. Use Cleat's idempotency key
  support for workflow start. The `call` event sourcing model provides
  equivalent durability guarantees through replay.

### 6. No Decorator-Based Step Configuration

DBOS configures retries, roles, and timeouts on step decorators. Cleat configures
these at the `call` call site.

- **Workaround**: Create wrapper functions for common call patterns:

```python
def db_call(service, op, request, h, retries=3):
    return h.call_with_retry(
        service, op, request,
        RetryPolicy(max_attempts=retries)
    )
```

### 7. No Automatic HTTP Client

DBOS workflows can use `fetch()` directly since they run in Node.js. Cleat workflows
run in WASM and cannot use `urllib`, `requests`, or `fetch`.

- **Workaround**: Use `h.fetch()` or `h.call("http", "fetch", ...)`
  for all HTTP requests. The host handles the actual network call.

### 8. No Application-Level Versioning

DBOS supports semantic versioning with decorators (`@DBOS.workflow({version: "1.1"})`).
Cleat versions workflows via WASM blob hash.

- **Advantage**: Cleat's WASM-based versioning is simpler — the WASM blob IS the
  version. No manual version strings needed.
- **Workaround**: For explicit version branches, use `h.version()` and
  `h.min_version()` in workflow code.
