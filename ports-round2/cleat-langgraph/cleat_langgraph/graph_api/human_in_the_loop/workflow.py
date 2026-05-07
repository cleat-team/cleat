"""
Human-in-the-loop chatbot — Cleat durable workflow (WASM-side).

Demonstrates the human-in-the-loop pattern using Cleat's signal mechanism
instead of LangGraph's ``interrupt()``.

Key pattern differences from the Temporal version:
  - Temporal: ``workflow.wait_condition(lambda: self._human_input is not None)``
    waits indefinitely; signals deliver input to the workflow object.
  - Cleat: ``h.await_signals(["human_feedback"], timeout_ms=604800000)``
    pauses the workflow until a signal arrives or timeout expires.

Signal model in Cleat:
  The Cleat runtime supports signal handling. The workflow calls
  ``await_signals()`` which blocks until a signal is received or timeout.
  On replay, the signal delivery is replayed from the event log.
"""

from __future__ import annotations

from cleat_sdk import HostCalls, durable_entry
from cleat_langgraph import CleatLangGraph


@durable_entry(name="chatbot")
def chatbot_workflow(h: HostCalls, user_message: str) -> str:
    """Run a chatbot that pauses for human review.

    Workflow:
    1. Generate a draft response (graph node via durable_call)
    2. Expose the draft via query state
    3. Wait for human feedback via signal (up to 7 days)
    4. Process the feedback (graph node via durable_call)
    5. Return the final response

    Args:
        h: HostCalls context (injected by the Cleat runtime).
        user_message: The user's input message.

    Returns:
        The final chatbot response after human review.
    """
    agent = CleatLangGraph(h, "chatbot")
    state: dict = {
        "value": user_message,
        "feedback": None,
        "approved": False,
        "__node__": "__start__",
    }

    # Step 1: Generate draft
    h.durable_log("Generating draft response...")
    state = agent.step(state)  # runs generate_draft node
    draft = state.get("value", "")
    h.durable_log(f"Draft generated: {draft[:60]}...")

    # Expose draft via query state for external polling
    h.set_query_state("draft", draft)

    # Step 2: Wait for human feedback via signal
    h.durable_log("Waiting for human feedback (signal: human_feedback)...")
    # Wait up to 7 days for human feedback
    signal_result = h.await_signals(["human_feedback"], timeout_ms=604800000)

    if signal_result.timed_out:
        h.durable_log("Timed out waiting for human feedback")
        # Default: proceed with the draft as-is
        state["approved"] = True
    else:
        feedback = signal_result.value
        h.durable_log(f"Received human feedback: {feedback}")
        state["feedback"] = feedback
        state["approved"] = feedback == "approve"

    # Step 3: Process human review
    state = agent.step(state)  # runs human_review node
    final_response = state.get("value", draft)

    h.durable_log("Chatbot workflow complete")
    return final_response
