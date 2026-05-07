"""
Control Flow — Cleat durable workflow (WASM-side).

Demonstrates sequential processing and conditional branching with
per-task durability. Each ``cleat_call`` is recorded as a separate
event, enabling replay at per-item granularity.

Key difference from the Temporal version:
  - Temporal: ``validation_futures = [validate_item(item) for item in items]``
    creates concurrent futures, then ``[await f for f in validation_futures]``
    waits for all. This enables parallel task execution.
  - Cleat: ``cleat_call`` is synchronous. Items are validated sequentially
    (or via the host runtime if it supports parallel dispatch).
"""

from __future__ import annotations

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry(name="control_flow")
def control_flow_workflow(h: HostCalls, items: list[str]) -> dict:
    """Process a batch of items with validation, classification, and routing.

    Workflow:
    1. Validate each item (sequential cleat_calls).
    2. Classify and process valid items (sequential with conditional routing).
    3. Summarize results.

    Args:
        h: HostCalls context.
        items: List of items to process.

    Returns:
        dict with ``results``, ``summary``, and ``total``.
    """
    h.cleat_log(f"Processing {len(items)} items")

    # Step 1: Validate all items (sequential; see note in module docstring)
    valid_items: list[str] = []

    for item in items:
        is_valid = h.cleat_call(
            "langgraph",
            "execute_task",
            {"task_name": "validate_item", "item": item},
        )
        if isinstance(is_valid, dict) and is_valid.get("result"):
            valid_items.append(item)
        elif is_valid is True:
            valid_items.append(item)

    h.cleat_log(f"{len(valid_items)} valid items out of {len(items)}")

    # Step 2: Process each valid item
    results: list[str] = []

    for item in valid_items:
        # Classify
        category = h.cleat_call(
            "langgraph",
            "execute_task",
            {"task_name": "classify_item", "item": item},
        )
        cat = category if isinstance(category, str) else category.get("result", "normal")

        # Route based on classification
        if cat == "urgent":
            task_name = "process_urgent"
        else:
            task_name = "process_normal"

        processed = h.cleat_call(
            "langgraph",
            "execute_task",
            {"task_name": task_name, "item": item},
        )
        result = processed if isinstance(processed, str) else processed.get("result", "")
        results.append(result)

    # Step 3: Summarize
    summary = h.cleat_call(
        "langgraph",
        "execute_task",
        {"task_name": "summarize", "results": results},
    )
    summary_text = summary if isinstance(summary, str) else summary.get("result", "")

    return {
        "results": results,
        "summary": summary_text,
        "total": len(results),
    }
