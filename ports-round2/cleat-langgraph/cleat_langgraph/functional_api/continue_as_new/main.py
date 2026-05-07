"""
Functional API Continue-as-new — entry point.
"""

from __future__ import annotations

import json
import logging

from cleat_langgraph import LangGraphRuntime
from cleat_langgraph.host import register_langgraph_service

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)


class MockCleatRuntime:
    def __init__(self) -> None:
        self._services: dict[str, callable] = {}
        self._tasks: dict[str, callable] = {}
        self._state_store: dict[str, str] = {}

    def register_service(self, name: str, handler: callable) -> None:
        self._services[name] = handler

    def register_task(self, name: str, fn: callable) -> None:
        self._tasks[name] = fn

    def invoke_workflow(self, workflow_name: str, input_data: int) -> int:
        from cleat_sdk import HostCalls

        class MockHostCalls:
            def __init__(self, runtime) -> None:
                self._runtime = runtime
                self._step_cache: dict = {}

            def durable_call(
                self, service: str, operation: str, request: dict
            ) -> dict | int:
                cache_key = f"{service}:{operation}:{str(request)}"
                if cache_key in self._step_cache:
                    return self._step_cache[cache_key]

                if operation == "execute_task":
                    task_name = request.get("task_name", "")
                    if task_name in self._runtime._tasks:
                        fn = self._runtime._tasks[task_name]
                        result = fn(request.get("data", 0))
                        response = result if isinstance(result, int) else {"result": result}
                        self._step_cache[cache_key] = response
                        return response

                if service in self._runtime._services:
                    result = self._runtime._services[service](operation, request)
                    self._step_cache[cache_key] = result
                    return result

                raise RuntimeError(f"Unknown: {service}/{operation}")

            def durable_log(self, msg: str) -> None:
                logger.info("[workflow] %s", msg)

            def set_query_state(self, key: str, value: str) -> None:
                pass

            def get_state(self, key: str) -> str | None:
                return self._runtime._state_store.get(key)

            def set_state(self, key: str, value: str) -> None:
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

        from cleat_langgraph.functional_api.continue_as_new.workflow import (
            pipeline_functional_workflow,
        )

        return pipeline_functional_workflow(mock_h, input_data)


def main() -> None:
    logger.info("=== Cleat-LangGraph: Functional API Continue-as-new ===")

    cleat = MockCleatRuntime()
    langgraph = LangGraphRuntime()

    from cleat_langgraph.functional_api.continue_as_new.host import (
        double,
        add_50,
        triple,
    )
    cleat.register_task("double", double)
    cleat.register_task("add_50", add_50)
    cleat.register_task("triple", triple)

    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    input_val = 10
    result = cleat.invoke_workflow("pipeline_functional", input_val)

    print(f"Pipeline result: {result} (expected: 210)")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
