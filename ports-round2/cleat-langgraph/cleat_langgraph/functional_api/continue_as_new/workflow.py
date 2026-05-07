"""
Continue-as-new — Cleat durable workflow (WASM-side).

Demonstrates the pipeline pattern with per-task result caching in
Cleat's durable state store. Each stage checkpoints its result,
so if any stage fails and the workflow replays, only uncompleted
stages are re-executed.

Compare with the Graph API version (``graph_api/continue_as_new/``)
which uses ``execute_node`` instead of ``execute_task``.
"""

from __future__ import annotations

import json

from cleat_sdk import HostCalls, durable_entry

CACHE_PREFIX = "pipeline:cache:"


@durable_entry(name="pipeline_functional")
def pipeline_functional_workflow(h: HostCalls, input_data: int) -> int:
    """Run a 3-stage pipeline with per-task result caching.

    Pipeline: input * 2 → + 50 → * 3

    Example: input 10 → 20 → 70 → 210

    Args:
        h: HostCalls context.
        input_data: The initial numeric value.

    Returns:
        The final pipeline result.
    """
    tasks = [
        ("double", lambda val: {"data": val}),
        ("add_50", lambda val: {"data": val}),
        ("triple", lambda val: {"data": val}),
    ]

    current_value = input_data

    for task_name, arg_builder in tasks:
        cache_key = f"{CACHE_PREFIX}{task_name}"
        cached = h.get_state(cache_key)

        if cached is not None:
            h.durable_log(f"{task_name}: restored from cache")
            try:
                current_value = int(json.loads(cached))
            except (json.JSONDecodeError, TypeError, ValueError):
                current_value = int(cached)
            continue

        h.durable_log(f"{task_name}: executing with input {current_value}")
        result = h.durable_call(
            "langgraph",
            "execute_task",
            {
                "task_name": task_name,
                **arg_builder(current_value),
            },
        )
        current_value = result if isinstance(result, int) else result.get("result", current_value)
        h.set_state(cache_key, json.dumps(current_value))

    h.durable_log(f"Pipeline complete: {input_data} → {current_value}")
    return current_value
