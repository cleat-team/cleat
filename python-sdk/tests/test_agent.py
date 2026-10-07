"""Tests for ``cleat_sdk.agent.run_agent``.

What these CANNOT cover, stated so the gap is not mistaken for coverage: the
harness constructs its own populated HostCalls, so it says nothing about
whether a compiled module imports what the agent workflow needs. That is the
workflow's own ``//cleat:require`` directive, guarded at the build level in the
Go repository (wasm + testdata/agentguest).
"""

import json

from cleat_sdk import AGENT_WORKFLOW_NAME, AgentConfig, Tool, run_agent
from cleat_sdk.test_harness import CleatTestHarness


def _child_call(h: CleatTestHarness):
    """The single workflow-kind call the harness recorded, or None."""
    calls = [c for c in h.call_history if c.service == "workflow"]
    return calls[0] if calls else None


def test_run_agent_starts_the_shipped_agent_and_returns_its_result():
    h = CleatTestHarness()
    h.register_child_stub(AGENT_WORKFLOW_NAME, '{"answer":"21C","steps":2,"tool_calls":[]}')

    res = run_agent(
        h,
        AgentConfig(tools=[Tool(name="t", kind="service", service="weather", operation="get")]),
        "how warm is Tokyo?",
    )

    assert res["answer"] == "21C"
    # Asserted on the RECORDED call, not on the result: a wrapper that started
    # nothing would still hand back whatever the stub returned.
    call = _child_call(h)
    assert call is not None, "no child workflow was started"
    assert call.operation == AGENT_WORKFLOW_NAME


def test_run_agent_sends_the_message_and_tools_to_the_child():
    h = CleatTestHarness()
    h.register_child_stub(AGENT_WORKFLOW_NAME, '{"answer":"ok","steps":1}')

    run_agent(
        h,
        AgentConfig(
            system_prompt="be terse",
            provider="openai",
            model="gpt-4o-mini",
            max_steps=3,
            budget=1.5,
            tenant_id="acme",
            artifact_key="report",
            tool_error_mode="fail",
            tools=[Tool(name="lookup", kind="workflow", workflow="summarise", description="d")],
        ),
        "go",
    )

    body = json.loads(_child_call(h).request)
    assert body["message"] == "go"
    assert body["system_prompt"] == "be terse"
    assert body["max_steps"] == 3
    assert body["budget"] == 1.5
    assert body["tenant_id"] == "acme"
    assert body["artifact_key"] == "report"
    assert body["tool_error_mode"] == "fail"
    assert body["tools"] == [
        {"name": "lookup", "kind": "workflow", "workflow": "summarise", "description": "d"}
    ]


def test_run_agent_omits_unset_optional_fields():
    """The workflow applies its own defaults, so a field left unset must not be
    sent as a zero -- an explicit ``max_steps: 0`` would read as a budget."""
    h = CleatTestHarness()
    h.register_child_stub(AGENT_WORKFLOW_NAME, '{"answer":"ok","steps":1}')

    run_agent(h, AgentConfig(), "hi")

    body = json.loads(_child_call(h).request)
    for absent in (
        "system_prompt",
        "provider",
        "model",
        "max_steps",
        "temperature",
        "budget",
        "tenant_id",
        "artifact_key",
        "tool_error_mode",
    ):
        assert absent not in body, f"{absent} was sent despite being unset"
    assert body["tools"] == []


def test_an_approval_tool_sends_its_poll_settings():
    """An ``approval`` tool's wait belongs to the tool, not the config.

    The two settings are gated on TRUTHINESS rather than the
    ``not in ("", None)`` the string fields use, because for these 0 IS the
    "take the workflow's default" value rather than an absence. **The gate is
    for consistency with this payload's own rule, not to prevent a behaviour
    change**: the workflow applies the default for 0 and for an absent key
    alike (``interval <= 0``, ``maxPolls <= 0``), so sending 0 explicitly would
    be harmless. An earlier version of this docstring said otherwise.
    """
    h = CleatTestHarness()
    h.register_child_stub(AGENT_WORKFLOW_NAME, '{"answer":"ok","steps":1}')

    run_agent(
        h,
        AgentConfig(
            tools=[
                Tool(name="approve", kind="approval", plugin="approvals", function="poll",
                     poll_interval_seconds=5, max_polls=3),
                Tool(name="defaulted", kind="approval", plugin="approvals", function="poll"),
            ]
        ),
        "go",
    )

    body = json.loads(_child_call(h).request)
    assert body["tools"][0] == {
        "name": "approve",
        "kind": "approval",
        "plugin": "approvals",
        "function": "poll",
        "poll_interval_seconds": 5,
        "max_polls": 3,
    }
    # The second tool takes the workflow's defaults, so neither key is sent.
    assert "poll_interval_seconds" not in body["tools"][1]
    assert "max_polls" not in body["tools"][1]
