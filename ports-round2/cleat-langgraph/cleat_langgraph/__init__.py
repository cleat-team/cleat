"""
Cleat-LangGraph Bridge: Durable Execution for LangGraph Agents

A design study port of the Temporal LangGraph plugin to the Cleat durable
execution framework. This bridge enables LangGraph workflows (both Graph API
and Functional API) to benefit from Cleat's durability guarantees.

Architecture
------------
The bridge operates in two layers:

1. **Host Layer** (outside WASM sandbox): Manages LangGraph graph compilation,
   node execution, and routing logic. LangGraph runs natively here with full
   access to async Python, LLM SDKs, and third-party libraries.

2. **Workflow Layer** (inside WASM sandbox): Cleat ``@durable_entry`` functions
   that orchestrate the LangGraph execution via ``HostCalls.durable_call()``.
   Each graph node / task execution is recorded as a durable event, enabling
   replay and crash recovery at per-node granularity.

Key Design Decisions
--------------------
- **Per-node durability**: Each LangGraph node/task is a separate
  ``durable_call``, matching the Temporal plugin's per-activity granularity.
- **Stateless host service**: All execution state is passed through the
  ``durable_call`` request/response cycle. The host caches compiled graphs
  but does not store execution state.
- **Sync bridge**: LangGraph's async execution is driven on the host side;
  the Cleat workflow boundary remains synchronous.
"""

from cleat_langgraph.bridge import (
    CleatLangGraph,
    LangGraphRuntime,
)
from cleat_langgraph.serialization import (
    CleatSerializer,
    LangGraphSerializer,
)

__all__ = [
    "CleatLangGraph",
    "LangGraphRuntime",
    "CleatSerializer",
    "LangGraphSerializer",
]
