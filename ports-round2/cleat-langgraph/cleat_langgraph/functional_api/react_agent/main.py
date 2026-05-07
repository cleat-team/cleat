"""
Functional API ReAct Agent — entry point.

Usage:
    python -m cleat_langgraph.functional_api.react_agent.main

Expected output:
    Agent answer: Here's what I found about San Francisco: [...]
    Tool calls made: 2
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

    def invoke_workflow(self, workflow_name: str, input_data: str) -> dict:
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
                    logger.debug("REPLAY: %s", cache_key)
                    return self._step_cache[cache_key]

                if operation == "execute_task":
                    task_name = request.get("task_name", "")
                    if task_name in self._runtime._tasks:
                        fn = self._runtime._tasks[task_name]
                        if task_name == "agent_think":
                            result = fn(
                                request.get("input", ""),
                                request.get("history", []),
                            )
                        else:
                            result = fn(
                                request.get("tool_name", ""),
                                request.get("tool_input", ""),
                            )
                        response = dict(result) if isinstance(result, dict) else {"result": result}
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

        from cleat_langgraph.functional_api.react_agent.workflow import (
            react_agent_functional_workflow,
        )

        return react_agent_functional_workflow(mock_h, input_data)


def main() -> None:
    logger.info("=== Cleat-LangGraph: Functional API ReAct Agent ===")

    cleat = MockCleatRuntime()
    langgraph = LangGraphRuntime()

    # Register tasks
    from cleat_langgraph.functional_api.react_agent.host import (
        agent_think,
        execute_tool,
    )
    cleat.register_task("agent_think", agent_think)
    cleat.register_task("execute_tool", execute_tool)

    register_langgraph_service(cleat, langgraph, service_name="langgraph")

    query = "Tell me about San Francisco"
    result = cleat.invoke_workflow("react_agent_functional", query)

    print(f"Agent answer: {result.get('answer', 'No answer')}")
    print(f"Tool calls made: {result.get('steps', 0)}")
    logger.info("=== Done ===")


if __name__ == "__main__":
    main()
