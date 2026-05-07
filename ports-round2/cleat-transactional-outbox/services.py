"""
Host-side service implementations for the transactional-outbox pattern.

These services run on the Cleat host (outside the WASM sandbox) and are
called by the workflow via ``HostCalls.cleat_call()``.

In a production deployment these services would be registered with the
Cleat host runtime.  For local development they can be instantiated
directly and used with the :class:`LocalRuntime`.

Each method accepts keyword arguments matching the dict that the
workflow sends via ``cleat_call(service, operation, request_dict)``.
"""

from __future__ import annotations

import os
import time
from typing import Any

import sqlalchemy as sa

# ---------------------------------------------------------------------------
# Database table schema
# ---------------------------------------------------------------------------

metadata = sa.MetaData()

orders = sa.Table(
    "orders",
    metadata,
    sa.Column("order_id", sa.Integer, primary_key=True, autoincrement=True),
    sa.Column("customer", sa.Text, nullable=False),
    sa.Column("item", sa.Text, nullable=False),
    sa.Column("quantity", sa.Integer, nullable=False),
    sa.Column(
        "notification_status", sa.Text, nullable=False, server_default="PENDING"
    ),
    sa.Column("created_at", sa.DateTime, server_default=sa.func.now()),
)


# ---------------------------------------------------------------------------
# DB Service
# ---------------------------------------------------------------------------


class DBService:
    """Host-side service for order database operations.

    Called by the workflow via ``h.cleat_call("db", "<operation>", {...})``.

    Methods are designed to accept the exact dict keys that the workflow
    sends, so they can be dispatched via simple ``getattr``.
    """

    def __init__(self, db_url: str | None = None) -> None:
        self._engine = sa.create_engine(
            db_url
            or os.environ.get(
                "DATABASE_URL",
                "postgresql+psycopg://postgres@localhost:5432/transactional_outbox",
            )
        )

    # -- schema ----------------------------------------------------------

    def create_orders_table(self, **kwargs: Any) -> dict:
        """Ensure the orders table exists.

        Called as ``cleat_call("db", "create_orders_table", {})``.
        Note: schema migrations are typically not part of the workflow
        (they run at startup), but included here for completeness.
        """
        metadata.create_all(self._engine)
        return {"status": "ok"}

    # -- read / write ----------------------------------------------------

    def insert_order(self, customer: str, item: str, quantity: int) -> dict:
        """Insert an order row and return the new order_id.

        Expected workflow call::

            h.cleat_call("db", "insert_order", {
                "customer": customer, "item": item, "quantity": quantity,
            })
        """
        with self._engine.begin() as conn:
            result = conn.execute(
                orders.insert().values(
                    customer=customer, item=item, quantity=quantity
                )
            )
            order_id: int = result.inserted_primary_key[0]
        print(f"[DB] Inserted order {order_id}: {quantity}x {item} for {customer}")
        return {"order_id": order_id}

    def update_notification_status(
        self, order_id: int, status: str
    ) -> dict:
        """Mark an order's notification as sent (or any other status).

        Expected workflow call::

            h.cleat_call("db", "update_notification_status", {
                "order_id": order_id, "status": "SENT",
            })
        """
        with self._engine.begin() as conn:
            conn.execute(
                orders.update()
                .where(orders.c.order_id == order_id)
                .values(notification_status=status)
            )
        print(f"[DB] Order {order_id} notification status -> {status}")
        return {"status": "ok"}

    def list_orders(self, **kwargs: Any) -> list[dict]:
        """Return all orders, newest first.

        Called directly by the FastAPI endpoint (not via workflow).
        """
        with self._engine.connect() as conn:
            rows = (
                conn.execute(
                    orders.select().order_by(orders.c.order_id.desc())
                )
                .mappings()
                .all()
            )
            return [dict(r) for r in rows]


# ---------------------------------------------------------------------------
# Notifier Service (message broker simulation)
# ---------------------------------------------------------------------------


class NotifierService:
    """Host-side service for sending order notifications.

    Simulates publishing a message to a message broker (Kafka, RabbitMQ,
    SQS, etc.).  The sleep models network / broker latency.

    Expected workflow call::

        h.cleat_call("notifier", "send_notification", {
            "order_id": order_id, "customer": customer, "item": item,
        })
    """

    def send_notification(
        self, order_id: int, customer: str, item: str
    ) -> dict:
        """Simulate publishing a notification message."""
        print(
            f"[Notifier] Publishing notification for order {order_id}: "
            f"{item} for {customer}"
        )
        time.sleep(3)  # simulate broker latency
        print(
            f"[Notifier] Notification published for order {order_id}: "
            f"{item} for {customer}"
        )
        return {"status": "sent"}
