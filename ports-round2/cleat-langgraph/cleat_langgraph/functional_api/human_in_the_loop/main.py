"""
Functional API Human-in-the-loop chatbot — entry point.
"""

from __future__ import annotations

import logging
import threading

from cleat_langgraph import LangGraphRuntime
from cleat_langgraph.host import register_langgraph_service

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
logger = logging.getLogger(__name__)


class MockCleatRuntime:
    def __init__(self) -> None:
        self._services: dict[str, callable] = {}
        self._tasks: dict[str, callable] = {}
        self._pending_signals: dict[str, str | None] = {}

    def register_service(self, name: str, handler: callable) -> None:
        self._services[name] = handler

    def register_task(self, name: str, fn: callable) -> None:
        self._tasks[name] = fn

    def send_signal(self, signal_name: str, value: str | None = None) -> None:
        self._pending_signals[signal_name] = value

    def invoke_workflow(self, workflow_name: str, input_data: str) -> str:
        import time
        from cleat_sdk import HostCalls

        class MockHostCalls:
            def __init__(self, runtime) -> None:
                self._runtime = runtime
                self._step_cache: dict = {}

            def durable_call(
                self, service: str, operation: str, request: dict
            ) -> dict | str:
                cache_key = f"{service}:{operation}:{str(request)}"
                if cache_key in self._step_cache:
                    return self._step_cache[cache_key]

                if operation == "execute_task":
                    task_name = request.get("task_name", "")
                    if task_name in self._runtime._tasks:
                        fn = self._runtime._tasks[task_name]
                        if task_name == "generate_draft":
                            result = fn(request.get("input", ""))
                        elif task_name == "request_human_review":
                            result = fn(
                                request.get("draft", ""),
                                request.get("feedback"),
                                request.get("approved", False),
                            )
                        else:
                            result = fn(**{k: v for k, v in request.items() if k != "task_name"})
                        response = result if isinstance(result, str) else {"result": result}
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
                logger.info("[query] %s = %s", key, value)

            def get_state(self, key: str) -> str | None:
                return None

            def set_state(self, key: str, value: str) -> None:
                pass

            def durable_sleep(self, ms: int) -> None:
                pass

            def await_signals(
                self, names: list[str], timeout_ms: int
            ) -> dict:
                deadline = time.time() + (timeout_ms / 1000)
                while time.time() < deadline:
                    for name in names:
                        if name in self._runtime._pending_signals:
                            value = self._runtime._pending_signals.pop(name)
                            return {"signal_name": name, "value": value, "timed_out": False}
                    time.sleep(0.1)
                return {"signal_name": "", "value": None, "timed_out": True}

            def poll_signal(self, name: str) -> dict:
                if name in self._runtime._pending_signals:
                    value = self._runtime._pending_signals.pop(name)
                    return {"signal_name": name, "value": value, "timed_out": False}
                return {"signal_name": name, "value": None, "timed_out": True}

        mock_h = MockHostCalls(self)
        mock_h._services = self._services

        from cleat_langgraph.functional_api.human_in_the_loop.workflow import (
            chatbot_functional_workflow,
        )

        return chatbot_functional_workflow(mock_h, input_data)


def main() -> None:
    logger.info("=== Cleat-LangGraph: Functional API Human-in-the-loop ===")

    cleat = MockCleatRuntime()
    langgraph = LangGraphRuntime()

    from cleat_langgraph.functional_api.human_in_the_loop.host import (
        generate_draft,
        request_human_review,
    )
    cleat.register_task("generate_draft", generate_draft)
    cleat.register_task("request_human_review", request_human_review)
    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    def deliver() -> None:
        import time
        time.sleep(5)
        cleat.send_signal("human_feedback", "approve")

    threading.Thread(target=deliver, daemon=True).start()

    result = cleat.invoke_workflow("chatbot_functional", "What is the meaning of life?")
    print(f"Final response: {result}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
