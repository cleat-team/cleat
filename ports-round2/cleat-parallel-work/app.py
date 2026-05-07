"""
Fan-out/fan-in parallel work execution pattern using the Cleat Python SDK.

Demonstrates parallel execution by splitting a task description into subtasks,
executing each subtask as an independent child workflow in parallel (fan-out),
waiting for all to complete (fan-in), and aggregating the results.

This is a direct port of the Restate ``patterns-use-cases/parallelizework``
example, adapted to the Cleat durable execution model.

Restate original pattern::

    subtasks = await ctx.run_typed("split task", split, task=task)
    result_promises = [
        ctx.run_typed(f"execute {subtask}", execute_subtask, subtask=subtask)
        for subtask in subtasks.subtasks
    ]
    results_done = await restate.gather(*result_promises)
    results = [await result for result in results_done]
    return aggregate(results)

Cleat equivalent pattern::

    for subtask in subtask_list:
        run_id = h.child_workflow("execute_subtask", {"subtask": subtask})
        run_ids.append(run_id)
    child_results = h.await_all_children(run_ids)
    # aggregate child_results
"""

import json
from typing import Optional

from cleat_sdk import HostCalls, durable_entry, ChildResult


# ---------------------------------------------------------------------------
# Child workflow: execute one subtask
# ---------------------------------------------------------------------------


@durable_entry
def execute_subtask(h: HostCalls, subtask: str) -> str:
    """Execute a single subtask as an independent child workflow.

    Each invocation runs as an independent child workflow, allowing the
    host runtime to execute multiple instances concurrently (fan-out).

    On first execution:
      1. Logs the start.
      2. Calls ``durable_sleep`` to simulate work (triggers suspension).
      3. On resume, logs completion and returns the result.

    On replay:
      The recorded sleep and result are replayed deterministically.
    """
    h.durable_log(f"[execute_subtask] Started: {subtask}")

    # Simulate variable-duration work with a deterministic sleep.
    # In a real application this would be a call to an external service
    # via ``h.durable_call("myservice", "DoWork", {"subtask": subtask})``.
    h.durable_sleep(5000)

    h.durable_log(f"[execute_subtask] Completed: {subtask}")
    return f"{subtask}: DONE"


# ---------------------------------------------------------------------------
# Parent workflow: fan-out worker
# ---------------------------------------------------------------------------


def _aggregate_results(results: list[str]) -> str:
    """Combine individual result strings into a single result.

    This is a pure function that runs locally without journaling.  It is safe
    because it has no side effects and produces the same output on replay.
    """
    return ", ".join(results)


@durable_entry
def fan_out_worker(h: HostCalls, task: str) -> str:
    """Fan-out/fan-in worker: split, fan out, fan in, aggregate.

    Parameters
    ----------
    h : HostCalls
        The host calls interface for durable operations.
    task : str
        A comma-separated string of subtask descriptions, e.g.
        ``"process_order, send_email, update_inventory"``.

    Returns
    -------
    str
        A comma-separated string of aggregated subtask results.
    """
    h.durable_log(f"[fan_out_worker] Starting with task: {task}")

    # --- Split (pure, runs locally, no journaling needed) ------------------
    subtasks = [s.strip() for s in task.split(",") if s.strip()]
    h.durable_log(f"[fan_out_worker] Split into {len(subtasks)} subtask(s)")

    if not subtasks:
        h.durable_log("[fan_out_worker] No subtasks to process")
        return ""

    # --- Fan out: start a child workflow for each subtask ------------------
    run_ids: list[str] = []
    for i, subtask in enumerate(subtasks):
        # The child workflow's ``@durable_entry`` expects a "subtask" key in
        # the JSON input.  Passing a dict ensures the child workflow function
        # receives ``subtask=<str>`` as a keyword argument.
        run_id = h.child_workflow("execute_subtask", {"subtask": subtask})
        run_ids.append(run_id)
        h.durable_log(f"[fan_out_worker] Started child[{i}]: {run_id} -> {subtask}")

    # --- Fan in: wait for ALL child workflows to complete ------------------
    # ``await_all_children`` is the Cleat equivalent of ``restate.gather()``.
    # It blocks until every child in the list has finished, then returns a
    # list of ``ChildResult`` in the same order as *run_ids*.
    h.durable_log(f"[fan_out_worker] Awaiting {len(run_ids)} children...")
    child_results: list[ChildResult] = h.await_all_children(run_ids)
    h.durable_log(f"[fan_out_worker] All {len(child_results)} children completed")

    # --- Collect results, handling errors from individual children ---------
    # ``ChildResult.error`` is ``None`` for successful children and a
    # non-empty string for failed ones.
    result_strings: list[str] = []
    for i, cr in enumerate(child_results):
        if cr.error:
            h.durable_log(f"[fan_out_worker] Child[{i}] FAILED: {cr.error}")
            result_strings.append(f"ERROR(subtask[{i}]): {cr.error}")
        else:
            # ``cr.result`` is the JSON-encoded return value from the child
            # workflow.  Since the child returned a plain string, we decode
            # it back.
            try:
                decoded = json.loads(cr.result) if cr.result else ""
                result_strings.append(str(decoded))
            except (json.JSONDecodeError, TypeError):
                result_strings.append(str(cr.result))

    # --- Aggregate (pure, runs locally) -----------------------------------
    aggregated = _aggregate_results(result_strings)
    h.durable_log(f"[fan_out_worker] Aggregated: {aggregated}")
    return aggregated


# ---------------------------------------------------------------------------
# Error-handling variant: stop-on-first-failure
# ---------------------------------------------------------------------------


@durable_entry
def fan_out_worker_stop_on_error(h: HostCalls, task: str) -> str:
    """Variant that raises on first child failure instead of collecting errors.

    This demonstrates the alternative strategy: fail fast when any parallel
    task fails, rather than collecting partial results.

    Note: The child workflows that are still running when one fails may
    continue to completion on the host.  The parent only propagates the
    error; it does not cancel the other children (Cleat does not currently
    support child cancellation).
    """
    h.durable_log(f"[stop_on_error] Starting with task: {task}")

    subtasks = [s.strip() for s in task.split(",") if s.strip()]
    if not subtasks:
        return ""

    run_ids = [
        h.child_workflow("execute_subtask", {"subtask": s})
        for s in subtasks
    ]

    child_results = h.await_all_children(run_ids)

    for i, cr in enumerate(child_results):
        if cr.error:
            raise RuntimeError(
                f"Child workflow for subtask[{i}] failed: {cr.error}"
            )

    result_strings: list[str] = []
    for cr in child_results:
        try:
            decoded = json.loads(cr.result) if cr.result else ""
            result_strings.append(str(decoded))
        except (json.JSONDecodeError, TypeError):
            result_strings.append(str(cr.result))

    return _aggregate_results(result_strings)
