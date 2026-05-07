"""
Continue-as-new — Cleat durable workflow (WASM-side).

Demonstrates the multi-stage pipeline pattern. In the Temporal version, each
pipeline stage runs in a separate workflow execution via ``continue-as-new``.
The ``cache`` mechanism prevents re-executing completed stages.

In Cleat, we use a different approach:
1. Each stage checkpoints its result in durable state (``h.set_state``).
2. Before executing a stage, check if a cached result exists.
3. If the workflow restarts (replay), completed stages are skipped.
4. ``child_workflow`` can be used for true multi-execution patterns.

This avoids re-executing completed stages while keeping all stages within
a single workflow execution.
"""

from __future__ import annotations

import json

from cleat_sdk import HostCalls, cleat_entry
from cleat_langgraph import CleatLangGraph

# State keys for caching stage results
CACHE_PREFIX = "pipeline:cache:"


@cleat_entry(name="pipeline")
def pipeline_workflow(h: HostCalls, input_data: int) -> int:
    """Run a 3-stage pipeline with result caching.

    Each stage:
    1. Checks for a cached result in durable state.
    2. If cached, returns the cached value (no re-execution).
    3. If not cached, executes the stage and caches the result.

    Pipeline: input * 2 → + 50 → * 3

    Example: input 10 → 20 → 70 → 210

    Args:
        h: HostCalls context.
        input_data: The initial numeric value.

    Returns:
        The final pipeline result.
    """
    agent = CleatLangGraph(h, "pipeline")
    state: dict = {
        "value": input_data,
        "__node__": "__start__",
    }

    stages = ["double", "add_50", "triple"]

    for stage in stages:
        # Check cache
        cache_key = f"{CACHE_PREFIX}{stage}"
        cached = h.get_state(cache_key)

        if cached is not None:
            # Restore from cache — this stage was already completed
            try:
                cached_value = int(json.loads(cached))
            except (json.JSONDecodeError, TypeError, ValueError):
                cached_value = int(cached)
            state["value"] = cached_value
            h.cleat_log(f"{stage}: using cached result {cached_value}")
            # Mark the node as visited in the state
            state["__node__"] = stage
            continue

        # Execute the stage (cleat_call)
        h.cleat_log(f"{stage}: executing with value {state['value']}")
        state = agent.execute_node(stage, state)

        # Cache the result
        h.set_state(cache_key, json.dumps(state["value"]))

    h.cleat_log(f"Pipeline complete: {input_data} → {state['value']}")
    return state["value"]
