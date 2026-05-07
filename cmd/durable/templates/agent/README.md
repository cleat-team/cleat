# {{.ProjectName}} — Durable AI Agent

A complete AI agent powered by cleat, with durable execution and crash recovery.

## Quickstart (5 minutes)

### Prerequisites
- Go 1.22+
- Docker
- An OpenAI API key (or Anthropic, Groq, or Ollama)

### 1. Start PostgreSQL

```bash
docker-compose up -d postgres
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

### 3. Build, deploy, and run

```bash
# Build the workflow to WASM
durable build -o ./out/agent.wasm .

# Deploy to PostgreSQL
durable deploy --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
  --name agent ./out/agent.wasm

# Start the worker
docker-compose up -d worker
```

### 4. Start an agent

```bash
curl -X POST http://localhost:8080/api/workflows/agent/start \
  -H "Content-Type: application/json" \
  -d '{
    "task": "What is the square root of 144 times 7?",
    "provider": "openai",
    "model": "gpt-4o-mini"
  }'
```

### 5. Watch it work

Open http://localhost:8080 to see the cleat dashboard. Your agent appears
in the workflow list. Click it to see the event timeline, including LLM
calls with token usage and cost.

### 6. Try crash recovery

While an agent is running, kill the worker:

```bash
docker-compose kill worker
docker-compose up -d worker
```

The agent continues exactly where it left off. LLM responses are replayed
from event history — you never pay for the same API call twice.

## How it works

The agent loop in `workflow.go`:
1. Receives a task and system prompt
2. Calls the LLM via the `llm` plugin (`h.PluginCall("llm", "chat", ...)`)
3. If the LLM wants to use a tool (calculator, web search), executes it
4. Feeds the tool result back to the LLM
5. Repeats until the LLM responds with text (done) or max steps reached

Every LLM call is recorded in cleat's event history. If the worker crashes,
the agent replays from history — LLM responses are returned from cache,
tools are re-executed deterministically.

## Adding tools

Edit `tools.go` and add entries to `getTools()` and `executeTool()`.
Tools can make durable API calls via `h.DurableCall(service, operation, input)`
or call plugins via `h.PluginCall(plugin, function, input)`.

## Using vector search

The `pgvector` plugin is included. Store documents with embeddings:

```go
h.PluginCall("pgvector", "upsert", json)
```

Search for similar documents:

```go
h.PluginCall("pgvector", "search", json)
```

## Provider configuration

```json
{
  "llm": {
    "providers": {
      "openai": { "api_key": "...", "enabled": true, "default_model": "gpt-4o-mini" },
      "anthropic": { "api_key": "...", "enabled": true, "default_model": "claude-sonnet-4-6" },
      "groq": { "api_key": "...", "enabled": true, "default_model": "llama-3.3-70b" },
      "ollama": { "enabled": true, "default_model": "llama3.2" }
    }
  },
  "pgvector": {
    "embedding_provider": "openai",
    "embedding_model": "text-embedding-3-small",
    "dimensions": 1536,
    "default_collection": "default"
  }
}
```
