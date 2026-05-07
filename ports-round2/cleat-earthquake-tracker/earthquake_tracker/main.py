"""
Earthquake Tracker - Cleat SDK Port

Periodically polls the USGS earthquake API and records new earthquakes
using Cleat's durable execution framework for reliable, exactly-once processing.

Original DBOS implementation:
    https://github.com/dbos-inc/dbos-demo-apps/tree/main/python/earthquake-tracker

Key differences from the DBOS version:
    - No Postgres database: uses Cleat's key-value state service for persistence
    - No cron decorator: the host runtime schedules workflow invocations
    - No SQLAlchemy ORM: earthquake data stored as JSON in Cleat state
    - HTTP fetch via HostCalls.durable_fetch() (deterministic/replayable)
    - Notifications via a custom host "notification" service
"""

from __future__ import annotations

import json
import urllib.parse
from datetime import datetime, timedelta, timezone
from typing import Any

from cleat_sdk import HostCalls, durable_entry

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

USGS_API_URL = "https://earthquake.usgs.gov/fdsnws/event/1/query"
STATE_SEEN_IDS_KEY = "earthquake:seen_ids"
STATE_EQ_PREFIX = "earthquake:data:"
MIN_MAGNITUDE = 1.0
LOOKBACK_HOURS = 1

# ---------------------------------------------------------------------------
# Data types
# ---------------------------------------------------------------------------


class EarthquakeData(dict):
    """An earthquake record from the USGS API.

    Preserved as a dict (TypedDict pattern) matching the original DBOS app.
    Keys: id, place, magnitude, timestamp
    """

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        # Ensure all required keys exist
        for k in ("id", "place", "magnitude", "timestamp"):
            if k not in self:
                self[k] = "" if k in ("id", "place") else 0


# ---------------------------------------------------------------------------
# State helpers
#
# We interact with Cleat's key-value state service directly via
# durable_call rather than relying on HostCalls.get_state() because
# get_state()'s type coercion logic may not round-trip complex JSON
# types correctly (see ISSUES.md).
# ---------------------------------------------------------------------------


def _state_get(h: HostCalls, key: str) -> Any:
    """Retrieve a value from durable state by key.

    Returns ``None`` if the key does not exist.
    """
    try:
        result = h.durable_call("state", "get", {"key": key})
        return json.loads(result)
    except RuntimeError:
        return None


def _state_set(h: HostCalls, key: str, value: Any) -> None:
    """Store a value in durable state under the given key.

    The value is JSON-serialised automatically.
    """
    h.durable_call("state", "set", {"key": key, "value": value})


# ---------------------------------------------------------------------------
# Workflow helpers
# ---------------------------------------------------------------------------


def _build_usgs_url(start_time: datetime, end_time: datetime) -> str:
    """Build the USGS API URL with query parameters."""
    params = {
        "format": "geojson",
        "starttime": start_time.strftime("%Y-%m-%dT%H:%M:%S"),
        "endtime": end_time.strftime("%Y-%m-%dT%H:%M:%S"),
        "minmagnitude": str(MIN_MAGNITUDE),
    }
    qs = urllib.parse.urlencode(params)
    return f"{USGS_API_URL}?{qs}"


def _parse_earthquake_data(raw_json: str) -> list[EarthquakeData]:
    """Parse USGS GeoJSON response into EarthquakeData list."""
    data = json.loads(raw_json)
    earthquakes: list[EarthquakeData] = []
    for item in data.get("features", []):
        props = item.get("properties", {})
        earthquake = EarthquakeData(
            id=item["id"],
            place=props.get("place", "Unknown"),
            magnitude=props.get("mag", 0.0),
            timestamp=props.get("time", 0),
        )
        earthquakes.append(earthquake)
    return earthquakes


def _get_seen_ids(h: HostCalls) -> list[str]:
    """Get the list of previously recorded earthquake IDs."""
    stored = _state_get(h, STATE_SEEN_IDS_KEY)
    if stored is None:
        return []
    if isinstance(stored, list):
        return stored
    # Defensive: handle stored string-wrapped JSON
    if isinstance(stored, str):
        try:
            parsed = json.loads(stored)
            return parsed if isinstance(parsed, list) else [stored]
        except (json.JSONDecodeError, TypeError):
            return [stored]
    # Single string value
    if isinstance(stored, str):
        return [stored]
    return []


def _save_seen_ids(h: HostCalls, ids: list[str]) -> None:
    """Persist the list of seen earthquake IDs."""
    _state_set(h, STATE_SEEN_IDS_KEY, ids)


# ---------------------------------------------------------------------------
# Workflow steps
# ---------------------------------------------------------------------------


def get_earthquake_data(
    h: HostCalls, start_time: datetime, end_time: datetime
) -> list[EarthquakeData]:
    """Fetch earthquake data from the USGS API.

    This is a durable, replay-safe HTTP fetch via Cleat's ``http`` service.
    Equivalent to ``@DBOS.step()`` + ``requests.get()`` in the original app.

    Parameters
    ----------
    h : HostCalls
        HostCalls instance for the current execution context.
    start_time : datetime
        Start of the query window (UTC).
    end_time : datetime
        End of the query window (UTC).

    Returns
    -------
    list[EarthquakeData]
        List of earthquake records found in the time window.

    Raises
    ------
    RuntimeError
        If the USGS API returns a non-200 status.
    """
    url = _build_usgs_url(start_time, end_time)
    body, status = h.durable_fetch(url, "GET")

    if status != 200:
        raise RuntimeError(
            f"Error fetching data from USGS: status={status} body={body[:500]}"
        )

    return _parse_earthquake_data(body)


def record_earthquake_data(h: HostCalls, data: EarthquakeData) -> bool:
    """Record an earthquake in durable state.

    Returns ``True`` if this is a *new* earthquake (just inserted), or
    ``False`` if it already existed (updated).

    Equivalent to ``@DBOS.transaction()`` + SQLAlchemy upsert in the
    original app, but stores data in Cleat's key-value state rather
    than Postgres.

    Parameters
    ----------
    h : HostCalls
        HostCalls instance.
    data : EarthquakeData
        The earthquake record to store.

    Returns
    -------
    bool
        ``True`` if the earthquake is newly recorded, ``False`` if it
        was already known.
    """
    eq_key = f"{STATE_EQ_PREFIX}{data['id']}"
    existing = _state_get(h, eq_key)
    _state_set(h, eq_key, dict(data))
    return existing is None


# ---------------------------------------------------------------------------
# Main workflow entry point
# ---------------------------------------------------------------------------


def track_earthquakes_impl(h: HostCalls, scheduled_time: str) -> dict:
    """Check for new earthquakes and record them (implementation).

    This is the raw implementation function, separated from the
    ``@durable_entry`` wrapper so it can be tested directly with
    ``CleatTestHarness`` without going through the WASM ABI wrapper.

    Designed to be invoked on a schedule by the Cleat host runtime.
    The ``scheduled_time`` is an ISO-8601 UTC timestamp passed by the
    scheduler (e.g. ``"2025-01-15T10:00:00+00:00"``).

    For each invocation:
      1. Calculates a 1-hour lookback window ending at ``scheduled_time``.
      2. Fetches earthquake data from USGS for that window.
      3. Filters out previously-seen earthquakes using durable state.
      4. Records new earthquakes in durable state.
      5. Sends a notification for each new earthquake via the ``notification``
         host service.

    Returns
    -------
    dict
        Summary containing ``new_count``, ``total_seen``, ``errors``,
        and ``scheduled_time``.

    Raises
    ------
    Exception
        Propagated to the host runtime for retry/reporting.
    """
    # Parse scheduled time
    end_time = datetime.fromisoformat(scheduled_time)
    if end_time.tzinfo is None:
        end_time = end_time.replace(tzinfo=timezone.utc)

    start_time = end_time - timedelta(hours=LOOKBACK_HOURS)

    h.durable_log(
        f"track_earthquakes: checking {start_time.isoformat()} to "
        f"{end_time.isoformat()}"
    )

    # Step 1: Fetch earthquake data from USGS
    earthquakes = get_earthquake_data(h, start_time, end_time)

    if not earthquakes:
        h.durable_log("No earthquakes found in time window")
        return {
            "new_count": 0,
            "total_seen": 0,
            "errors": [],
            "scheduled_time": scheduled_time,
        }

    # Step 2: Get previously seen IDs for idempotency
    seen_ids = _get_seen_ids(h)
    h.durable_log(f"Previously seen {len(seen_ids)} earthquake IDs")

    # Step 3: Process each earthquake
    new_count = 0
    errors: list[str] = []

    for eq in earthquakes:
        eq_id: str = eq["id"]

        if eq_id in seen_ids:
            # Already known - update record, no notification
            h.durable_log(f"Updating existing earthquake: {eq_id}")
            record_earthquake_data(h, eq)
            continue

        # New earthquake
        h.durable_log(
            f"New earthquake: {eq_id} place={eq['place']} "
            f"mag={eq['magnitude']}"
        )

        try:
            # Record in state
            record_earthquake_data(h, eq)

            # Send notification via host service
            notification_payload = {
                "type": "new_earthquake",
                "id": eq_id,
                "place": eq["place"],
                "magnitude": eq["magnitude"],
                "timestamp": eq["timestamp"],
            }
            h.durable_call("notification", "send", notification_payload)

            # Track as seen
            seen_ids.append(eq_id)
            new_count += 1

        except RuntimeError as exc:
            error_msg = f"Failed to process earthquake {eq_id}: {exc}"
            h.durable_log(error_msg)
            errors.append(error_msg)

    # Step 4: Persist updated seen IDs
    _save_seen_ids(h, seen_ids)

    summary = {
        "new_count": new_count,
        "total_seen": len(seen_ids),
        "errors": errors,
        "scheduled_time": scheduled_time,
    }

    h.durable_log(f"track_earthquakes complete: {json.dumps(summary)}")
    return summary


@durable_entry(name="track_earthquakes")
def track_earthquakes(h: HostCalls, scheduled_time: str) -> dict:
    """@durable_entry wrapper around :func:`track_earthquakes_impl`.

    This function is the WASM-exportable entry point for the Cleat host.
    It delegates to ``track_earthquakes_impl`` for the actual logic.
    Tests should call ``track_earthquakes_impl`` directly with a
    ``CleatTestHarness`` instance.
    """
    return track_earthquakes_impl(h, scheduled_time)
