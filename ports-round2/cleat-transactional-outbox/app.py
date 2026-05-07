"""
FastAPI application for the transactional-outbox demo.

Exposes REST endpoints that start the Cleat workflow and query orders.

The FastAPI app runs on the **host** side (outside the WASM sandbox).
It uses the :class:`LocalRuntime` to execute workflows in-process for
development.  In production, the workflow runs in the Cleat WASM host
and the app communicates via :class:`cleat_sdk.CleatClient`.
"""

from __future__ import annotations

import os
from pathlib import Path

import uvicorn
from fastapi import FastAPI
from fastapi.responses import HTMLResponse
from pydantic import BaseModel

from runtime import LocalRuntime
from services import DBService, NotifierService
from workflow import ORDER_ID_STATE_KEY, place_order_workflow

# ---------------------------------------------------------------------------
# FastAPI app
# ---------------------------------------------------------------------------

app = FastAPI(title="Transactional Outbox (Cleat port)")

# ---------------------------------------------------------------------------
# Models
# ---------------------------------------------------------------------------


class OrderRequest(BaseModel):
    customer: str
    item: str
    quantity: int


# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------

_runtime: LocalRuntime | None = None


def get_runtime() -> LocalRuntime:
    assert _runtime is not None, "Runtime not initialised — call init_runtime()"
    return _runtime


def init_runtime() -> LocalRuntime:
    global _runtime
    db = DBService()
    notifier = NotifierService()
    _runtime = LocalRuntime(db=db, notifier=notifier)
    return _runtime


# ---------------------------------------------------------------------------
# Routes
# ---------------------------------------------------------------------------


@app.post("/orders")
def create_order(request: OrderRequest) -> dict:
    """Place an order via the Cleat workflow and return immediately.

    The workflow inserts the order, sets query state with the order_id,
    sends a notification, and updates the status — all durably.
    This endpoint returns as soon as the order_id is available (after
    the DB insert), without waiting for the notification to complete.
    """
    rt = get_runtime()

    run_id = rt.start_workflow(
        place_order_workflow,
        customer=request.customer,
        item=request.item,
        quantity=request.quantity,
    )

    # Poll briefly for the order_id that the workflow exposes via
    # set_query_state after the DB insert.
    order_id_str = rt.wait_for_query_state(run_id, ORDER_ID_STATE_KEY, timeout_sec=10.0)

    if order_id_str is None:
        return {"error": "workflow did not produce an order_id within timeout"}

    return {"order_id": int(order_id_str)}


@app.get("/orders")
def list_orders() -> list[dict]:
    """List all orders, newest first."""
    from services import DBService
    db = DBService()
    return db.list_orders()


@app.get("/")
def index() -> HTMLResponse:
    """Serve the HTML frontend."""
    html = (Path(__file__).parent / "static" / "index.html").read_text()
    return HTMLResponse(html)


# ---------------------------------------------------------------------------
# CLI entry point
# ---------------------------------------------------------------------------


def main() -> None:
    """Run the application.

    Initialises the database schema, creates the runtime, and starts
    uvicorn.  Requires ``DATABASE_URL`` env var or uses a default
    local Postgres connection.
    """
    host = os.environ.get("HOST", "0.0.0.0")
    port = int(os.environ.get("PORT", "8000"))

    # Create tables
    db = DBService()
    db.create_orders_table()

    # Initialise runtime
    init_runtime()

    print(f"Starting transactional-outbox demo on {host}:{port}")
    uvicorn.run(app, host=host, port=port)


if __name__ == "__main__":
    main()
