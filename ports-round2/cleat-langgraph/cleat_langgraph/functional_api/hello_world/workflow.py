"""
Hello World — LangGraph Functional API (host-side) + Cleat workflow.

The Functional API uses ``@task`` for individual work items and
``@entrypoint`` to orchestrate them. In the Cleat port, each ``@task``
maps to a ``durable_call`` (per-task durability), and the ``@entrypoint``
maps to the ``@durable_entry`` workflow.

Architecture:
  - ``process_query`` is a LangGraph ``@task`` that runs on the host side.
  - The Cleat workflow calls ``h.durable_call("langgraph", "execute_task",
    {"task_name": "process_query", "input": query})`` for each task.
  - On replay, the cached result is returned without re-execution.
"""

from __future__ import annotations

from cleat_sdk import HostCalls, durable_entry


@durable_entry(name="hello_functional")
def hello_functional_workflow(h: HostCalls, query: str) -> str:
    """Process a query through a single LangGraph task.

    Equivalent to the Functional API::

        @entrypoint()
        async def hello_entrypoint(query: str) -> dict:
            result = await process_query(query)
            return {"result": result}

    Args:
        h: HostCalls context.
        query: The input string to process.

    Returns:
        The processed result.
    """
    # Execute the task via durable_call
    result = h.durable_call(
        "langgraph",
        "execute_task",
        {
            "task_name": "process_query",
            "input": query,
        },
    )

    return result.get("result", "")
