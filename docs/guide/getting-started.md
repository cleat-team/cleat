# Getting started with Cleat

This tutorial walks you through installing cleat, starting PostgreSQL, writing
your first workflow, building it to WASM, deploying to the database, and
triggering execution.

## Prerequisites

Before you begin, verify you have the required tools installed:

```bash
# Check Go version (1.26+ required)
go version

# Check PostgreSQL client
psql --version

# Check Docker (recommended for local PostgreSQL)
docker --version
docker compose version
```

Install any missing tools:

- **Go**: Download from https://go.dev/dl/ (1.26 or later)
- **Docker**: https://docs.docker.com/get-docker/
- **psql**: included with PostgreSQL, or install via `brew install postgresql` /
  `apt install postgresql-client`

## Step 1: Install the cleat CLI

```bash
go install github.com/rcownie/cleat/cmd/cleat@latest
go install github.com/rcownie/cleat/cmd/cleat-worker@latest
```

Verify the installation:

```bash
cleat --help
cleat-worker --help
```

> **Alternative**: Clone the repository and build from source:
>
> ```bash
> git clone https://github.com/rcownie/cleat.git
> cd cleat
> go install ./cmd/cleat
> go install ./cmd/cleat-worker
> ```

## Step 2: Start PostgreSQL

Create a `docker-compose.yml` file:

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_DB: cleat
      POSTGRES_USER: user
      POSTGRES_PASSWORD: pass
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
```

Start the database:

```bash
docker compose up -d
```

Wait for PostgreSQL to be ready, then run the schema migration:

```bash
# Clone the repository for the schema file
git clone https://github.com/rcownie/cleat.git ~/cleat
cd ~/cleat

# Apply the schema
psql "postgres://user:pass@localhost/cleat?sslmode=disable" -f schema.sql
```

The schema creates four tables:

- **workflow_defs** -- stores compiled WASM blobs, versioned by workflow name
- **workflow_instances** -- tracks individual workflow execution state
- **event_history** -- ordered list of every DurableCall, sleep, signal, and
  defer event
- **workflow_signals** -- external signals delivered to running workflows

## Step 3: Create your first workflow

Create a file called `hello.go`:

```go
package main

import (
    "fmt"
    "github.com/rcownie/cleat/cleat"
)

func HelloWorld(h cleat.HostCalls, name string) (string, error) {
    h.DurableLog(fmt.Sprintf("Hello, %s!", name))

    result, err := h.DurableCall("greeter", "Greet", name)
    if err != nil {
        return "", fmt.Errorf("greeting failed: %w", err)
    }

    h.SetQueryState("greeted", name)
    return result, nil
}
```

Key points:

- The entry point function takes `h cleat.HostCalls` as its first parameter.
  This is the interface for all external interactions (durable calls, logging,
  timers, signals).
- `DurableCall(service, operation, requestJSON)` records the request and
  response in the event history. On replay, completed calls return cached
  results.
- `DurableLog` adds an entry to the workflow event log.
- `SetQueryState` stores key-value state that can be read via the REST API
  (`GET /api/workflows/:id/query?key=greeted`).

## Step 4: Build the workflow

Compile the Go package to a WASM binary:

```bash
cleat build -o ./out ./hello.go
```

This runs the transformer pipeline:

1. **analyzer.Load** -- loads the Go package, parses the AST, identifies
   exported functions as entry points
2. **callgraph.Build** -- builds a static call graph of the target package
3. **closure.Compute** -- computes the cleat closure (functions reachable from
   entry points that make HostCalls)
4. **transform** -- rewrites source files: adds HostCalls parameters to
   functions that need them, inserts imports, generates WASM export wrappers
5. **wasm.Compile** -- generates WASM import declarations and compiles to
   `wasip1` binary

On success, you will see output like:

```
Analyzed 1 package, 1 entry points
Generated WASM binary: ./out/hello.wasm (1.2 MB)
```

If you have TinyGo installed, you can produce a smaller binary:

```bash
cleat build --target tinygo -o ./out ./hello.go
```

## Step 5: Deploy the workflow

Upload the WASM binary to PostgreSQL:

```bash
cleat deploy --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --name hello_world ./out/hello.wasm
```

You should see output like:

```
Deployed hello_world v1 to postgres://user:pass@localhost/cleat?sslmode=disable
```

Each deployment creates a new version. You can list versions:

```bash
cleat versions --db "postgres://user:pass@localhost/cleat?sslmode=disable" hello_world
```

## Step 6: Run the worker

Start the cleat worker daemon:

```bash
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --api-addr :8080
```

The worker polls PostgreSQL for runnable workflow instances, drives execution,
and serves the web UI and REST API on port 8080.

You should see log output like:

```
Starting cleat-worker (namespace: default, concurrency: 10)
HTTP API listening on :8080
Polling for work every 500ms
```

## Step 7: Trigger execution

With the worker running, trigger a workflow execution via the REST API:

```bash
curl -X POST http://localhost:8080/api/workflows \
    -H "Content-Type: application/json" \
    -d '{
        "def_name": "hello_world",
        "entry_point": "HelloWorld",
        "input": "Alice"
    }'
```

The response includes a `workflow_id`:

```json
{
    "workflow_id": "wf_abc123"
}
```

## Step 8: View the result

Check the workflow status:

```bash
curl http://localhost:8080/api/workflows/wf_abc123
```

You should see:

```json
{
    "id": "wf_abc123",
    "def_name": "hello_world",
    "status": "completed",
    "result": "\"Hello, Alice!\"",
    ...
}
```

You can also query state:

```bash
curl "http://localhost:8080/api/workflows/wf_abc123/query?key=greeted"
```

## Step 9: Explore the web UI

Open http://localhost:8080 in your browser. The embedded Svelte web UI shows:

- **Dashboard** -- overview of workflow counts by status
- **Workflow list** -- searchable table of all workflow instances
- **Workflow detail** -- event history, state, and result for a single instance
- **Schedule management** -- create and manage cron schedules

## Next steps

- [Your first workflow](your-first-workflow.md) -- build a realistic order
  processing workflow with error handling and compensation
- [Common patterns](common-patterns.md) -- Saga, fan-out, signals, child
  workflows, and more
- [CLI reference](../README.md#cli-reference) -- full documentation for all
  cleat commands
