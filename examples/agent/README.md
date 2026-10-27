# Agent — an agent as a workflow

The agent loop, shipped once and started as a child by any SDK.

```bash
cleat build -o /tmp/out ./examples/agent/
```

That produces `/tmp/out/agent.wasm`. Deploy it under the name `agent`, because a
child is resolved by name against a deployed definition:

```bash
cleat deploy --name agent /tmp/out/agent.wasm
```

## Why this is a workflow and not a library

A library that runs inside the guest has to be rewritten in every language.
Before cleat#1983 that is exactly what had happened: `cleat/ai/agent` was a
Go-only ReAct loop with no importer, and the two agent templates carried
hand-written copies of the same loop, one per language, neither tested.

The loop now lives in `cleat/agentworkflow` and this directory is the whole
deployable artifact — an entry point and nothing else. Every SDK's surface is a
single call that starts this child and awaits it:

| SDK | call |
|---|---|
| Go | `agentworkflow.RunAsChild(h, cfg, msg)` |
| Python | `cleat_sdk.agent.run_agent(h, cfg, msg)` |
| Rust | `cleat_sdk::agent::run_agent(h, cfg, msg)` |
| Java | `cleat.Agent.runAgent(host, cfg, msg)` |
| AssemblyScript | `runAgent(h, cfg, msg)` from `@cleat/sdk` |

## Every step is durable

Each LLM turn and each tool call is a recorded step, so an agent started this
way survives a crash mid-conversation and resumes without asking the model again
for turns it already completed, and without repeating a tool call whose effect
already happened. That property comes from the engine rather than from this
workflow, which is why every SDK gets it for free.

## Input and output

Input is the agent's configuration:

```json
{
  "system_prompt": "You are a helpful assistant.",
  "provider": "openai",
  "model": "gpt-4o-mini",
  "max_steps": 10,
  "message": "How warm is Tokyo?",
  "tools": [
    {"name": "lookup_weather", "kind": "service", "service": "weather", "operation": "get",
     "description": "Look up the weather",
     "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}},
    {"name": "summarise", "kind": "workflow", "workflow": "summarise"}
  ]
}
```

A `service` tool resolves at the worker via `--service-endpoints`, and a
`plugin` tool against a registered plugin; a `workflow` tool is started as a
child and awaited. **The model is not told which is which** — it sees only the
name, description and parameters.

Output is `{"answer": ..., "steps": ..., "tool_calls": [...]}`.

## Running it

```bash
cleat run --wasm /tmp/out/agent.wasm --entry-point agent \
  --input '{"message": "How warm is Tokyo?", "provider": "openai", "model": "gpt-4o-mini",
            "tools": [{"name": "lookup_weather", "kind": "service",
                       "service": "weather", "operation": "get"}]}'
```

`run_agent` from another workflow is the same thing with the config typed; see
"Agent Workflows" in [`docs/reference/sdk-api.md`](../../docs/reference/sdk-api.md).
