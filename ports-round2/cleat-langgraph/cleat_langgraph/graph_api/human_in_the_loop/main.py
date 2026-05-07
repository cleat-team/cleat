"""
Human-in-the-loop chatbot — entry point.

Demonstrates:
1. Registering the chatbot graph with the LangGraph runtime.
2. Registering the LangGraph host service.
3. Invoking the workflow.
4. Delivering human feedback via signal.

Usage:
    python -m cleat_langgraph.graph_api.human_in_the_loop.main

Expected output:
    Draft generated: Here's my response to 'What is the meaning of life?'...
    Final response: Here's my response to 'What is the meaning of life?'...
"""

from __future__ import annotations

import logging

from cleat_langgraph import LangGraphRuntime
from cleat_langgraph.host import register_langgraph_service
from cleat_langgraph.graph_api.human_in_the_loop.graph import make_chatbot_graph

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Mock Cleat Runtime
# ---------------------------------------------------------------------------


class MockCleatRuntime:
    """Minimal Cleat runtime stub for design study."""

    def __init__(self) -> None:
        self._services: dict[str, callable] = {}
        self._pending_signals: dict[str, str | None] = {}

    def register_service(self, name: str, handler: callable) -> None:
        self._services[name] = handler

    def send_signal(self, signal_name: str, value: str | None = None) -> None:
        """Deliver a signal (simulates external signal delivery)."""
        self._pending_signals[signal_name] = value

    def invoke_workflow(self, workflow_name: str, input_data: str) -> str:
        from cleat_sdk import HostCalls

        class MockHostCalls:
            def __init__(self, runtime) -> None:
                self._runtime = runtime
                self._step_cache: dict = {}
                self._query_state: dict = {}

            def cleat_call(
                self, service: str, operation: str, request: dict
            ) -> dict:
                cache_key = f"{service}:{operation}:{str(request)}"
                if cache_key in self._step_cache:
                    logger.debug("REPLAY: %s", cache_key)
                    return self._step_cache[cache_key]
                if service in self._runtime._services:
                    result = self._runtime._services[service](operation, request)
                    self._step_cache[cache_key] = result
                    return result
                raise RuntimeError(f"Unknown service: {service}")

            def cleat_log(self, msg: str) -> None:
                logger.info("[workflow] %s", msg)

            def set_query_state(self, key: str, value: str) -> None:
                self._query_state[key] = value
                logger.info("[query state] %s = %s", key, value)

            def await_signals(
                self, names: list[str], timeout_ms: int
            ) -> dict:
                """Simulate signal delivery with polling."""
                import time

                logger.info("[signal] Waiting for signals: %s", names)
                deadline = time.time() + (timeout_ms / 1000)
                while time.time() < deadline:
                    for name in names:
                        if name in self._runtime._pending_signals:
                            value = self._runtime._pending_signals.pop(name)
                            logger.info("[signal] Received: %s = %s", name, value)
                            return {
                                "signal_name": name,
                                "value": value,
                                "timed_out": False,
                            }
                    time.sleep(0.1)
                logger.info("[signal] Timed out")
                return {"signal_name": "", "value": None, "timed_out": True}

            def get_state(self, key: str) -> str | None:
                return None

            def set_state(self, key: str, value: str) -> None:
                pass

            def cleat_sleep(self, ms: int) -> None:
                pass

            def poll_signal(self, name: str) -> dict:
                if name in self._runtime._pending_signals:
                    value = self._runtime._pending_signals.pop(name)
                    return {"signal_name": name, "value": value, "timed_out": False}
                return {"signal_name": name, "value": None, "timed_out": True}

        mock_h = MockHostCalls(self)
        mock_h._services = self._services

        from cleat_langgraph.graph_api.human_in_the_loop.workflow import (
            chatbot_workflow,
        )

        return chatbot_workflow(mock_h, input_data)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    """Run the human-in-the-loop chatbot sample."""
    logger.info("=== Cleat-LangGraph: Human-in-the-loop Chatbot ===")

    # Create runtimes
    cleat = MockCleatRuntime()
    langgraph = LangGraphRuntime()
    langgraph.register_graph("chatbot", make_chatbot_graph)
    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    # Simulate: start workflow, it will pause waiting for signal
    user_message = "What is the meaning of life?"
    logger.info("User message: %s", user_message)

    # In a real system, the workflow would be running and we'd send a
    # signal to it. Here we simulate by pre-setting a signal.
    import threading

    def deliver_signal() -> None:
        import time
        time.sleep(7)  # Simulate human thinking time
        logger.info("[external] Sending human feedback: approve")
        cleat.send_signal("human_feedback", "approve")

    threading.Thread(target=deliver_signal, daemon=True).start()

    result = cleat.invoke_workflow("chatbot", user_message)

    print(f"Final response: {result}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
