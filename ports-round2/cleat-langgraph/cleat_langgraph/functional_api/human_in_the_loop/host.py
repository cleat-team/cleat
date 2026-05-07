"""
Human-in-the-loop chatbot — Functional API host-side definitions.

Defines LangGraph ``@task`` functions. Note that the ``interrupt()``
pattern from the Functional API is replaced with Cleat's signal
mechanism in the workflow layer.
"""

from langgraph.func import task


@task
def generate_draft(message: str) -> str:
    """Generate a draft response. Replace with an LLM call in production."""
    return (
        f"Here's my response to '{message}': "
        "The answer is 42. Let me know if this helps!"
    )


@task
def request_human_review(draft: str, feedback: str | None, approved: bool) -> str:
    """Process human feedback on a draft.

    Args:
        draft: The generated draft.
        feedback: Human feedback text (None if no feedback received).
        approved: Whether the draft was approved.

    Returns:
        The final response (approved draft or revised version).
    """
    if approved or feedback == "approve":
        return draft
    return f"[Revised] {draft} (incorporating feedback: {feedback})"


all_tasks = [generate_draft, request_human_review]
