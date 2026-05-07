"""
Continue-as-new — entry point.

Usage:
    python -m cleat_langgraph.graph_api.continue_as_new.main

Expected output:
    Pipeline result: 210
"""

from __future__ import annotations

import json
import logging

from cleat_langgraph import LangGraphRuntime
from cleat_langgraph.host import register_langgraph_service
from cleat_langgraph.graph_api.continue_as_new.graph import make_pipeline_graph

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)


class MockCleatRuntime:
    def __init__(self) -> None:
        self._services: dict[str, callable] = {}
        self._state_store: dict[str, str] = {}

    def register_service(self, name: str, handler: callable) -> None:
        self._services[name] = handler

    def invoke_workflow(self, workflow_name: str, input_data: int) -> int:
        from cleat_sdk import HostCalls

        class MockHostCalls:
            def __init__(self, runtime) -> None:
                self._runtime = runtime
                self._step_cache: dict = {}

            def durable_call(
                self, service: str, operation: str, request: dict
            ) -> dict:
                cache_key = f"{service}:{operation}:{str(request)}"
                if cache_key in self._step_cache:
                    return self._step_cache[cache_key]
                if service in self._runtime._services:
                    result = self._runtime._services[service](operation, request)
                    self._step_cache[cache_key] = result
                    return result
                raise RuntimeError(f"Unknown service: {service}")

            def durable_log(self, msg: str) -> None:
                logger.info("[workflow] %s", msg)

            def set_query_state(self, key: str, value: str) -> None:
                pass

            def get_state(self, key: str) -> str | None:
                return self._runtime._state_store.get(key)

            def set_state(self, key: str, value: str) -> None:
                logger.info("[state] %s = %s", key, value)
                self._runtime._state_store[key] = value

            def durable_sleep(self, ms: int) -> None:
                pass

            def await_signals(
                self, names: list[str], timeout_ms: int
            ) -> dict:
                return {"signal_name": "", "value": None, "timed_out": True}

            def poll_signal(self, name: str) -> dict:
                return {"signal_name": name, "value": None, "timed_out": True}

        mock_h = MockHostCalls(self)
        mock_h._services = self._services

        from cleat_langgraph.graph_api.continue_as_new.workflow import (
            pipeline_workflow,
        )

        return pipeline_workflow(mock_h, input_data)


def main() -> None:
    logger.info("=== Cleat-LangGraph: Continue-as-new Pipeline ===")

    cleat = MockCleatRuntime()
    langgraph = LangGraphRuntime()
    langgraph.register_graph("pipeline", make_pipeline_graph)
    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    input_val = 10
    result = cleat.invoke_workflow("pipeline", input_val)

    print(f"Pipeline result: {result}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
