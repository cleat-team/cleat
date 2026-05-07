# Restate to Cleat Migration Guide

Migrate your Restate workflows (services) to the Cleat durable execution framework.
This guide covers conceptual mapping, API differences, code examples, and known gaps.

---

## Conceptual Mapping

| Restate Concept | Cleat Equivalent | Notes |
|---|---|---|
| Service / Handler | `@cleat_entry` function | Both use function-as-entry-point model |
| `ctx.call()` | `call()` | Similar: calls another service with replay |
| `ctx.sleep(duration)` | `sleep(ms)` | Unit difference (see below) |
| `ctx.oneWayCall()` | `send()` | Fire-and-forget semantics |
| `ctx.sideEffect()` | `call("side_effect", ...)` | No direct equivalent; use service call |
| `ctx.awakeable()` / `ctx.awakeableWithId()` | `create_promise()` + `await_promise()` | Similar: external resolution |
| `ctx.resolveAwakeable(id, value)` | `resolve_promise()` | Same pattern |
| `ctx.rejectAwakeable(id, reason)` | `reject_promise()` | Same pattern |
| `ctx.run()` | `call()` | Non-deterministic code wrapped in host call |
| Journal / Log | `log()` | Similar deterministic logging |
| `ctx.serviceClient()` | `child_workflow()` | Starting another workflow |
| State keys (`ctx.get`/`ctx.set`/`ctx.clear`) | `get_state()` / `set_state()` / `delete_state()` | Similar key-value state |
| `ctx.endpointContext()` | `current_workflow_id()` / `current_run_id()` | Identity info as host calls |
| Virtual Objects | `set_scope()` / virtual object decorator | Cleat has explicit scope management |
| Idempotency / deduplication | Idempotency-Key header | REST-level idempotency |
| `ctx.requestId()` | `current_run_id()` | Similar run identity |
| Ingress (HTTP → handler) | REST API (`POST /api/workflows/:name/start`) | Both have HTTP endpoints |
| `ctx.backgroundCall()` / `ctx.send()` | `send()` | Fire-and-forget async execution |
| `ctx.rpc()` | `call()` | Blocking service call with reply |
| `ctx.consumeEvent()` | `await_signals()` | Event-driven triggers |

---

## API Differences

### Service Definition

**Restate (TypeScript):**
```typescript
import * as restate from "@restatedev/restate-sdk";

const myService = restate.service({
    name: "myService",
    handlers: {
        myWorkflow: async (ctx: restate.Context, input: MyInput) => {
            // ...
        },
    },
});

export type MyService = typeof myService;
```

**Restate (Java):**
```java
@RestateService
public class MyService {
    @Handler
    public String myWorkflow(RestateContext ctx, String input) {
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

### Service Call

**Restate — `ctx.call()`:**
```typescript
const result = await ctx.call(PaymentService, "charge", {
    amount: 100,
    currency: "USD",
});
```

**Cleat (Python):**
```python
result = h.call("payment", "charge", {"amount": 100, "currency": "USD"})
```

### Sleep / Timer

**Restate:**
```typescript
await ctx.sleep(java.time.Duration.ofSeconds(5));
```

**Cleat (Python) — note milliseconds:**
```python
h.sleep(5000)  # 5 seconds
```

**Cleat (Go):**
```go
h.Sleep(5 * time.Second)
```

### Side Effect

**Restate — `ctx.sideEffect()`:**
```typescript
const randomId = await ctx.sideEffect(() => UUID.randomUUID().toString());
```

**Cleat (Python):**
```python
deterministic_id = h.uuid("seed")
```

**Cleat (Go):**
```go
id := h.UUID("seed")
```

Cleat's `uuid()` method provides deterministic UUIDs that are stable across replays
without requiring a side-effect wrapper.

### External Promise (Awakeable)

**Restate — `ctx.awakeable()`:**
```typescript
const awakeable = ctx.awakeableWithId<string>("my-awakeable-id");
// Send awakeable.id to external system
const result = await awakeable.promise;
```

**Restate — resolution (external):**
```typescript
// POST /restate/awakeable/{awakeableId}/resolve
restateClient.resolveAwakeable(awakeableId, "resolutionValue");
```

**Cleat (Python):**
```python
promise_id = h.create_promise("my-promise")
# Send promise_id to external system
result = h.await_promise(promise_id, 60000)
```

**Cleat — resolution (external):**
```python
client = CleatClient("http://localhost:8080")
client.resolve_promise(promise_id, "resolutionValue")
```

### State Management

**Restate — key-value state:**
```typescript
const val = await ctx.get<string>("my-key");
await ctx.set("my-key", "new-value");
await ctx.clear("my-key");
```

**Cleat (Python):**
```python
val = h.get_state("my-key", str)
h.set_state("my-key", "new-value")
h.delete_state("my-key")
```

### Starting a Service

**Restate — `ctx.serviceClient()`:**
```typescript
const client = ctx.serviceClient(MyChildService);
const result = await client.myHandler(input);
```

**Cleat (Python):**
```python
run_id = h.child_workflow("my_child", input_data)
result = h.await_child(run_id)
```

---

## Before/After Examples

### Order Processing Service

**Restate (TypeScript) — before:**
```typescript
import * as restate from "@restatedev/restate-sdk";

const orderService = restate.service({
    name: "orderService",
    handlers: {
        placeOrder: async (ctx: restate.Context, order: OrderInput) => {
            // Step 1: Reserve inventory
            const reserveResult = await ctx.call(
                InventoryService,
                "reserve",
                { items: order.items }
            );

            // Step 2: Charge payment
            let paymentResult;
            try {
                paymentResult = await ctx.call(
                    PaymentService,
                    "charge",
                    { total: order.total }
                );
            } catch (error) {
                // Compensate: release inventory
                await ctx.call(
                    InventoryService,
                    "release",
                    { reservationId: reserveResult.reservationId }
                );
                throw error;
            }

            // Step 3: Wait for approval (external signal)
            const approval = ctx.awakeableWithId<string>(
                `approval-${order.orderId}`
            );
            // Pass approval.id to external system...

            const approvalResult = await approval.promise;

            // Step 4: Create shipment
            const shipResult = await ctx.call(
                ShippingService,
                "createShipment",
                { orderId: order.orderId }
            );

            await ctx.set("status", "confirmed");
            return { status: "shipped", trackingId: shipResult.trackingId };
        },
    },
});
```

**Cleat (Go) — after:**
```go
//go:cleat_entry
func PlaceOrder(h cleat.HostCalls, order OrderInput) (OrderResult, error) {
    h.Log("Placing order " + order.OrderID)

    // Step 1: Reserve inventory
    reserveResp, err := h.Call("inventory", "Reserve",
        marshal(map[string]interface{}{"items": order.Items}))
    if err != nil {
        return OrderResult{}, err
    }
    var reserveResult ReserveResult
    json.Unmarshal([]byte(reserveResp), &reserveResult)

    // Step 2: Charge payment
    chargeResp, err := h.Call("payment", "charge",
        marshal(map[string]interface{}{"total": order.Total}))
    if err != nil {
        // Compensate: release inventory
        h.Call("inventory", "release",
            marshal(map[string]interface{}{"reservationId": reserveResult.ReservationID}))
        return OrderResult{}, err
    }

    // Step 3: Wait for approval promise (external resolution)
    promiseID, err := h.CreatePromise("approval-" + order.OrderID)
    if err != nil {
        return OrderResult{}, err
    }
    // Pass promiseID to external system via REST API

    approvalResult, timedOut, err := h.AwaitPromise(promiseID, 5*time.Minute)
    if timedOut || err != nil {
        // Compensate
        h.Call("payment", "refund", chargeResp)
        h.Call("inventory", "release",
            marshal(map[string]interface{}{"reservationId": reserveResult.ReservationID}))
        return OrderResult{}, fmt.Errorf("approval timed out")
    }

    // Step 4: Create shipment
    shipResp, err := h.Call("shipping", "createShipment",
        marshal(map[string]interface{}{"orderId": order.OrderID}))
    if err != nil {
        return OrderResult{}, err
    }
    var shipResult ShipResult
    json.Unmarshal([]byte(shipResp), &shipResult)

    h.SetState("status", "confirmed")
    return OrderResult{
        Status:     "shipped",
        TrackingID: shipResult.TrackingID,
    }, nil
}
```

**Cleat (Python) — after:**
```python
@cleat_entry
def place_order(h: HostCalls, order: dict) -> str:
    h.log(f"Placing order {order['orderId']}")

    # Step 1: Reserve inventory
    reserve_resp = h.call("inventory", "Reserve", {"items": order["items"]})
    reserve_data = json.loads(reserve_resp)

    # Step 2: Charge payment
    try:
        charge_resp = h.call("payment", "charge", {"total": order["total"]})
    except Exception:
        h.call("inventory", "release", {"reservationId": reserve_data["reservationId"]})
        raise

    # Step 3: Wait for approval
    promise_id = h.create_promise(f"approval-{order['orderId']}")
    approval_result = h.await_promise(promise_id, 300000)
    if approval_result.timed_out:
        # Compensate
        h.call("payment", "refund", json.loads(charge_resp))
        h.call("inventory", "release", {"reservationId": reserve_data["reservationId"]})
        return json.dumps({"error": "approval timed out"})

    # Step 4: Create shipment
    ship_resp = h.call("shipping", "createShipment", {"orderId": order["orderId"]})
    ship_data = json.loads(ship_resp)

    h.set_state("status", "confirmed")
    return json.dumps({"status": "shipped", "trackingId": ship_data.get("trackingId", "")})
```

---

## Known Gaps and Workarounds

### 1. No `ctx.call()` Service Client API

Restate provides typed service clients (`ctx.serviceClient(PaymentService)`) that
provide compile-time type safety when calling other services. Cleat uses string-based
service and operation names.

- **Gap**: No compile-time type checking for cross-workflow calls.
- **Workaround**: Define strongly typed wrapper functions:

```python
def call_payment_charge(h: HostCalls, amount: int) -> PaymentResult:
    resp = h.call("payment", "charge", {"amount": amount})
    return PaymentResult(**json.loads(resp))
```

In Go, use `CallTyped()` for type-safe calls:
```go
var result ChargeResult
err := h.CallTyped("payment", "charge", request, &result)
```

### 2. No Virtual Object Keyed State (Automatic)

Restate has built-in virtual object state that is automatically scoped to the
object key. Cleat requires explicit scope management.

- **Gap**: Virtual object state requires `set_scope()` / `clear_scope()` calls.
- **Workaround**: Use Cleat's scope API:

```python
@virtual_object("shopping_cart")
def cart_handler(h: HostCalls, input: str) -> str:
    prev = h.set_scope("shopping_cart", extract_key(input))
    try:
        state = h.get_state("items", list)
        # ...
    finally:
        h.clear_scope()
```

### 3. Unit Differences

- **Gap**: Restate uses `Duration` objects (Java) or `ctx.sleep(Duration.ofSeconds(n))`;
  Cleat uses milliseconds (`sleep(ms)`).
- **Workaround**: Multiply by 1000: `h.sleep(restate_seconds * 1000)`.
  In Go, use `h.Sleep(n * time.Second)`.

### 4. No Direct `ctx.sideEffect()` Equivalent

Restate's `ctx.sideEffect()` runs non-deterministic code (UUID generation, random
number generation) once and records the result. Cleat's host calls are always
replayed deterministically.

- **Workaround**: Use `h.uuid(seed)` for deterministic UUIDs. For other non-deterministic
  code, wrap it in a service call via `call()`.

### 5. No Service Discovery / Registration

Restate has an "ingress" that routes HTTP requests to handlers, and services must
be deployed and registered. Cleat deploys WASM blobs to a database.

- **Gap**: Different deployment model. Cleat deploys workflow code as WASM blobs,
  not as network services.
- **Workaround**: Use the `cleat deploy` CLI command to deploy workflow definitions.
  REST API endpoints are auto-generated from deployed workflow names.

### 6. No `ctx.backgroundCall()` / `ctx.send()` Distinction

Restate distinguishes between `ctx.oneWayCall()` (fire-and-forget, not awaiting
the result), `ctx.send()` (delayed invocation), and `ctx.rpc()` (blocking call).
Cleat has `send()` (fire-and-forget), `schedule_invoke()` (delayed), and
`call()` (blocking).

- **Workaround**: Map directly:
  - `ctx.call()` / `ctx.rpc()` -> `call()`
  - `ctx.oneWayCall()` -> `send()`
  - `ctx.send()` with delay -> `schedule_invoke()`

### 7. No Awakeable ID Generation

Restate generates awakeable IDs that are globally unique. Cleat generates promise
IDs from the host.

- **Gap**: Promise IDs are generated by the host and returned from `create_promise()`.
  You cannot choose a custom promise ID.
- **Workaround**: Pass the returned promise ID to the external system that needs
  to resolve or reject it.

### 8. No Built-in Deduplication

Restate provides exactly-once execution for service calls. Cleat provides idempotency
key support for workflow start (REST layer), but not for individual durable calls.

- **Workaround**: Use `call_with_retry()` with idempotent service operations.
  The event-sourced model ensures each durable call is recorded once and replayed
  deterministically, which provides effectively-once semantics.
