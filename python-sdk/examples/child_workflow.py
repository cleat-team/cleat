"""Child workflow example - demonstrates parent-child workflow patterns."""
import json
from dataclasses import dataclass
from typing import Optional
from cleat_sdk import HostCalls, cleat_entry, ChildResult


@dataclass
class OrderInput:
    order_id: str
    items: list[str]


@cleat_entry
def process_item(h: HostCalls, item: str) -> str:
    """Child workflow: process a single item."""
    h.cleat_log(f"Processing item: {item}")
    response = h.cleat_call("warehouse", "ProcessItem", {"item": item})
    return response


@cleat_entry
def process_order(h: HostCalls, input: OrderInput) -> str:
    """Parent workflow: fan out item processing to child workflows."""
    h.cleat_log(f"Processing order {input.order_id} with {len(input.items)} items")

    # Start child workflows for each item in parallel
    run_ids = []
    for item in input.items:
        run_id = h.child_workflow("process_item", item)
        run_ids.append(run_id)
        h.cleat_log(f"Started child workflow for {item}: {run_id}")

    # Await all children
    results: list[ChildResult] = h.await_all_children(run_ids)

    h.cleat_log(f"All items processed for order {input.order_id}")
    return json.dumps({
        "order_id": input.order_id,
        "results": [
            {"run_id": r.run_id, "result": r.result, "error": r.error}
            for r in results
        ],
    })
