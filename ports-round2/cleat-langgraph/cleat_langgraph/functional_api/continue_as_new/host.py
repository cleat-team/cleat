"""
Continue-as-new — Functional API host-side definitions.

Defines LangGraph ``@task`` functions for the multi-stage pipeline.
"""

from langgraph.func import entrypoint, task


@task
def double(data: int) -> int:
    """Stage 1: double the input."""
    return data * 2


@task
def add_50(data: int) -> int:
    """Stage 2: add 50."""
    return data + 50


@task
def triple(data: int) -> int:
    """Stage 3: triple the result."""
    return data * 3


@entrypoint()
async def pipeline_entrypoint(data: int) -> dict:
    """Run the 3-stage pipeline: double → add_50 → triple.

    This is the reference implementation. In the Cleat version, each
    task is called individually via cleat_call for per-task caching.
    """
    doubled = await double(data)
    plus_50 = await add_50(doubled)
    tripled = await triple(plus_50)
    return {"result": tripled}


all_tasks = [double, add_50, triple]
