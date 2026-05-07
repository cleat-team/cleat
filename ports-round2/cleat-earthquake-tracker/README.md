# Cleat Earthquake Tracker

A port of the [DBOS Earthquake Tracker](https://github.com/dbos-inc/dbos-demo-apps/tree/main/python/earthquake-tracker)
to the [Cleat](https://cleat.dev) durable execution Python SDK.

This project periodically polls the USGS earthquake API and records new
earthquakes using Cleat's durable execution framework for reliable, exactly-once
processing.

## Architecture

```
Host Scheduler (every 60s)
  |
  v
track_earthquakes(scheduled_time)     <-- @durable_entry workflow
  |
  |-- durable_fetch(USGS_URL)         <-- Durable HTTP via host "http" service
  |-- durable_call("state", ...)      <-- Durable key-value state storage
  |-- durable_call("notification", ..)<-- Custom notification host service
  |
  v
Result: {new_count, total_seen, errors}
```

## Project Structure

```
cleat-earthquake-tracker/
  earthquake_tracker/
    __init__.py        # Public API exports
    main.py            # Workflow definition and helpers
  tests/
    __init__.py
    test_tracker.py    # Test suite using CleatTestHarness
  requirements.txt     # Python dependencies
  services_contract.md # Host service requirements
  ISSUES.md            # Issues found during porting
  README.md            # This file
```

## Migration Notes: DBOS to Cleat API Mapping

### Core Concepts

| DBOS Concept | Cleat Equivalent | Notes |
|-------------|------------------|-------|
| `@DBOS.workflow()` | `@durable_entry(name="...")` | Both mark a function as a durable workflow entry point |
| `@DBOS.step()` | Regular Python function with `HostCalls` param | Cleat has no step decorator; use plain functions |
| `@DBOS.transaction()` | `durable_call("state", "set/get", ...)` | DBOS uses SQL; Cleat uses key-value state |
| `@DBOS.scheduled("* * * * *")` | Host-side scheduling | **No Cleat equivalent** (see Issue 1) |
| `DBOS.logger` | `h.durable_log(msg)` | Cleat logs to event history (string only, no levels) |
| `DBOS.sql_session` | `h.set_state()` / `h.get_state()` | Different storage model entirely |

### Host Calls

| DBOS | Cleat | Notes |
|------|-------|-------|
| `requests.get(url, params)` | `h.durable_fetch(url)` | Deterministic/replay-safe via host |
| `requests.get()` with params dict | `h.durable_fetch(built_url)` | Must construct URL manually (Issue 10) |
| N/A (implicit in DBOS) | `h.durable_call("http", "fetch", req)` | Lower-level alternative to `durable_fetch()` |
| N/A | `h.durable_call("state", "set", dict)` | Store key-value data durably |
| N/A | `h.durable_call("state", "get", dict)` | Retrieve key-value data durably |
| N/A | `h.durable_call("notification", "send", dict)` | Send notification (custom host service) |
| `DBOS.random()` | `h.random()` | Deterministic random |
| `DBOS.now()` | `h.now()` | Deterministic wall clock |
| `DBOS.sleep(ms)` | `h.durable_sleep(ms)` | Durable timer (suspends workflow) |

### State Management

| DBOS | Cleat |
|------|-------|
| Postgres table (`earthquake_tracker`) | State keys `earthquake:data:<id>` |
| SQL upsert with conflict handling | `_state_get()` + `_state_set()` pattern |
| Return value from `RETURNING` | Check `_state_get()` return for `None` |
| Alembic migrations | No schema management needed (schemaless KV) |

### Workflow Parameters

| DBOS | Cleat |
|------|-------|
| `scheduled_time: datetime` | `scheduled_time: str` (ISO-8601) |
| `actual_time: datetime` | Not used (host responsibility) |
| TypedDict `EarthquakeData` | `EarthquakeData` dict subclass |

### Testing

| DBOS | Cleat |
|------|-------|
| `conftest.py` with Postgres fixtures | `CleatTestHarness` (no database needed) |
| Alembic migrations in tests | No migrations needed |
| `mock.patch()` for mocking | `h.stub_call()` for service mocking |
| Database reset for each test | `h.reset()` or fresh harness per test |

## Setup

### Install Dependencies

```bash
# From the ports-round2 directory:
pip install -e /localssd/rcownie/cleat-agent1/python-sdk
```

### Run Tests

```bash
cd cleat-earthquake-tracker
pip install -r requirements.txt
pytest tests/ -v
```

## Deployment

The workflow must be deployed to a Cleat host that provides the services
described in `services_contract.md`. Key requirements:

1. The host must invoke `track_earthquakes` with a `scheduled_time` parameter
   every 60 seconds.
2. The host must register the `notification` service handler.
3. The `http` and `state` services are standard Cleat host services.

## Known Issues

See `ISSUES.md` for a complete list of issues encountered during porting,
including 11 items ranging from Critical (missing cron support) to Low
(minor API gaps).

## Original Project

The original DBOS project is at:
`/localssd/rcownie/cleat-agent1/ports-round2/dbos-demo-apps/python/earthquake-tracker/`

It also includes a Streamlit visualization (`streamlit.py`) which is not
ported here as it is a pure data visualization concern unrelated to durable
execution.
