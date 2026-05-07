# Services Contract

The transactional-outbox workflow relies on two host-side services that are
called via `HostCalls.durable_call(service, operation, request)`.  These
services run **outside** the WASM sandbox, on the Cleat host runtime.

In a production deployment, these services must be registered with the
Cleat host runtime.  The workflow sends JSON-serialised dicts as request
payloads and expects JSON strings as responses.

---

## 1. Database Service (`"db"`)

Provides CRUD operations on the `orders` table.

### `insert_order`

Insert an order row and return the new `order_id`.

**Request:**

```json
{
  "customer": "Alice",
  "item": "Widget",
  "quantity": 2
}
```

**Response:**

```json
{
  "order_id": 42
}
```

### `update_notification_status`

Mark an order's notification status.

**Request:**

```json
{
  "order_id": 42,
  "status": "SENT"
}
```

**Response:**

```json
{
  "status": "ok"
}
```

### `create_orders_table`

Ensure the orders table exists (called at startup).

**Request:** `{}`

**Response:**

```json
{
  "status": "ok"
}
```

### `list_orders`

Return all orders (called directly by the FastAPI endpoint, not via the
workflow).

**Request:** not applicable (called directly)

**Response:**

```json
[
  {
    "order_id": 42,
    "customer": "Alice",
    "item": "Widget",
    "quantity": 2,
    "notification_status": "SENT",
    "created_at": "2025-01-15T10:30:00"
  }
]
```

---

## 2. Notifier Service (`"notifier"`)

Simulates publishing a message to a message broker (Kafka, RabbitMQ, SQS,
webhook, etc.).

### `send_notification`

Publish an order confirmation notification.

**Request:**

```json
{
  "order_id": 42,
  "customer": "Alice",
  "item": "Widget"
}
```

**Response:**

```json
{
  "status": "sent"
}
```

---

## 3. State Service (`"state"`) — built-in

Cleat provides a built-in `"state"` service for durable state management.
The workflow uses it indirectly via `HostCalls.set_query_state()` and
`HostCalls.get_state()`.

### `set_query_state`

Called by the workflow to expose the `order_id` for external retrieval.

**Request:** `(key, value)` — stores `"{run_id}:order_id" -> "42"`

Retrieved by the FastAPI app via the LocalRuntime or CleatClient API.

---

## Service Registration for Production

When deploying to a Cleat host, these services must be registered as
JSON-RPC or REST endpoints that the Cleat runtime can call.  The exact
registration mechanism depends on the deployment environment.

For local development, the `LocalRuntime` class routes `durable_call`
directly to Python service instances, bypassing the need for a real
Cleat host.
