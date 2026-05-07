"""
ReAct Agent — entry point.

Demonstrates:
1. Creating the LangGraph runtime and registering the ReAct agent graph.
2. Registering the LangGraph host service with the Cleat runtime.
3. Invoking the Cleat workflow with a user query.

Usage:
    python -m cleat_langgraph.graph_api.react_agent.main

Expected output:
    Agent answer: Here's what I found about San Francisco: [Tool] Weather...
"""

from __future__ import annotations

import logging

from cleat_langgraph import LangGraphRuntime
from cleat_langgraph.host import register_langgraph_service
from cleat_langgraph.graph_api.react_agent.graph import make_agent_graph

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Mock Cleat Runtime (for design study)
# ---------------------------------------------------------------------------


class MockCleatRuntime:
    """Minimal Cleat runtime stub for design study demonstration.

    See ``cleat_langgraph.graph_api.hello_world.main`` for full docs.
    """

    def __init__(self) -> None:
        self._services: dict[str, callable] = {}

    def register_service(self, name: str, handler: callable) -> None:
        self._services[name] = handler

    def invoke_workflow(self, workflow_name: str, input_data: str) -> str:
        from cleat_sdk import HostCalls

        class MockHostCalls:
            _step_cache: dict = {}

            def cleat_call(
                self, service: str, operation: str, request: dict
            ) -> dict:
                cache_key = (service, operation, str(request))
                if cache_key in self._step_cache:
                    logger.debug("REPLAY: returning cached result for %s", cache_key)
                    return self._step_cache[cache_key]
                if service in self._services:
                    result = self._services[service](operation, request)
                    self._step_cache[cache_key] = result
                    return result
                raise RuntimeError(f"Unknown service: {service}")

            def cleat_log(self, msg: str) -> None:
                logger.info("[workflow] %s", msg)

            def set_query_state(self, key: str, value: str) -> None:
                pass

            def get_state(self, key: str) -> str | None:
                return None

            def set_state(self, key: str, value: str) -> None:
                pass

            def cleat_sleep(self, ms: int) -> None:
                pass

            def await_signals(self, names: list[str], timeout_ms: int) -> dict:
                return {"signal_name": "", "value": None, "timed_out": True}

            def poll_signal(self, name: str) -> dict:
                return {"signal_name": name, "value": None, "timed_out": True}

        mock_h = MockHostCalls()
        mock_h._services = self._services

        from cleat_langgraph.graph_api.react_agent.workflow import (
            react_agent_workflow,
        )

        return react_agent_workflow(mock_h, input_data)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    """Run the ReAct agent sample."""
    logger.info("=== Cleat-LangGraph: ReAct Agent (Graph API) ===")

    # Step 1: Create the Cleat runtime
    cleat = MockCleatRuntime()

    # Step 2: Create LangGraph host runtime and register the graph
    langgraph = LangGraphRuntime()
    langgraph.register_graph("react-agent", make_agent_graph)

    # Step 3: Register the LangGraph service with the Cleat runtime
    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    # Step 4: Invoke the workflow
    query = "Tell me about San Francisco"
    logger.info("Query: %s", query)

    result = cleat.invoke_workflow("react_agent", query)

    # Step 5: Print the result
    print(f"Agent answer: {result}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
