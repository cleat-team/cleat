"""
Human-in-the-loop chatbot — Cleat durable workflow (WASM-side).

Demonstrates the human-in-the-loop pattern with Cleat signals,
mapped from the LangGraph Functional API ``interrupt()`` pattern.
"""

from __future__ import annotations

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry(name="chatbot_functional")
def chatbot_functional_workflow(h: HostCalls, user_message: str) -> str:
    """Run a chatbot that pauses for human review.

    Workflow:
    1. Generate draft (via cleat_call to host-side task)
    2. Expose draft via query state
    3. Wait for human feedback via signal
    4. Process feedback (via cleat_call to host-side task)
    5. Return the final response

    Args:
        h: HostCalls context.
        user_message: The user's input message.

    Returns:
        The final response after human review.
    """
    h.cleat_log("Generating draft...")
    draft_result = h.cleat_call(
        "langgraph",
        "execute_task",
        {"task_name": "generate_draft", "input": user_message},
    )
    draft = draft_result if isinstance(draft_result, str) else draft_result.get("result", "")

    h.cleat_log(f"Draft: {draft[:60]}...")
    h.set_query_state("draft", draft)

    # Wait for human feedback
    h.cleat_log("Waiting for human_feedback signal...")
    signal = h.await_signals(["human_feedback"], timeout_ms=604800000)

    feedback = signal.value if not signal.timed_out else None
    approved = feedback == "approve"

    h.cleat_log(f"Feedback: {feedback}, approved: {approved}")

    # Process review
    review_result = h.cleat_call(
        "langgraph",
        "execute_task",
        {
            "task_name": "request_human_review",
            "draft": draft,
            "feedback": feedback,
            "approved": approved,
        },
    )
    final_response = review_result if isinstance(review_result, str) else review_result.get("result", draft)

    h.cleat_log("Chatbot functional workflow complete")
    return final_response
