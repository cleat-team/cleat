# Transactional Outbox — Cleat Port

A port of the DBOS transactional-outbox demo app
(`dbos-demo-apps/python/transactional-outbox/`) using the **Cleat** Python
durable execution SDK.

## Architecture

```
HTTP Request  ──►  FastAPI  ──►  LocalRuntime / CleatClient
                                      │
                          ┌───────────┴───────────┐
                          │                       │
                    Workflow (WASM)           Host Services
                    @durable_entry             (Python)
                          │                       │
              ┌───────────┤              ┌────────┴────────┐
              │           │              │                 │
         durable_call  set_query_    DBService      NotifierService
         ("db", ...)   state(...)    (Postgres)     (message broker)
```

The **workflow** (in WASM) coordinates the order lifecycle:
1. Insert order in DB
2. Expose `order_id` via query state
3. Publish notification
4. Mark notification sent

The **host services** (outside WASM) execute the actual work:
- `DBService` — PostgreSQL (or SQLite in tests)
- `NotifierService` — simulated message broker

The **FastAPI app** receives HTTP requests, starts the workflow, and
retrieves the `order_id` via query state so it can return immediately
while the workflow continues with the notification.

## Setup

```bash
# 1. Install dependencies
uv sync

# 2. Set database URL (PostgreSQL)
export DATABASE_URL="postgresql+psycopg://postgres@localhost:5432/transactional_outbox"

# 3. Run the app
uv run python main.py
```

Or run with SQLite for development (no Postgres needed):

```bash
export DATABASE_URL="sqlite:///outbox.db"
uv run python main.py
```

Visit http://localhost:8000 to place orders and watch notifications.

## Running Tests

```bash
# Unit tests (CleatTestHarness stubs)
uv run pytest test_outbox.py -v

# Integration tests (SQLite in-memory)
uv run pytest test_outbox.py -v -k "Integration or Edge"
```

## Project Files

| File | Purpose |
|---|---|
| `workflow.py` | `@durable_entry` workflow function (WASM-compilable) |
| `services.py` | Host-side service implementations (DB, notifier) |
| `runtime.py` | Local in-process runtime for development |
| `app.py` | FastAPI application with REST endpoints |
| `test_outbox.py` | Unit and integration tests |
| `services_contract.md` | Host service API contract |
| `ISSUES.md` | SDK gaps and issues found during porting |

---

## DBOS → Cleat API Migration Reference

Every DBOS API call used in the original app, mapped to its Cleat equivalent.

### Core Concepts

| DBOS | Cleat | Notes |
|---|---|---|
| `@DBOS.workflow()` | `@durable_entry("name")` | Cleat requires an explicit name parameter |
| `@DBOS.transaction()` | No direct equivalent | All DB access via `durable_call("db", ...)` |
| `@DBOS.step()` | `durable_call("service", "op", ...)` | Cleat uses recorded durable calls instead of bare steps |
| `DBOS(config=...)` | `CleatClient(base_url=...)` | Different configuration model |
| `DBOS.launch()` | Cleat host runtime | Cleat runs as a separate host process |
| `DBOS.sql_session` | Host `DBService` | Cleat has no direct SQL access from workflows |

### API Call Mapping

| Original DBOS Call | Cleat Equivalent | File:Line |
|---|---|---|
| `@DBOS.workflow()` | `@durable_entry("place_order")` | `workflow.py:30` |
| `@DBOS.transaction()` | Service method + `durable_call` | `services.py` |
| `@DBOS.step()` | `h.durable_call("notifier", ...)` | `workflow.py:59` |
| `DBOS.sql_session.execute(...)` | `h.durable_call("db", "insert_order", ...)` | `workflow.py:42` |
| `DBOS.set_event(key, value)` | `h.set_query_state(key, value)` | `workflow.py:54` |
| `DBOS.get_event(wf_id, key)` | `runtime.wait_for_query_state(run_id, key)` | See Issue #2 |
| `DBOS.start_workflow(fn, ...)` | `runtime.start_workflow(fn, ...)` or `client.start_workflow(name, input)` | `app.py:60` |
| `DBOS.logger.info(...)` | `h.durable_log(...)` | (not used in this port) |
| No outbox table needed | No outbox table needed | Both provide durability guarantees |

### Pattern Mapping

#### 1. Outbox Pattern

**DBOS:** Events are not explicitly written to an outbox table. The workflow
engine journals all operations and replays on failure. `@DBOS.workflow()`
wraps the business logic, and `@DBOS.transaction()` ensures atomic DB writes.

**Cleat:** Same concept. `@durable_entry` wraps the workflow, and
`HostCalls.durable_call()` journals all external operations. The host
runtime ensures exactly-once execution. No outbox table is needed in
either framework.

#### 2. Database Transactions

**DBOS:** `@DBOS.transaction()` provides a managed SQLAlchemy session
(`DBOS.sql_session`) with automatic rollback on failure.

**Cleat:** Database operations are defined as **host services** and called
via `durable_call("db", "operation", payload)`. The Cleat host runtime
manages the actual connection. The workflow coordinates but does not
directly touch the database.

#### 3. Message Broker Publishing

**DBOS:** `@DBOS.step()` wraps non-deterministic side-effects. The step
is retried until it succeeds.

**Cleat:** `durable_call("notifier", "send_notification", payload)` serves
the same purpose. The call is journaled and replayed deterministically.
For Kafka, use the `Plugins.kafkaconnect.produce()` method.

#### 4. Exactly-Once Semantics

**DBOS:** Every workflow execution is durably recorded. On crash recovery,
incomplete workflows are retried from the last recorded step. Idempotent
operations are the responsibility of the service.

**Cleat:** Same guarantee. `durable_call` is recorded in the event history.
On replay, recorded responses are returned without re-executing the call.
Idempotency must still be handled at the service level.

#### 5. Event Communication (set_event/get_event)

**DBOS:** `set_event` in the workflow, `get_event` in the API handler.
The API blocks until the event is available.

**Cleat:** `set_query_state` in the workflow. **No blocking getter
exists.** The API must poll. See ISSUES.md #2.

---

## Key Differences from the Original

1. **No `@DBOS.transaction()`**: All DB access is via `durable_call` to
   host services.  The workflow is a pure coordinator.
2. **No `set_event`/`get_event`**: Use `set_query_state` + polling instead.
3. **Separate service contract**: Host services must be explicitly defined
   and registered.  There is no "auto-magic" SQLAlchemy session.
4. **WASM sandbox**: Workflow functions run in a WASM sandbox.  No direct
   I/O, no direct imports of DB drivers, HTTP clients, etc.
5. **`@durable_entry` wraps the function**: Access the original via
   `__wrapped__` for direct testing.  See ISSUES.md #1.
6. **LocalRuntime needed**: The SDK lacks an in-process runtime for
   development.  See ISSUES.md #3.

## Production Deployment

For production, the workflow would be compiled to WASM and deployed to a
Cleat host.  The FastAPI app would use `CleatClient` instead of
`LocalRuntime`:

```python
from cleat_sdk import CleatClient

client = CleatClient(base_url="http://cleat-host:8080")

@app.post("/orders")
def create_order(request: OrderRequest):
    run_id = client.start_workflow(
        "place_order",
        {
            "customer": request.customer,
            "item": request.item,
            "quantity": request.quantity,
        },
    )
    # Poll for order_id via Cleat host's query state API
    order_id = _wait_for_state(client, run_id, "order_id")
    return {"order_id": order_id}
```

The host services (`DBService`, `NotifierService`) would be registered
as REST endpoints that the Cleat runtime can invoke when the workflow
calls `durable_call`.
