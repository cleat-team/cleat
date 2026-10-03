"""run_agent -- start the shipped agent workflow as a child and await it.

The agent loop is itself a WORKFLOW (``cleat/agentworkflow`` in the Go SDK), so
this module is the whole per-language surface: the LLM turns, the tool
dispatch, the step budget and the durability all live in the workflow, and a
Python workflow gets every one of them by starting it under its name.

Before cleat#1983 Python carried its own hand-written copy of the loop, which
nothing tested. A library that runs inside the guest has to be rewritten per
language; a workflow is written once and started from anywhere.

Each LLM turn and each tool call is a durable step inside the workflow, so an
agent started this way survives a crash mid-conversation and resumes without
asking the model again for turns it already completed.

Usage::

    from cleat_sdk.agent import AgentConfig, Tool, run_agent

    result = run_agent(h, AgentConfig(tools=[
        Tool(name="lookup", kind="service", service="weather", operation="get"),
    ]), "How warm is Tokyo?")
    return result["answer"]
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

# The workflow name the agent must be DEPLOYED under, because a child is
# resolved by name against a deployed definition.
AGENT_WORKFLOW_NAME = "agent"

TOOL_KINDS = ("service", "plugin", "workflow", "approval")


@dataclass
class Tool:
    """One tool the model may call, and how to call it.

    ``name``, ``description`` and ``parameters`` are what the model sees; the
    rest is dispatch, and it is deliberately opaque to the model -- the same
    tool list reaches a durable call, a plugin or a child workflow without the
    model being told which.

    ``kind`` is one of ``service`` (``service`` + ``operation``), ``plugin``
    (``plugin`` + ``function``), ``workflow`` (``workflow``) or ``approval``
    (``plugin`` + ``function``, polled until the claim reports ``found``).
    """

    name: str
    kind: str
    description: str = ""
    parameters: dict[str, Any] | None = None
    service: str = ""
    operation: str = ""
    plugin: str = ""
    function: str = ""
    workflow: str = ""

    # An ``approval`` tool's wait. 0 takes the workflow's own default.
    poll_interval_seconds: int = 0
    max_polls: int = 0

    def to_json(self) -> dict[str, Any]:
        out: dict[str, Any] = {"name": self.name, "kind": self.kind}
        for key in ("description", "parameters", "service", "operation", "plugin", "function", "workflow"):
            value = getattr(self, key)
            if value not in ("", None):
                out[key] = value
        # Truthiness, not `not in ("", None)`: 0 is a MEANINGFUL default here
        # ("take the workflow's"), so an unset value must be absent rather than
        # sent as an explicit zero -- the same rule the optional fields above
        # follow, which 0 would otherwise break.
        for key in ("poll_interval_seconds", "max_polls"):
            value = getattr(self, key)
            if value:
                out[key] = value
        return out


@dataclass
class AgentConfig:
    """The agent's configuration. Mirrors ``cleat/agentworkflow``'s Input."""

    tools: list[Tool] = field(default_factory=list)
    system_prompt: str = ""
    provider: str = ""
    model: str = ""
    max_steps: int = 0
    temperature: float = 0.0

    # The run's spend ceiling in dollars, against the cost the llm plugin
    # reports. 0.0 (the default) means unbounded. The workflow stops before
    # starting a turn once the ceiling is REACHED, and reports
    # ``status == "budget_exceeded"`` in the result.
    budget: float = 0.0

    # Attribution, echoed back in the result and never required by the
    # workflow. A caller whose spend boundary must be named enforces that
    # itself.
    tenant_id: str = ""

    # When set, the workflow writes the finished answer to the bundled
    # blobstore plugin under this key and echoes it in the result.
    artifact_key: str = ""

    # What a failed tool -- or a tool the model invented -- does. ``""`` is the
    # workflow's default (``"inject"``: the error goes back to the model as the
    # tool's result). ``"fail"`` ends the run instead. This is a CONTRACT
    # difference, not a preference, so it is stated rather than assumed.
    tool_error_mode: str = ""


def run_agent(h: Any, config: AgentConfig, message: str) -> dict[str, Any]:
    """Start the agent workflow as a child and return its result.

    Parameters
    ----------
    h:
        HostCalls instance for the current execution context.
    config:
        What the agent is given: its tools, and the model to use.
    message:
        The user's message.

    Returns
    -------
    dict
        ``{"answer": ..., "steps": ..., "tool_calls": [...]}``.

    Raises
    ------
    RuntimeError
        If the agent workflow is not deployed under ``AGENT_WORKFLOW_NAME``.
    """
    payload: dict[str, Any] = {"message": message, "tools": [t.to_json() for t in config.tools]}
    for key in ("system_prompt", "provider", "model", "tenant_id", "artifact_key", "tool_error_mode"):
        value = getattr(config, key)
        if value:
            payload[key] = value
    if config.max_steps:
        payload["max_steps"] = config.max_steps
    if config.temperature:
        payload["temperature"] = config.temperature
    # Falsy 0.0 means unbounded, which is also the workflow's own default, so
    # an unset budget is correctly absent rather than an explicit zero.
    if config.budget:
        payload["budget"] = config.budget

    run_id = h.child_workflow(AGENT_WORKFLOW_NAME, json.dumps(payload, default=str))
    result_json = h.await_child(run_id)
    result: dict[str, Any] = json.loads(result_json)
    return result
