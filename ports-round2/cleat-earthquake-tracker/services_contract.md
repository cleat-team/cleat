# Services Contract: Earthquake Tracker

This document describes the host-side services that the Cleat Earthquake Tracker
workflow depends on. The Cleat runtime must provide these services for the
workflow to function correctly.

---

## 1. `http` Service (Standard)

The `http` service is a standard Cleat host service that is assumed to be
available in any Cleat runtime. The workflow uses it to fetch data from the
USGS Earthquake API.

### Operations

#### `fetch`

Performs an HTTP request and returns the response body and status code.

**Request format:**
```json
{
  "url": "https://earthquake.usgs.gov/fdsnws/event/1/query?format=geojson&starttime=...&endtime=...&minmagnitude=1.0",
  "method": "GET",
  "headers": {},
  "body": ""
}
```

**Response format:**
```json
{
  "body": "<response body string>",
  "status": 200
}
```

**Used by:** `HostCalls.durable_fetch()` in `get_earthquake_data()`

---

## 2. `state` Service (Standard)

The `state` service is a standard Cleat host service for durable key-value
storage. State is persisted durably and deterministically replayed during
workflow re-execution.

### Operations

#### `get`

Retrieves a value by key from durable state.

**Request format:**
```json
{
  "key": "earthquake:seen_ids"
}
```

**Response format:** The stored value as JSON (or error if key does not exist).

**Status codes:**
- Success: Returns the stored JSON value.
- Key not found: Host should return an error (RuntimeError caught by workflow).

#### `set`

Stores a value by key in durable state.

**Request format:**
```json
{
  "key": "earthquake:seen_ids",
  "value": ["ci40171730", "nc73543321"]
}
```

**Response format:** Any JSON value (ignored by the workflow).

### State Keys

| Key | Type | Purpose |
|-----|------|---------|
| `earthquake:seen_ids` | `list[str]` | Tracks all earthquake IDs processed so far for idempotency |
| `earthquake:data:<id>` | `object` | Stores individual earthquake record data |

---

## 3. `notification` Service (Custom)

The `notification` service is a **custom host service** that must be
implemented by the Cleat host environment. The workflow calls this service
each time a new earthquake is detected and successfully recorded.

### Operations

#### `send`

Sends a notification about a new earthquake detection.

**Request format:**
```json
{
  "type": "new_earthquake",
  "id": "ci40171730",
  "place": "15 km SW of Searles Valley, CA",
  "magnitude": 2.21,
  "timestamp": 1738136375670
}
```

**Response format:**
Any JSON value (result is not used by the workflow).

**Error handling:**
The workflow catches `RuntimeError` from this service and logs the failure
as an error in the workflow result summary but does NOT fail the entire
workflow. This means notification failures are non-fatal.

**Implementation notes:**
The host may implement this service however it sees fit. Options include:
- Send an email via SMTP
- Post a Slack message
- Trigger a webhook
- Write to a log file
- Push a mobile notification

---

## 4. Scheduling (Host Runtime Responsibility)

The Earthquake Tracker workflow does NOT include a built-in scheduler or
cron trigger. The Cleat host runtime is responsible for invoking the
`track_earthquakes` workflow on a regular schedule (e.g., every minute).

### Contract

- The host should invoke the workflow `track_earthquakes` every 60 seconds.
- Each invocation must include a `scheduled_time` parameter in ISO-8601 format
  (e.g., `"2025-01-15T10:00:00+00:00"`).
- The `scheduled_time` should represent the nominal invocation time (the
  wall-clock time at which this run was scheduled, not the actual execution
  time). This is important because the workflow uses this value to calculate
  the USGS query window.
- The host is responsible for idempotency at the scheduling level (ensuring
  each scheduled time is processed exactly once).

### USGS Query Window

The workflow uses the `scheduled_time` to calculate a 1-hour lookback window:

- `end_time = scheduled_time` (the nominal invocation time)
- `start_time = scheduled_time - 1 hour`

This ensures that earthquake data, which is sometimes updated late, is
re-visited in subsequent runs, and any updates to existing records are captured.

---

## 5. Data Flow Summary

```
Host Scheduler (every 60s)
  |
  v
track_earthquakes(scheduled_time)
  |
  ├── durable_fetch("http", "fetch", USGS_URL)
  │     └── USGS Earthquake API (external)
  |
  ├── durable_call("state", "get", "earthquake:seen_ids")
  │     └── Cleat State Service
  |
  ├── for each earthquake:
  │     ├── durable_call("state", "get", "earthquake:data:<id>")
  │     ├── durable_call("state", "set", "earthquake:data:<id>", data)
  │     └── durable_call("notification", "send", notification_data)
  |
  └── durable_call("state", "set", "earthquake:seen_ids", updated_ids)
```

---

## 6. Service Registration

When deploying the workflow, the Cleat host must register the following
services:

```python
# Pseudocode for host-side registration (host-runtime-specific)
host.register_service("http", http_handler)
host.register_service("state", state_handler)
host.register_service("notification", notification_handler)  # Custom
```

The `http` and `state` services are typically built into the Cleat runtime.
The `notification` service must be provided by the deploying application.
