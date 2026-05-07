"""
Hello World — entry point for the Cleat-LangGraph hello world sample.

This runs on the HOST side and:
1. Creates the Cleat runtime.
2. Registers the LangGraph graph.
3. Registers the LangGraph host service.
4. Invokes the Cleat workflow.

Usage:
    python -m cleat_langgraph.graph_api.hello_world.main

Expected output:
    Result: Processed: Hello, Cleat + LangGraph!
"""

from __future__ import annotations

import sys
import logging

# In a real deployment, these would be imported from the actual Cleat runtime.
# For this design study, we define a minimal mock runtime for illustration.
from cleat_langgraph import LangGraphRuntime
from cleat_langgraph.host import register_langgraph_service
from cleat_langgraph.graph_api.hello_world.graph import make_hello_graph

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Mock Cleat Runtime (for design study demonstration only)
# ---------------------------------------------------------------------------


class MockCleatRuntime:
    """Minimal Cleat runtime stub for design study illustration.

    In production, the Cleat runtime is provided by the host platform
    (WASM engine with durable execution). This mock demonstrates the
    service registration pattern.
    """

    def __init__(self) -> None:
        self._services: dict[str, callable] = {}

    def register_service(self, name: str, handler: callable) -> None:
        self._services[name] = handler
        logger.info("Service registered: %s", name)

    def invoke_workflow(self, workflow_name: str, input_data: str) -> str:
        """Simulate invoking a Cleat workflow.

        In reality, this would:
        1. Load the WASM module containing the ``@durable_entry`` function.
        2. Execute it in the sandbox.
        3. Intercept ``durable_call`` calls and dispatch to registered services.
        4. Record events in the durable log.
        5. Return the result.

        For the design study, we manually dispatch to the workflow function
        (bypassing WASM) to demonstrate the pattern.
        """
        from cleat_sdk import HostCalls

        # Create a mock HostCalls that dispatches to our registered services
        class MockHostCalls:
            def durable_call(self, service: str, operation: str, request: dict) -> dict:
                if service in self._services:
                    return self._services[service](operation, request)
                raise RuntimeError(f"Unknown service: {service}")

            def durable_log(self, msg: str) -> None:
                logger.info("[workflow log] %s", msg)

            def set_query_state(self, key: str, value: str) -> None:
                logger.debug("Query state set: %s = %s", key, value)

            def get_state(self, key: str) -> str | None:
                return None

            def set_state(self, key: str, value: str) -> None:
                logger.debug("State set: %s = %s", key, value)

            # Stub methods for completeness
            def durable_sleep(self, ms: int) -> None:
                logger.debug("Sleep: %d ms", ms)

            def await_signals(self, names: list[str], timeout_ms: int) -> dict:
                return {"signal_name": "", "value": None, "timed_out": True}

            def poll_signal(self, name: str) -> dict:
                return {"signal_name": name, "value": None, "timed_out": True}

        mock_h = MockHostCalls()
        mock_h._services = self._services  # type: ignore

        # Import and call the workflow
        from cleat_langgraph.graph_api.hello_world.workflow import (
            hello_world_workflow,
        )

        return hello_world_workflow(mock_h, input_data)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    """Run the hello world sample.

    Steps:
    1. Create the Cleat runtime.
    2. Create the LangGraph runtime and register the graph.
    3. Register the LangGraph host service.
    4. Invoke the workflow.
    5. Print the result.
    """
    logger.info("=== Cleat-LangGraph: Hello World ===")

    # Step 1: Create the Cleat runtime
    cleat = MockCleatRuntime()

    # Step 2: Create LangGraph host runtime and register the graph
    langgraph = LangGraphRuntime()
    langgraph.register_graph("hello-world", make_hello_graph)

    # Step 3: Register the LangGraph service with the Cleat runtime
    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    # Step 4: Invoke the workflow
    query = "Hello, Cleat + LangGraph!"
    logger.info("Input: %s", query)

    result = cleat.invoke_workflow("hello_world", query)

    # Step 5: Print the result
    print(f"Result: {result}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
