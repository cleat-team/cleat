"""
Control Flow — Functional API host-side definitions.

Demonstrates parallel task execution (via concurrent durable_calls),
sequential for-loop processing, and conditional branching — all
orchestrated in the Cleat workflow.

Note: In the Temporal Functional API version, parallelism is achieved
by creating task futures without immediately awaiting them. In Cleat,
parallelism is approximated by issuing multiple ``durable_call``
invocations — but since Cleat's ``durable_call`` is synchronous,
true parallelism requires the host runtime to support it.
"""

from langgraph.func import task


@task
def validate_item(item: str) -> bool:
    """Validate an item. Returns True if non-empty and well-formed."""
    return len(item.strip()) > 0 and not item.startswith("INVALID:")


@task
def classify_item(item: str) -> str:
    """Classify an item as 'urgent' or 'normal'."""
    return "urgent" if "urgent" in item.lower() else "normal"


@task
def process_urgent(item: str) -> str:
    """Process an urgent item with priority handling."""
    return f"[PRIORITY] Processed: {item}"


@task
def process_normal(item: str) -> str:
    """Process a normal item with standard handling."""
    return f"[STANDARD] Processed: {item}"


@task
def summarize(results: list[str]) -> str:
    """Produce a summary of all processed results."""
    urgent_count = sum(1 for r in results if r.startswith("[PRIORITY]"))
    normal_count = sum(1 for r in results if r.startswith("[STANDARD]"))
    return (
        f"Processed {len(results)} items "
        f"({urgent_count} urgent, {normal_count} normal)"
    )


all_tasks = [
    validate_item,
    classify_item,
    process_urgent,
    process_normal,
    summarize,
]
