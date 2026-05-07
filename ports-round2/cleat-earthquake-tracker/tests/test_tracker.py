"""
Tests for the Cleat Earthquake Tracker workflow.

Uses the CleatTestHarness to test without a WASM runtime.

Port of the original DBOS test suite from:
    dbos-demo-apps/python/earthquake-tracker/tests/test_earthquake_tracker.py

Key differences from DBOS tests:
    - No database fixtures (Cleat uses key-value state, not Postgres)
    - Uses CleatTestHarness stub_call to mock services
    - No need for alembic migrations or database resets
    - State is verified through the test harness's call history
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from unittest.mock import patch

from cleat_sdk.test_harness import CleatTestHarness

from earthquake_tracker.main import (
    EarthquakeData,
    _get_seen_ids,
    _save_seen_ids,
    _state_get,
    _state_set,
    get_earthquake_data,
    record_earthquake_data,
    track_earthquakes_impl,
)

# ---------------------------------------------------------------------------
# Sample data
# ---------------------------------------------------------------------------

SAMPLE_USGS_RESPONSE = json.dumps({
    "type": "FeatureCollection",
    "metadata": {
        "generated": 1700000000000,
        "url": "https://earthquake.usgs.gov/fdsnws/event/1/query",
        "title": "USGS Earthquakes",
        "status": 200,
        "api": "1.5.8",
        "count": 2,
    },
    "features": [
        {
            "type": "Feature",
            "id": "ci40171730",
            "properties": {
                "mag": 2.21,
                "place": "15 km SW of Searles Valley, CA",
                "time": 1738136375670,
            },
        },
        {
            "type": "Feature",
            "id": "nc73543321",
            "properties": {
                "mag": 1.85,
                "place": "9 km NW of The Geysers, CA",
                "time": 1738136000000,
            },
        },
    ],
})

SAMPLE_EARTHQUAKE: EarthquakeData = EarthquakeData(
    id="ci40171730",
    place="15 km SW of Searles Valley, CA",
    magnitude=2.21,
    timestamp=1738136375670,
)


# ---------------------------------------------------------------------------
# Tests for get_earthquake_data
# ---------------------------------------------------------------------------


def test_get_earthquake_data_fetch():
    """Verify that get_earthquake_data parses USGS GeoJSON correctly."""
    h = CleatTestHarness()

    # Stub the http.fetch call that durable_fetch delegates to
    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": SAMPLE_USGS_RESPONSE, "status": 200}),
    )

    end_time = datetime.now(timezone.utc)
    start_time = end_time - timedelta(hours=24)
    earthquakes = get_earthquake_data(h, start_time, end_time)

    assert len(earthquakes) == 2
    assert earthquakes[0]["id"] == "ci40171730"
    assert earthquakes[0]["place"] == "15 km SW of Searles Valley, CA"
    assert earthquakes[0]["magnitude"] == 2.21
    assert earthquakes[0]["timestamp"] == 1738136375670
    assert earthquakes[1]["id"] == "nc73543321"


def test_get_earthquake_data_http_error():
    """Verify that an HTTP error raises RuntimeError."""
    h = CleatTestHarness()

    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": "Bad Gateway", "status": 502}),
    )

    end_time = datetime.now(timezone.utc)
    start_time = end_time - timedelta(hours=1)

    try:
        get_earthquake_data(h, start_time, end_time)
        assert False, "Expected RuntimeError for 502 status"
    except RuntimeError as e:
        assert "502" in str(e)


# ---------------------------------------------------------------------------
# Tests for record_earthquake_data
# ---------------------------------------------------------------------------


def test_record_new_earthquake():
    """Verify that recording a new earthquake returns True."""
    h = CleatTestHarness()

    # Stub the state service calls: first get (None = not found), then set
    h.stub_call("state", "get", response=json.dumps(None))
    h.stub_call("state", "set", response=json.dumps(None))

    result = record_earthquake_data(h, SAMPLE_EARTHQUAKE)
    assert result is True, "New earthquake should return True"


def test_record_existing_earthquake():
    """Verify that recording an existing earthquake returns False."""
    h = CleatTestHarness()

    # Stub the state service calls: first get (found existing), then set
    h.stub_call("state", "get", response=json.dumps(SAMPLE_EARTHQUAKE))
    h.stub_call("state", "set", response=json.dumps(None))

    result = record_earthquake_data(h, SAMPLE_EARTHQUAKE)
    assert result is False, "Existing earthquake should return False"


# ---------------------------------------------------------------------------
# Tests for state helpers
# ---------------------------------------------------------------------------


def test_state_set_get():
    """Verify that _state_get and _state_set work via CleatTestHarness."""
    h = CleatTestHarness()

    # Stub the state calls
    h.stub_call("state", "set", response=json.dumps(None))
    h.stub_call("state", "get", response=json.dumps(["a", "b", "c"]))

    _state_set(h, "test-key", ["a", "b", "c"])

    value = _state_get(h, "test-key")
    assert value == ["a", "b", "c"]


def test_state_get_nonexistent():
    """Verify that _state_get returns None for missing keys."""
    h = CleatTestHarness()

    # Stub the state get to raise RuntimeError (key not found)
    h.stub_call("state", "get", error="key not found")

    value = _state_get(h, "nonexistent-key")
    assert value is None


# ---------------------------------------------------------------------------
# Tests for seen_ids helpers
# ---------------------------------------------------------------------------


def test_get_seen_ids_empty():
    """Verify that _get_seen_ids returns [] when no state exists."""
    h = CleatTestHarness()

    # Stub the state get to fail (no data yet)
    h.stub_call("state", "get", error="key not found")

    seen = _get_seen_ids(h)
    assert seen == []


def test_get_seen_ids_with_data():
    """Verify that _get_seen_ids returns stored IDs."""
    h = CleatTestHarness()

    h.stub_call("state", "get", response=json.dumps(["id1", "id2", "id3"]))

    seen = _get_seen_ids(h)
    assert seen == ["id1", "id2", "id3"]


# ---------------------------------------------------------------------------
# Tests for the main workflow
# ---------------------------------------------------------------------------


def test_track_earthquakes_new_only():
    """Run the full workflow with mock data: all earthquakes are new."""
    h = CleatTestHarness()

    # --- Stub the http.fetch for USGS API call ---
    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": SAMPLE_USGS_RESPONSE, "status": 200}),
    )

    # --- Stub state service: get seen_ids (empty, first run) ---
    h.stub_call("state", "get", error="key not found")

    # For each earthquake: state.get (check if exists), state.set (save)
    # Two earthquakes: get(not-found) -> set, get(not-found) -> set
    h.stub_call("state", "get", error="not found")  # eq1 check
    h.stub_call("state", "set", response=json.dumps(None))  # eq1 save
    h.stub_call("state", "get", error="not found")  # eq2 check
    h.stub_call("state", "set", response=json.dumps(None))  # eq2 save

    # --- Stub notification service for each new earthquake ---
    h.stub_call("notification", "send", response=json.dumps(None))
    h.stub_call("notification", "send", response=json.dumps(None))

    # --- Stub final state.set for saving seen_ids ---
    h.stub_call("state", "set", response=json.dumps(None))

    # Run the workflow
    scheduled_time = "2025-01-15T10:00:00+00:00"
    result = track_earthquakes_impl(h, scheduled_time)

    # Verify the result
    assert result["new_count"] == 2
    assert result["total_seen"] == 2
    assert result["errors"] == []
    assert result["scheduled_time"] == scheduled_time


def test_track_earthquakes_with_duplicates():
    """Run the full workflow: some earthquakes already seen."""
    h = CleatTestHarness()

    # The USGS response has 2 earthquakes
    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": SAMPLE_USGS_RESPONSE, "status": 200}),
    )

    # seen_ids already contains "ci40171730" (first one is a duplicate)
    h.stub_call("state", "get", response=json.dumps(["ci40171730"]))

    # eq1 (ci40171730): already in seen_ids, so only update (get+set)
    h.stub_call("state", "get", response=json.dumps(SAMPLE_EARTHQUAKE))  # found
    h.stub_call("state", "set", response=json.dumps(None))  # update

    # eq2 (nc73543321): new, so get(not-found) -> set -> notify
    h.stub_call("state", "get", error="not found")
    h.stub_call("state", "set", response=json.dumps(None))
    h.stub_call("notification", "send", response=json.dumps(None))

    # Final seen_ids save
    h.stub_call("state", "set", response=json.dumps(None))

    scheduled_time = "2025-01-15T10:00:00+00:00"
    result = track_earthquakes_impl(h, scheduled_time)

    assert result["new_count"] == 1, "Only one earthquake should be new"
    assert result["total_seen"] == 2, "Should now have 2 total seen"
    assert result["errors"] == []


def test_track_earthquakes_no_data():
    """Run the workflow when USGS returns no earthquakes."""
    h = CleatTestHarness()

    empty_response = json.dumps({
        "type": "FeatureCollection",
        "metadata": {"count": 0},
        "features": [],
    })

    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": empty_response, "status": 200}),
    )

    scheduled_time = "2025-01-15T10:00:00+00:00"
    result = track_earthquakes_impl(h, scheduled_time)

    assert result["new_count"] == 0
    assert result["total_seen"] == 0
    assert result["errors"] == []


def test_track_earthquakes_http_failure():
    """Run the workflow when USGS returns an error."""
    h = CleatTestHarness()

    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": "Service Unavailable", "status": 503}),
    )

    scheduled_time = "2025-01-15T10:00:00+00:00"

    try:
        track_earthquakes_impl(h, scheduled_time)
        assert False, "Expected RuntimeError for 503 status"
    except RuntimeError as e:
        assert "503" in str(e)


def test_track_earthquakes_notification_failure():
    """Verify that a notification failure is caught and reported as error."""
    h = CleatTestHarness()

    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": SAMPLE_USGS_RESPONSE, "status": 200}),
    )

    # First run: no seen IDs
    h.stub_call("state", "get", error="key not found")

    # eq1: state operations succeed
    h.stub_call("state", "get", error="not found")
    h.stub_call("state", "set", response=json.dumps(None))
    # Notification fails for eq1
    h.stub_call("notification", "send", error="notification service unavailable")

    # eq2: state operations succeed, notification succeeds
    h.stub_call("state", "get", error="not found")
    h.stub_call("state", "set", response=json.dumps(None))
    h.stub_call("notification", "send", response=json.dumps(None))

    # Final seen_ids save
    h.stub_call("state", "set", response=json.dumps(None))

    scheduled_time = "2025-01-15T10:00:00+00:00"
    result = track_earthquakes_impl(h, scheduled_time)

    # One error from the failed notification
    assert len(result["errors"]) == 1
    assert "ci40171730" in result["errors"][0]
    # Only the second eq was processed successfully
    assert result["new_count"] == 1
    assert result["total_seen"] == 1


# ---------------------------------------------------------------------------
# Integration-style tests using direct harness state inspection
# ---------------------------------------------------------------------------


def test_call_history_records_all_calls():
    """Verify that all durable calls are recorded in the harness history."""
    h = CleatTestHarness()

    h.stub_call(
        "http", "fetch",
        response=json.dumps({"body": SAMPLE_USGS_RESPONSE, "status": 200}),
    )
    h.stub_call("state", "get", error="key not found")
    h.stub_call("state", "get", error="not found")
    h.stub_call("state", "set", response=json.dumps(None))
    h.stub_call("state", "get", error="not found")
    h.stub_call("state", "set", response=json.dumps(None))
    h.stub_call("notification", "send", response=json.dumps(None))
    h.stub_call("notification", "send", response=json.dumps(None))
    h.stub_call("state", "set", response=json.dumps(None))

    result = track_earthquakes_impl(h, "2025-01-15T10:00:00+00:00")

    # Check call counts
    http_calls = h.call_count("http", "fetch")
    assert http_calls == 1, "Should make exactly 1 HTTP fetch"

    notification_calls = h.call_count("notification", "send")
    assert notification_calls == 2, "Should send 2 notifications"

    state_get_calls = h.call_count("state", "get")
    assert state_get_calls == 3, "3 state gets: seen_ids + eq1 + eq2"

    state_set_calls = h.call_count("state", "set")
    assert state_set_calls == 3, "3 state sets: eq1 + eq2 + seen_ids"
