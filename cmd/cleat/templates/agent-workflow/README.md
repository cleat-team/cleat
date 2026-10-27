# {{.ProjectName}} — the agent workflow, deployable

cleat's agent loop (`cleat/agentworkflow`), shipped once in the SDK and started
as a child by any caller. This project is the whole deployable artifact: an
entry point and nothing else.

## Quickstart

### 1. Start PostgreSQL and a worker

```bash
docker-compose up -d postgres migrate app-role
```

### 2. Configure your LLM provider

Create `plugin-config.json`:

```json
{
  "llm": {
    "providers": {
      "openai": {
        "api_key": "sk-your-key-here",
        "default_model": "gpt-4o-mini",
        "enabled": true
      }
    }
  }
}
```

### 3. Start the worker

```bash
docker-compose up -d worker
```

### 4. Build and deploy

```bash
make build
make deploy
```

`make deploy` runs `cleat deploy --name agent ./out/{{.ProjectName}}.wasm` --
**not** `--name {{.ProjectName}}`, unlike every other template. The deployed
NAME is a contract: every SDK's child-starting call (`h.ChildWorkflow("agent",
...)`, Python's `run_agent`) names the child "agent" literally, regardless of
what you called this project. Deploy it under any other name and nothing can
find it.

### 5. Run it

```bash
cleat run --wasm ./out/{{.ProjectName}}.wasm --entry-point agent \
  --input '{"message": "How warm is Tokyo?", "provider": "openai", "model": "gpt-4o-mini",
            "tools": [{"name": "lookup_weather", "kind": "service",
                       "service": "weather", "operation": "get"}]}'
```

Or start it as a child from your own workflow -- that is the usual way in to
reach it, and the reason it is a deployable definition rather than a library.

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

A `service` tool resolves at the worker via `--service-endpoints`, a `plugin`
tool against a registered plugin, and a `workflow` tool is started as a child
and awaited. **The model is not told which is which** -- it sees only the
name, description and parameters.

Output is `{"answer": ..., "steps": ..., "tool_calls": [...]}`.

## Every step is durable

Each LLM turn and each tool call is a recorded step, so an agent started this
way survives a crash mid-conversation and resumes without asking the model
again for turns it already completed, and without repeating a tool call whose
effect already happened. That property comes from the engine rather than from
this workflow, which is why every caller gets it for free.

## Calling it from your own workflow

See "Agent Workflows" in
[`docs/reference/sdk-api.md`](https://github.com/cleat-team/cleat/blob/develop/docs/reference/sdk-api.md)
for the typed call each SDK exposes (`agentworkflow.RunAsChild` in Go,
`run_agent` in Python). `cleat init --template agent` scaffolds a minimal
caller.
