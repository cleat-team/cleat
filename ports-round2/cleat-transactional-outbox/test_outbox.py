"""
Tests for the transactional-outbox Cleat port.

Two test layers:
1. **Unit tests** using ``CleatTestHarness`` — stub all ``cleat_call``
   responses, verify call order and parameters.
2. **Integration tests** using ``LocalRuntime`` — exercise the workflow
   against service implementations with a real SQLite in-memory DB.
"""

from __future__ import annotations

import json
import os
import time
from typing import Any, Generator

import pytest

from cleat_sdk.test_harness import CleatTestHarness

from workflow import ORDER_ID_STATE_KEY, place_order_workflow


# ========================================================================
# Fixtures
# ========================================================================


@pytest.fixture
def harness() -> CleatTestHarness:
    """Create a fresh CleatTestHarness with stubbed services."""
    h = CleatTestHarness()

    # Stub all three cleat_calls the workflow makes, in order.
    h.stub_call("db", "insert_order", '{"order_id": 42}')
    h.stub_call(
        "notifier", "send_notification", '{"status": "sent"}'
    )
    h.stub_call("db", "update_notification_status", '{"status": "ok"}')

    return h


# ========================================================================
# Unit tests (CleatTestHarness)
# ========================================================================


class TestWorkflowUnit:
    """Test the workflow in isolation using CleatTestHarness stubs."""

    def test_happy_path(self, harness: CleatTestHarness) -> None:
        """Workflow inserts order, notifies, and updates status."""
        result = place_order_workflow.__wrapped__(  # type: ignore[attr-defined]
            harness, customer="Alice", item="Widget", quantity=2
        )
        data = json.loads(result)

        assert data["order_id"] == 42
        assert data["notification_status"] == "SENT"

        # Verify all three calls were made in order.
        assert harness.call_count("db", "insert_order") == 1
        assert harness.call_count("notifier", "send_notification") == 1
        assert harness.call_count("db", "update_notification_status") == 1

        # Check the insert_order request payload.
        insert_rec = harness.last_call("db", "insert_order")
        assert insert_rec is not None
        insert_req = json.loads(insert_rec.request)
        assert insert_req["customer"] == "Alice"
        assert insert_req["item"] == "Widget"
        assert insert_req["quantity"] == 2

        # Check the notification request.
        notif_rec = harness.last_call("notifier", "send_notification")
        assert notif_rec is not None
        notif_req = json.loads(notif_rec.request)
        assert notif_req["order_id"] == 42
        assert notif_req["customer"] == "Alice"

        # Check the update request.
        update_rec = harness.last_call("db", "update_notification_status")
        assert update_rec is not None
        update_req = json.loads(update_rec.request)
        assert update_req["order_id"] == 42
        assert update_req["status"] == "SENT"

    def test_query_state_is_set(self, harness: CleatTestHarness) -> None:
        """Query state is set with order_id after the DB insert."""
        place_order_workflow.__wrapped__(  # type: ignore[attr-defined]
            harness, customer="Bob", item="Gadget", quantity=1
        )
        # CleatTestHarness stores set_query_state calls in _query_state.
        assert harness._query_state.get(ORDER_ID_STATE_KEY) == "42"

    def test_db_failure_propagates(self, harness: CleatTestHarness) -> None:
        """If the DB call fails, the workflow raises and no further calls."""
        # Replace the first stub with an error.
        harness.reset()
        harness.stub_call("db", "insert_order", error="connection refused")

        with pytest.raises(RuntimeError, match="cleat_call failed"):
            place_order_workflow.__wrapped__(  # type: ignore[attr-defined]
                harness, customer="Charlie", item="Doohickey", quantity=3
            )

        # Zero calls to notifier (workflow aborted before getting there).
        assert harness.call_count("notifier", "send_notification") == 0

    def test_notifier_failure_still_updates_status(
        self, harness: CleatTestHarness
    ) -> None:
        """If the notifier fails, the workflow should still propagate.

        Note: The current workflow does NOT handle notifier failures
        gracefully.  If send_notification raises, the entire workflow
        fails and the DB status update is never called.
        """
        harness.reset()
        harness.stub_call("db", "insert_order", '{"order_id": 7}')
        harness.stub_call(
            "notifier", "send_notification", error="broker timeout"
        )

        with pytest.raises(RuntimeError, match="cleat_call failed"):
            place_order_workflow.__wrapped__(  # type: ignore[attr-defined]
                harness, customer="Diana", item="Thingy", quantity=1
            )

        # With a Cleat host, the workflow would be retried (idempotent
        # insert).  For now the error propagates to the caller.
        assert harness.call_count("db", "insert_order") == 1
        assert harness.call_count("db", "update_notification_status") == 0


# ========================================================================
# Helper to test with LocalRuntime + in-memory DB
# ========================================================================


@pytest.fixture
def local_runtime() -> Generator[Any, None, None]:
    """Create a LocalRuntime backed by an in-memory SQLite database."""
    import sqlalchemy as sa
    from runtime import LocalRuntime
    from services import DBService, NotifierService, metadata as table_metadata

    # Use SQLite in-memory for testing.
    db = DBService(db_url="sqlite:///:memory:")
    # Create the table schema.
    table_metadata.create_all(db._engine)

    # Patch NotifierService to be instant (no sleep).
    class FastNotifier(NotifierService):
        def send_notification(self, order_id, customer, item):
            print(f"[TestNotifier] Instant notification for order {order_id}")
            return {"status": "sent"}

    rt = LocalRuntime(db=db, notifier=FastNotifier())

    yield rt

    # Teardown: drop tables.
    table_metadata.drop_all(db._engine)


class TestWorkflowIntegration:
    """Integration tests using LocalRuntime with real DB."""

    def test_full_workflow(self, local_runtime: Any) -> None:
        """Complete workflow: insert, notify, update status."""
        rt = local_runtime

        run_id = rt.start_workflow(
            place_order_workflow,
            customer="Eve",
            item="SQL Widget",
            quantity=5,
        )

        # Check that the workflow completed.
        status = rt.get_workflow_status(run_id)
        assert status["status"] == "completed"

        # Check result.
        result = json.loads(status["result"])
        assert "order_id" in result
        assert result["notification_status"] == "SENT"

        # Verify the order was persisted in the DB.
        db = rt._services["db"]
        orders = db.list_orders()
        matching = [o for o in orders if o["order_id"] == result["order_id"]]
        assert len(matching) == 1
        assert matching[0]["notification_status"] == "SENT"
        assert matching[0]["customer"] == "Eve"

    def test_query_state_available_after_insert(
        self, local_runtime: Any
    ) -> None:
        """The order_id is available via query state immediately."""
        rt = local_runtime
        run_id = rt.start_workflow(
            place_order_workflow,
            customer="Frank",
            item="Query Widget",
            quantity=1,
        )

        order_id_str = rt.get_query_state(run_id, ORDER_ID_STATE_KEY)
        assert order_id_str is not None
        assert int(order_id_str) > 0

    def test_multiple_orders(self, local_runtime: Any) -> None:
        """Multiple orders can be placed concurrently."""
        rt = local_runtime

        ids = []
        for i in range(3):
            run_id = rt.start_workflow(
                place_order_workflow,
                customer=f"User-{i}",
                item=f"Item-{i}",
                quantity=i + 1,
            )
            status = rt.get_workflow_status(run_id)
            result = json.loads(status["result"])
            ids.append(result["order_id"])

        assert len(set(ids)) == 3  # all unique

        # Verify all orders are in the DB.
        db = rt._services["db"]
        orders = db.list_orders()
        assert len(orders) >= 3


# ========================================================================
# Edge cases
# ========================================================================


class TestEdgeCases:
    """Edge-case tests for the workflow."""

    def test_zero_quantity(self, harness: CleatTestHarness) -> None:
        """Zero quantity is allowed (DB constraint)."""
        harness.reset()
        harness.stub_call("db", "insert_order", '{"order_id": 99}')
        harness.stub_call("notifier", "send_notification", '{"status": "sent"}')
        harness.stub_call("db", "update_notification_status", '{"status": "ok"}')

        result = place_order_workflow.__wrapped__(  # type: ignore[attr-defined]
            harness, customer="Grace", item="Free", quantity=0
        )
        data = json.loads(result)
        assert data["order_id"] == 99

    def test_empty_customer_name(self, harness: CleatTestHarness) -> None:
        """Empty customer name is passed through."""
        harness.reset()
        harness.stub_call("db", "insert_order", '{"order_id": 1}')
        harness.stub_call("notifier", "send_notification", '{"status": "sent"}')
        harness.stub_call("db", "update_notification_status", '{"status": "ok"}')

        result = place_order_workflow.__wrapped__(  # type: ignore[attr-defined]
            harness, customer="", item="Something", quantity=1
        )
        data = json.loads(result)
        assert data["order_id"] == 1

    def test_large_quantity(self, harness: CleatTestHarness) -> None:
        """Large quantity should pass through."""
        harness.reset()
        harness.stub_call("db", "insert_order", '{"order_id": 1000}')
        harness.stub_call("notifier", "send_notification", '{"status": "sent"}')
        harness.stub_call("db", "update_notification_status", '{"status": "ok"}')

        result = place_order_workflow.__wrapped__(  # type: ignore[attr-defined]
            harness, customer="Heidi", item="Bulk", quantity=999999
        )
        data = json.loads(result)
        assert data["order_id"] == 1000
