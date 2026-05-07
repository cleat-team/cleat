"""
Functional API Hello World — entry point.

Usage:
    python -m cleat_langgraph.functional_api.hello_world.main
"""

from __future__ import annotations

import logging

from cleat_langgraph import LangGraphRuntime
from cleat_langgraph.host import register_langgraph_service

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)


class MockCleatRuntime:
    def __init__(self) -> None:
        self._services: dict[str, callable] = {}
        self._tasks: dict[str, callable] = {}

    def register_service(self, name: str, handler: callable) -> None:
        self._services[name] = handler

    def register_task(self, name: str, fn: callable) -> None:
        self._tasks[name] = fn

    def invoke_workflow(self, workflow_name: str, input_data: str) -> str:
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

                if operation == "execute_task":
                    task_name = request.get("task_name", "")
                    task_input = request.get("input", "")
                    if task_name in self._runtime._tasks:
                        fn = self._runtime._tasks[task_name]
                        result = fn(task_input)
                        response = {"result": result}
                        self._step_cache[cache_key] = response
                        return response

                if service in self._runtime._services:
                    result = self._runtime._services[service](operation, request)
                    self._step_cache[cache_key] = result
                    return result

                raise RuntimeError(f"Unknown operation: {operation}")

            def durable_log(self, msg: str) -> None:
                logger.info("[workflow] %s", msg)

            def set_query_state(self, key: str, value: str) -> None:
                pass

            def get_state(self, key: str) -> str | None:
                return None

            def set_state(self, key: str, value: str) -> None:
                pass

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

        from cleat_langgraph.functional_api.hello_world.workflow import (
            hello_functional_workflow,
        )

        return hello_functional_workflow(mock_h, input_data)


def main() -> None:
    logger.info("=== Cleat-LangGraph: Functional API Hello World ===")

    cleat = MockCleatRuntime()
    langgraph = LangGraphRuntime()

    # Register the task directly with the runtime
    from cleat_langgraph.functional_api.hello_world.host import process_query
    cleat.register_task("process_query", process_query)

    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    query = "Hello, Functional API!"
    result = cleat.invoke_workflow("hello_functional", query)

    print(f"Result: {result}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
