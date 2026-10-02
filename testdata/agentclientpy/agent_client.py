"""The Python half of cleat#1983's acceptance test.

A Python workflow that runs the shipped agent as a CHILD, handed the same agent
config the Go client is handed. Before cleat#1983 this file would have carried
its own hand-written copy of the ReAct loop; now it is four lines of dispatch.

THE CONFIG ARRIVES AS THE WORKFLOW INPUT, and that is the point of the
dataclass below. A copy of the config per language would be two configs that
agree on the day they are written, so the test embeds the SAME JSON text it
gives the Go client.

WHY THE INPUT IS WRAPPED IN {"config": ...} AND THE GO CLIENT'S IS NOT. Not a
choice: the two SDKs hand a workflow its parameters differently. A Go entry
point with a single string parameter receives the whole input JSON verbatim
(wasm/exports.go special-cases it), so the Go client's input IS the config. A
Python entry point binds each parameter BY NAME from the input object, so the
config has to sit under a key. Only the client's own envelope differs -- the
agent config inside it is byte-identical, and the test builds it that way.

WHY AgentInput MIRRORS AgentConfig RATHER THAN BEING IT. AgentConfig has no
``message``: ``run_agent`` takes the message as its own argument, so a config
dataclass carrying the message would have two places to look for it. The
mirror is the Go client's shape -- ``agentworkflow.Input``, which does carry
``message`` -- and this file converts one to the other at the boundary, which
is where a client should convert anything.

Build (the test does this):

    cleat build --target python --entry agent_client.py:run_agent_client \
      -o /tmp/out testdata/agentclientpy/agent_client.py
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from cleat_sdk import HostCalls, cleat_entry
from cleat_sdk.agent import AgentConfig, Tool, run_agent


@dataclass
class AgentInput:
    """The agent's configuration, as it arrives in the run's input.

    Field names are the JSON keys of the config, and they match
    ``cleat/agentworkflow``'s Input exactly -- that is what lets one JSON
    document be both clients' configuration.
    """

    message: str
    tools: list[Tool] = field(default_factory=list)
    system_prompt: str = ""
    provider: str = ""
    model: str = ""
    max_steps: int = 0
    temperature: float = 0.0


@cleat_entry("run_agent_client")
def run_agent_client(h: HostCalls, config: AgentInput) -> str:
    """Start the agent child and await it, then return its result."""
    result = run_agent(
        h,
        AgentConfig(
            tools=config.tools,
            system_prompt=config.system_prompt,
            provider=config.provider,
            model=config.model,
            max_steps=config.max_steps,
            temperature=config.temperature,
        ),
        config.message,
    )
    return json.dumps(result)
