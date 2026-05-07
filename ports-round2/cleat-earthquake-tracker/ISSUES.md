# Issues Encountered During Porting

This document records every issue, gap, bug, or missing feature found while
porting the DBOS earthquake-tracker to the Cleat Python SDK.

---

## Issue 1: No Scheduled / Cron Decorator

- **Severity:** Critical
- **Category:** Missing Feature

### Description

DBOS provides a `@DBOS.scheduled("* * * * *")` decorator that handles cron-style
scheduling of workflow functions. Cleat's Python SDK has no equivalent.

In the original app:
```python
@DBOS.scheduled("* * * * *")
@DBOS.workflow()
def run_every_minute(scheduled_time: datetime, actual_time: datetime):
    ...
```

### Expected Behavior

Cleat should provide a `@scheduled` or `@cron` decorator (or at minimum, a
configuration mechanism) that lets workflow authors express "run this workflow
every N minutes" without writing host-side scheduling code.

### Workaround

The host runtime is responsible for invoking the workflow on a schedule. The
workflow accepts a `scheduled_time` parameter (ISO-8601 string) from the
invoker. This is documented in `services_contract.md` as a host responsibility.

### Recommendation

Add a `@scheduled(cron_expr: str)` decorator to the Cleat Python SDK that
behaves like DBOS's `@DBOS.scheduled()`. The decorator should:
1. Register the cron expression with the host runtime at deployment time.
2. Inject `scheduled_time` and (optionally) `actual_time` parameters into
   the workflow call.
3. Ensure exactly-once semantics for each scheduled tick.

---

## Issue 2: No Built-in Database / Transaction Support

- **Severity:** High
- **Category:** Missing Feature

### Description

DBOS provides `@DBOS.transaction()` for reliable database operations within
workflows. It provides a `DBOS.sql_session` that integrates with SQLAlchemy
for Postgres. Cleat's SDK has no database integration at all.

In the original app:
```python
@DBOS.transaction()
def record_earthquake_data(data: EarthquakeData) -> bool:
    return DBOS.sql_session.execute(
        insert(earthquake_tracker)
        .values(**data)
        .on_conflict_do_update(index_elements=["id"], set_=data)
        .returning(text("xmax = 0 AS inserted"))
    ).scalar_one()
```

### Expected Behavior

Cleat should provide either:
a) A `@transaction` decorator and `cleat_session` for SQL database access
   (analogous to DBOS's SQLAlchemy integration).
b) A `db` plugin or service for structured data persistence.
c) At minimum, documentation on how to integrate external databases safely
   within workflows.

### Workaround

Use Cleat's built-in key-value `state` service for all data persistence.
Earthquake records are stored as individual state items keyed by ID, and
a separate `earthquake:seen_ids` key tracks which IDs have been processed.
This is adequate for this toy application but would not scale to production
use cases requiring relational queries, joins, or indexing.

### Recommendation

Add a `@transaction` decorator and/or a `Postgres` plugin that wraps
SQLAlchemy, with replay-safe database operations. This is a critical gap
for porting real applications to Cleat.

---

## Issue 3: `HostCalls.get_state()` Has Problematic Type Coercion

- **Severity:** Medium
- **Category:** Bug / Design Flaw

### Description

The `HostCalls.get_state(key, result_type)` method has type coercion logic
that can silently corrupt data. Specifically:

```python
def get_state(self, key: str, result_type: type[T]) -> T:
    result = self.durable_call("state", "get", {"key": key})
    data = json.loads(result)
    if isinstance(data, dict):
        return result_type(**data)  # dict(**{"k": "v"}) works
    return result_type(data)  # str(["a","b"]) = "['a', 'b']" (invalid JSON!)
```

When `result_type` is `str` and the stored data is a list (e.g., `["a", "b"]`):
1. `json.loads()` deserializes to the Python list `["a", "b"]`.
2. It is NOT a dict, so the `dict` branch is skipped.
3. `str(["a", "b"])` returns `"['a', 'b']"` -- which is **not valid JSON**.

This means `get_state("list_key", str)` returns a non-JSON Python repr string
instead of the original stored JSON string.

### Expected Behavior

When `result_type` is `str` and the stored data is a non-dict JSON type,
`get_state` should return the original JSON string, not the Python repr.
Alternatively, for `list` types, `result_type` should receive the list directly.

### Workaround

Avoid `get_state()` for complex types. Instead, call `durable_call("state", "get", ...)`
directly and parse the result with `json.loads()`. See the `_state_get()` helper
in `main.py` for the recommended pattern.

```python
def _state_get(h: HostCalls, key: str) -> Any:
    try:
        result = h.durable_call("state", "get", {"key": key})
        return json.loads(result)
    except RuntimeError:
        return None
```

### Recommendation

Fix `get_state()` to handle non-dict types correctly:
- When `result_type` is `str` and `data` is not a dict, return the raw
  `durable_call` response string, or serialize `data` back to JSON with
  `json.dumps(data)` rather than `str(data)`.
- Better: add a `json.dumps` call instead of `str()`.

---

## Issue 4: No Direct HTTP Libraries in Workflows (Non-Determinism)

- **Severity:** Critical
- **Category:** Documentation / Guidance Gap

### Description

The original DBOS app uses `requests.get()` directly in a `@DBOS.step()`.
In Cleat, calling `requests.get()` inside a workflow entry point would be
non-deterministic (it would fail on replay or produce different results).

### Expected Behavior

Cleat should provide clear guidance and examples showing how to make HTTP
requests deterministically. The SDK does provide `durable_fetch()` which
delegates to the `http` service, but there is no warning or linting to
prevent developers from using `requests` directly.

### Workaround

Always use `HostCalls.durable_fetch()` (or `durable_call("http", "fetch", ...)`)
for HTTP requests within workflows. This delegates to the host runtime which
caches responses for deterministic replay.

### Recommendation

- Add a section to the SDK docs titled "Durable HTTP Requests" with examples.
- Consider adding a linter rule that warns when `requests`, `urllib`, or other
  non-deterministic network calls appear inside `@durable_entry` functions.

---

## Issue 5: No Equivalent to `DBOS.logger`

- **Severity:** Low
- **Category:** API Gap

### Description

DBOS provides `DBOS.logger` as a first-class workflow-aware logger. Cleat
provides `HostCalls.durable_log(message)` which logs to the event history.
However:

1. `durable_log` only accepts a single string, not a format string with args.
2. There is no concept of log levels (info, warning, error).
3. There is no way to get a logger instance for use outside of workflows.

### Expected Behavior

A Python-standard compatible logger that:
- Integrates with the structured `logging` module.
- Supports log levels (debug, info, warning, error).
- Can be used in both workflow and non-workflow code.

### Workaround

Use `h.durable_log(f"formatted {value}")` with f-strings inside workflows.
For code outside workflows, use standard Python `logging.getLogger()`.

### Recommendation

Consider adding a `WorkflowLogger` that wraps `durable_log` and implements
the standard `logging.Logger` interface with level support.

---

## Issue 6: `HostCalls` Methods Raise `NotImplementedError` Outside WASM

- **Severity:** Medium
- **Category:** Developer Experience

### Description

All `_import_*` functions in `host_calls.py` raise `NotImplementedError`
when called outside a WASM runtime. While this is expected (they are FFI stubs),
it means developers cannot test any workflow code by importing and running it
directly -- they MUST use `CleatTestHarness`.

The error message is clear, but new users may be confused when their workflow
code fails with `NotImplementedError: durable_sleep can only be called within
a cleat WASM runtime.`

### Expected Behavior

Either:
a) Provide a "local mode" that uses real Python implementations instead of
   WASM FFI (like Temporal's dev server).
b) Make the `CleatTestHarness` the default when no WASM runtime is detected.

### Workaround

Always use `CleatTestHarness` for testing. The harness provides all the same
methods with in-memory implementations.

### Recommendation

Add a `LocalHostCalls` class that provides real local implementations (e.g.,
real HTTP calls via `requests`, real state via SQLite, etc.) for development
and testing without WASM. This would be similar to DBOS's local dev mode.

---

## Issue 7: No Workflow ID / Run ID Support in `CleatTestHarness`

- **Severity:** Low
- **Category:** Missing Feature in Test Harness

### Description

The `CleatTestHarness` has hardcoded `_workflow_id = "test-workflow-id"` and
`_run_id = "test-run-id"`. There is no way to set custom IDs per test case or
simulate different workflow identity scenarios.

### Expected Behavior

The test harness should allow setting custom workflow ID and run ID, either
via constructor parameters or setter methods.

### Workaround

No workaround needed for this project (we don't use workflow IDs), but it
would be helpful for testing workflows that use `current_workflow_id()` or
`current_run_id()`.

### Recommendation

Add optional `workflow_id` and `run_id` parameters to `CleatTestHarness.__init__()`.

---

## Issue 8: `CleatTestHarness` State Calls Do Not Persist Across Calls

- **Severity:** Medium
- **Category:** Test Harness Gap

### Description

In `CleatTestHarness`, `set_state()` and `get_state()` both delegate to
`durable_call("state", "set/get", ...)`, which uses the FIFO stub queue.
Each `get_state` call consumes a stub. If the workflow makes multiple
state operations, every single operation must be pre-registered as a stub
in the exact order they will be called.

This means the harness does NOT maintain actual state storage -- it is just
a stub-based mock. A `set(key, value)` followed by a `get(key)` in the same
test will NOT return the value that was just set unless you explicitly stub
the `get` response.

### Expected Behavior

`CleatTestHarness.set_state(key, value)` should store the value internally,
and `get_state(key)` should retrieve it from that internal store, without
requiring individual stubs for each state operation.

### Workaround

Every state operation must be pre-stubbed in the exact call order. This is
tedious but workable. See the test file for the pattern:

```python
h.stub_call("state", "get", error="not found")     # Check if exists
h.stub_call("state", "set", response=json.dumps(None))  # Save
```

### Recommendation

Add genuine in-memory state storage to `CleatTestHarness`:

```python
class CleatTestHarness(HostCalls):
    def __init__(self):
        ...
        self._real_state: dict[str, Any] = {}
    
    def set_state(self, key: str, value: Any) -> None:
        self._real_state[key] = value
        super().set_state(key, value)  # also records call
    
    def get_state(self, key: str, result_type: type = str) -> Any:
        if key in self._real_state:
            return self._real_state[key]
        return super().get_state(key, result_type)
```

---

## Issue 9: No Equivalent to `DBOS.step()` for Non-Durable Helper Functions

- **Severity:** Low
- **Category:** API Gap

### Description

DBOS provides `@DBOS.step()` for marking helper functions that can be called
from within workflows. These are not themselves entry points but are recorded
as steps in the workflow execution. Cleat has no equivalent decorator.

In the original app:
```python
@DBOS.step()
def get_earthquake_data(start_time: datetime, end_time: datetime) -> list[EarthquakeData]:
    ...
```

### Expected Behavior

Cleat should provide a way to mark helper functions as workflow steps for
better observability and organization of workflow logic. This could be:
a) A `@step` decorator that accepts a `HostCalls` parameter.
b) Documentation that all workflow logic should be in the entry point function.

### Workaround

Define regular Python functions that accept `HostCalls` as their first
parameter. These functions are called directly from the entry point. They
work identically but lack the explicit "step" semantic.

### Recommendation

Consider adding a `@step` decorator if the event history needs to distinguish
individual steps. For now, regular Python functions with `HostCalls: h` as
the first parameter are sufficient.

---

## Issue 10: `durable_fetch` Doesn't Support Query Parameters Dict

- **Severity:** Medium
- **Category:** API Gap

### Description

The `HostCalls.durable_fetch()` method accepts only a URL string, not a
separate query parameters dict. This means callers must manually construct
the URL with query parameters using `urllib.parse.urlencode()`.

In the original DBOS app, `requests.get()` conveniently accepts both a base
URL and a `params` dict.

### Expected Behavior

`durable_fetch()` (or a new `durable_fetch_with_params()`) should accept an
optional `params: dict` that gets URL-encoded automatically.

### Workaround

Use `urllib.parse.urlencode()` to append query parameters to the URL before
calling `durable_fetch()`. See `_build_usgs_url()` in `main.py`.

### Recommendation

Add an optional `params` parameter to `durable_fetch()` that is automatically
URL-encoded and appended to the URL.

---

## Issue 11: No Support for `datetime` Objects in JSON Serialization

- **Severity:** Low
- **Category:** SDK Gap

### Description

When passing `datetime` objects as workflow input or state, the SDK's internal
`json.dumps()` calls will fail because `datetime` objects are not natively
JSON-serializable. The SDK uses `default=str` in `durable_entry` output
serialization but not consistently in all places.

### Expected Behavior

The SDK should handle common types like `datetime` gracefully, either
by converting them to ISO-8601 strings automatically or by providing a
custom JSON encoder.

### Workaround

Convert `datetime` objects to ISO-8601 strings before passing them to any
HostCalls method. This is why our workflow takes `scheduled_time: str`
instead of `scheduled_time: datetime`.

### Recommendation

Add a custom JSON encoder (e.g., `CleatJSONEncoder`) that handles `datetime`,
`date`, `Decimal`, `UUID`, and other common types.

---

## Summary

| # | Severity | Category | Title |
|---|----------|----------|-------|
| 1 | Critical | Missing Feature | No Scheduled / Cron Decorator |
| 2 | High | Missing Feature | No Built-in Database / Transaction Support |
| 3 | Medium | Bug / Design Flaw | `HostCalls.get_state()` Problematic Type Coercion |
| 4 | Critical | Guidance Gap | No Direct HTTP Libraries in Workflows |
| 5 | Low | API Gap | No Equivalent to `DBOS.logger` |
| 6 | Medium | Developer Experience | `HostCalls` Methods Raise `NotImplementedError` Outside WASM |
| 7 | Low | Test Harness Gap | No Workflow ID / Run ID Support in `CleatTestHarness` |
| 8 | Medium | Test Harness Gap | `CleatTestHarness` State Calls Do Not Persist |
| 9 | Low | API Gap | No Equivalent to `DBOS.step()` |
| 10 | Medium | API Gap | `durable_fetch` Doesn't Support Query Parameters Dict |
| 11 | Low | SDK Gap | No Support for `datetime` Objects in JSON Serialization |
