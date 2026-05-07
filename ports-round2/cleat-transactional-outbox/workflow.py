"""
Workflow definition for the transactional-outbox pattern.

This module contains the Cleat @durable_entry decorated function that
coordinates the outbox workflow.  It runs inside a WASM sandbox and
uses HostCalls.durable_call() for all external interactions.

In the classic outbox pattern, an outbox table stores events atomically
with business data, and a background poller publishes them to a broker.
With Cleat (as with DBOS), the workflow engine itself provides the
durability guarantee, so no outbox table is needed.

    place_order
        ├── durable_call("db", "insert_order", ...)   → order_id
        ├── set_query_state("order_id", order_id)      → visible to API
        ├── durable_call("notifier", "send_notification", ...)  → status
        └── durable_call("db", "update_notification_status", ...) → ok
"""

from __future__ import annotations

import json

from cleat_sdk import HostCalls, durable_entry

ORDER_ID_STATE_KEY = "order_id"


@durable_entry(name="place_order")
def place_order_workflow(h: HostCalls, customer: str, item: str, quantity: int) -> str:
    """Place an order atomically with notification sending.

    This workflow replaces the classic outbox table + poller pattern.
    The Cleat durable execution engine guarantees that every order is
    inserted AND its notification is sent, atomically, despite crashes
    or process restarts.

    Parameters
    ----------
    customer:
        Customer name (e.g. ``"Alice"``).
    item:
        Item name (e.g. ``"Widget"``).
    quantity:
        Quantity ordered.

    Returns
    -------
    str
        JSON string with ``order_id`` and ``notification_status``.

    Host services required (see services_contract.md):
        - ``db.insert_order(customer, item, quantity) -> {"order_id": N}``
        - ``notifier.send_notification(order_id, customer, item) -> {"status": "sent"}``
        - ``db.update_notification_status(order_id, status) -> {"status": "ok"}``
    """
    # Step 1: Insert the order into the database via the host DB service.
    order_result = h.durable_call(
        "db",
        "insert_order",
        {"customer": customer, "item": item, "quantity": quantity},
    )
    order_data = json.loads(order_result)
    order_id: int = order_data["order_id"]

    # Expose the order_id immediately via queryable state so the HTTP API
    # can return early while the workflow continues with the notification.
    h.set_query_state(ORDER_ID_STATE_KEY, str(order_id))

    # Step 2: Send a notification (e.g. email, Kafka message, webhook).
    h.durable_call(
        "notifier",
        "send_notification",
        {"order_id": order_id, "customer": customer, "item": item},
    )

    # Step 3: Mark the notification as sent in the database.
    h.durable_call(
        "db",
        "update_notification_status",
        {"order_id": order_id, "status": "SENT"},
    )

    return json.dumps({"order_id": order_id, "notification_status": "SENT"})
