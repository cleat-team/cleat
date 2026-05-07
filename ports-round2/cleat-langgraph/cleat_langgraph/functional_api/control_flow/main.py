"""
Functional API Control Flow — entry point.
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

    def invoke_workflow(self, workflow_name: str, input_data: list[str]) -> dict:
        from cleat_sdk import HostCalls

        class MockHostCalls:
            def __init__(self, runtime) -> None:
                self._runtime = runtime
                self._step_cache: dict = {}

            def durable_call(
                self, service: str, operation: str, request: dict
            ) -> dict | str | bool:
                cache_key = f"{service}:{operation}:{str(request)}"
                if cache_key in self._step_cache:
                    return self._step_cache[cache_key]

                if operation == "execute_task":
                    task_name = request.get("task_name", "")
                    if task_name in self._runtime._tasks:
                        fn = self._runtime._tasks[task_name]
                        task_args = {k: v for k, v in request.items() if k != "task_name"}
                        result = fn(**task_args)
                        response = result if isinstance(result, (str, bool)) else {"result": result}
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

        from cleat_langgraph.functional_api.control_flow.workflow import (
            control_flow_workflow,
        )

        return control_flow_workflow(mock_h, input_data)


def main() -> None:
    logger.info("=== Cleat-LangGraph: Functional API Control Flow ===")

    cleat = MockCleatRuntime()
    langgraph = LangGraphRuntime()

    from cleat_langgraph.functional_api.control_flow.host import (
        validate_item,
        classify_item,
        process_urgent,
        process_normal,
        summarize,
    )
    cleat.register_task("validate_item", validate_item)
    cleat.register_task("classify_item", classify_item)
    cleat.register_task("process_urgent", process_urgent)
    cleat.register_task("process_normal", process_normal)
    cleat.register_task("summarize", summarize)

    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    items = [
        "urgent: server down",
        "normal: report due",
        "INVALID: bad data",
        "urgent: security patch",
        "normal: routine backup",
    ]
    result = cleat.invoke_workflow("control_flow", items)

    print(f"Summary: {result.get('summary', '')}")
    print(f"Total processed: {result.get('total', 0)}")
    for r in result.get("results", []):
        print(f"  {r}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
