"""
LLM Plugin Design: Proposal for a Cleat LLM Plugin.

This is a design study for what a Cleat LLM Plugin would look like.
It is NOT a working implementation — LangGraph, LangChain, and LLM
SDKs cannot run inside the WASM sandbox.

The plugin would be a host-side service (like the LangGraph bridge)
that wraps LLM calls as durable operations, enabling per-LLM-call
checkpointing and replay.

Current gap: no LLM plugin exists (issue #49 for Python, #86 for AS).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional


# ---------------------------------------------------------------------------
# Data types
# ---------------------------------------------------------------------------


@dataclass
class LLMMessage:
    """A message in an LLM conversation."""
    role: str  # "system", "user", "assistant", "tool"
    content: str
    tool_calls: List[Dict[str, Any]] = field(default_factory=list)
    tool_call_id: Optional[str] = None


@dataclass
class LLMResponse:
    """Response from an LLM call."""
    content: str
    tool_calls: List[Dict[str, Any]] = field(default_factory=list)
    usage: Dict[str, int] = field(default_factory=dict)
    model: str = ""
    finish_reason: str = ""


@dataclass
class LLMConfig:
    """Configuration for an LLM call."""
    model: str = "claude-sonnet-4-20250506"
    max_tokens: int = 4096
    temperature: float = 0.7
    system_prompt: str = ""
    tools: List[Dict[str, Any]] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Host-side LLM Service
# ---------------------------------------------------------------------------


class LLMHostService:
    """Host-side LLM service for Cleat workflows.

    Registered as a Cleat host service so that Cleat workflows can call::

        h.durable_call("llm", "chat", {
            "messages": [...],
            "config": {...},
        })

    Each LLM call is recorded in the event log. On replay, the cached
    response is returned without re-invoking the LLM.
    """

    def __init__(self) -> None:
        self._clients: Dict[str, Any] = {}

    def register_client(self, provider: str, client: Any) -> None:
        """Register an LLM client (e.g., Anthropic, OpenAI)."""
        self._clients[provider] = client

    def handle_chat(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Handle a chat completion request.

        Args:
            request: Must contain ``messages`` (list of dicts with
                ``role`` and ``content``) and optionally ``config``.

        Returns:
            LLM response with ``content``, ``tool_calls``, etc.
        """
        messages = request.get("messages", [])
        config_data = request.get("config", {})
        config = LLMConfig(**config_data)
        provider = config_data.get("provider", "anthropic")

        # In production, dispatch to the registered LLM client
        client = self._clients.get(provider)
        if client:
            return self._call_llm(client, messages, config)

        # Simulated response for design study
        return {
            "content": f"Simulated response to {len(messages)} messages",
            "tool_calls": [],
            "usage": {"input_tokens": 50, "output_tokens": 20},
            "model": config.model,
            "finish_reason": "end_turn",
        }

    def _call_llm(
        self, client: Any, messages: List[Dict[str, Any]], config: LLMConfig
    ) -> Dict[str, Any]:
        """Make the actual LLM API call."""
        # This would use the Anthropic/OpenAI SDK
        raise NotImplementedError(
            "LLM plugin requires a real LLM client. "
            "Register one via register_client()."
        )


# ---------------------------------------------------------------------------
# Workflow-side LLM Helper
# ---------------------------------------------------------------------------


class CleatLLM:
    """Cleat workflow helper for making durable LLM calls.

    Usage inside a Cleat workflow::

        @durable_entry(name="my_agent")
        def run(h: HostCalls, query: str) -> str:
            llm = CleatLLM(h)
            response = llm.chat([
                {"role": "user", "content": query},
            ])
            return response["content"]
    """

    def __init__(
        self,
        h: Any,
        service_name: str = "llm",
        default_config: Optional[Dict[str, Any]] = None,
    ):
        self._h = h
        self._service_name = service_name
        self._default_config = default_config or {}

    def chat(
        self,
        messages: List[Dict[str, Any]],
        config: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        """Make a durable LLM chat call.

        The call is recorded in Cleat's event log. On replay, the cached
        response is returned without re-invoking the LLM.

        Args:
            messages: List of message dicts with ``role`` and ``content``.
            config: Optional overrides for model, temperature, etc.

        Returns:
            LLM response dict with ``content``, ``tool_calls``, etc.
        """
        merged_config = dict(self._default_config)
        if config:
            merged_config.update(config)

        # Serialize messages for transport
        safe_messages = []
        for msg in messages:
            safe_msg = {
                "role": msg.get("role", "user"),
                "content": str(msg.get("content", "")),
            }
            if msg.get("tool_calls"):
                safe_msg["tool_calls"] = msg["tool_calls"]
            if msg.get("tool_call_id"):
                safe_msg["tool_call_id"] = msg["tool_call_id"]
            safe_messages.append(safe_msg)

        return self._h.durable_call(
            self._service_name,
            "chat",
            {"messages": safe_messages, "config": merged_config},
        )

    def with_tools(
        self,
        messages: List[Dict[str, Any]],
        tools: List[Dict[str, Any]],
        config: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        """Make a durable LLM call with tool definitions."""
        cfg = dict(config or {})
        cfg["tools"] = tools
        return self.chat(messages, cfg)


# ---------------------------------------------------------------------------
# Integration with LangGraph Bridge
# ---------------------------------------------------------------------------


class LangGraphWithLLM:
    """Extended LangGraph bridge with LLM support.

    This combines the LangGraph bridge's graph execution with per-LLM-call
    durability. Each LLM call made by a graph node is individually recorded
    in Cleat's event log.

    Usage on the host side::

        runtime = LangGraphWithLLM()
        runtime.register_llm_client("anthropic", anthropic_client)
        runtime.register_graph("react-agent", make_agent_graph)

    This enables:
    - Per-node durability (node execution)
    - Per-LLM-call durability (individual LLM API calls within nodes)
    - Deterministic replay at both levels
    """

    def __init__(self) -> None:
        from cleat_langgraph import LangGraphRuntime
        self._langgraph = LangGraphRuntime()
        self._llm = LLMHostService()
        self._node_to_llm_calls: Dict[str, int] = {}

    def register_llm_client(self, provider: str, client: Any) -> None:
        self._llm.register_client(provider, client)

    def register_graph(self, name: str, builder: Any) -> None:
        self._langgraph.register_graph(name, builder)

    def register_entrypoint(
        self, name: str, entrypoint_fn: Any, tasks: Any = None
    ) -> None:
        self._langgraph.register_entrypoint(name, entrypoint_fn, tasks)

    def handle_request(self, operation: str, request: Dict[str, Any]) -> Any:
        """Handle both LangGraph and LLM operations."""
        if operation in ("chat", "llm_chat"):
            return self._llm.handle_chat(request)
        return self._langgraph.handle_request(operation, request)


# ---------------------------------------------------------------------------
# Proposed API for the Cleat LLM Plugin
# ---------------------------------------------------------------------------

"""
The ideal Cleat LLM Plugin would provide:

1. ``@llm_call`` decorator
   Wraps a function as a durable LLM call. Records the prompt and response
   in the event log. On replay, returns the cached response.

   ::

        @durable_entry(name="research_agent")
        def research(h, topic):
            llm = CleatLLM(h)
            # Each chat() call is individually durable
            outline = llm.chat([{"role": "user", "content": f"Outline {topic}"}])
            section = llm.chat([{"role": "user", "content": f"Write {topic}"}])
            return outline["content"] + section["content"]

2. Streaming support
   LLM streaming tokens (SSE) conflict with Cleat's replay model.
   Solution: buffer the full response during first execution, then replay
   the buffered response. Streaming only works on the first pass, not replay.

3. Tool call durability
   Each LLM tool call should also be durable. The tool call invocation
   is recorded, and on replay the cached result is returned.

4. Cost tracking
   Record token usage per LLM call in Cleat state for observability
   and cost attribution.

5. Model fallback
   If an LLM call fails with a rate limit or server error, the host
   service can retry with a different model (transparent to the workflow).
"""
