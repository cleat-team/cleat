# Quick start: from zero to running workflow in 5 minutes

This guide walks you through deploying and running your first cleat workflow
end-to-end. Every command is a copy-paste snippet.

---

## 1. Prerequisites

- **Go 1.25+** -- [Download](https://go.dev/dl/)
- **Docker** -- for running Postgres locally
- **cleat CLI** -- install with one command:

```bash
go install github.com/cleat-team/cleat/cmd/cleat@latest
```

Verify the installation:

```bash
cleat version
# Expected output (version will vary):
# cleat v0.1.0
```

## 2. Start Postgres

Cleat uses Postgres as its durable store. Start a local instance with the
partner compose file:

```bash
docker compose -f docker-compose.partner.yml up -d postgres
```

Wait a moment for the container to be ready, then verify:

```bash
docker compose -f docker-compose.partner.yml ps
# Expected output:
# NAME                   IMAGE               SERVICE
# cleat-agent0-postgres-1  postgres:16        postgres   running (healthy)
```

## 3. Scaffold a project

The `cleat init` command creates a fully functional workflow project:

```bash
cleat init my-workflow
cd my-workflow
```

This generates:
- `main.go` -- a simple "Hello, World" workflow
- `cleat.yaml` -- project configuration
- `go.mod` -- Go module with cleat dependency

Preview the generated workflow:

```bash
cat main.go
```

It should look similar to:

```go
package main

import "github.com/cleat-team/cleat/cleat"

//go:export greet
func greet(h cleat.HostCalls, name string) (string, error) {
    result := "Hello, " + name + "!"
    h.SetQueryState("result", result)
    return result, nil
}
```

## 4. Build the workflow

Compile your workflow to WebAssembly:

```bash
cleat build -o workflow.wasm .
```

Expected output:

```
INFO  analyzing package  package=.
INFO  found entry point  function=greet
INFO  building wasm      target=workflow.wasm
INFO  build complete     elapsed=1.2s
```

You should now see a `workflow.wasm` file:

```bash
ls -lh workflow.wasm
# Expected output (size will vary):
# -rwxr-xr-x ... workflow.wasm
```

## 5. Deploy the workflow

Register the compiled WASM binary with the cleat runtime:

```bash
cleat deploy \
    --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
    --name my-workflow \
    workflow.wasm
```

Expected output:

```
INFO  deploying workflow  name=my-workflow
INFO  deploy complete     id=<workflow-def-id>
```

If you see `connection refused`, make sure Postgres is running (step 2).

## 6. Start the worker

The worker runs deployed workflows and exposes an HTTP API:

```bash
cleat-worker \
    --db "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable" \
    --api-addr :8080
```

Expected output:

```
INFO  starting worker     api_addr=:8080
INFO  worker ready        pid=<pid>
```

Leave this terminal running and open a new one for the next steps.

## 7. Run the workflow

Trigger a workflow execution via the REST API:

```bash
curl -X POST http://localhost:8080/api/workflows/my-workflow/start \
    -H "Content-Type: application/json" \
    -d '{"input": "World"}'
```

Expected response (formatted for readability):

```json
{
    "workflow_id": "wf_2a7b9f3e1c8d",
    "status": "running"
}
```

Copy the `workflow_id` value -- you will need it in the next step.

## 8. See the result

Query the workflow's state using the ID from the previous step (replace `<id>`):

```bash
curl http://localhost:8080/api/workflows/<id>/query?key=result
```

Expected response:

```json
{
    "key": "result",
    "value": "Hello, World!"
}
```

You can also inspect the full workflow status, including event history:

```bash
curl http://localhost:8080/api/workflows/<id>
```

Expected response:

```json
{
    "workflow_id": "wf_2a7b9f3e1c8d",
    "status": "completed",
    "result": "Hello, World!",
    "event_history": [
        {
            "type": "workflow_started",
            "timestamp": "..."
        },
        {
            "type": "set_query_state",
            "key": "result",
            "value": "Hello, World!"
        },
        {
            "type": "workflow_completed",
            "result": "Hello, World!"
        }
    ]
}
```

## 9. What just happened?

In about 30 seconds you:

1. Started Postgres as the durable backend
2. Scaffolded a cleat workflow project
3. Built the workflow to WebAssembly
4. Deployed the WASM binary to the runtime
5. Ran a worker that listens for execution requests
6. Triggered a workflow and read its output

The worker recorded every step in Postgres. If the worker had crashed and
restarted, it would have resumed the workflow exactly where it left off --
that is the durability guarantee.

## 10. Clean up

Stop the worker (Ctrl+C in its terminal), then stop and remove the database:

```bash
docker compose -f docker-compose.partner.yml down -v
```

## Next steps

- [Your first workflow: order processing](your-first-workflow.md) -- build a
  realistic workflow with multiple steps and compensation
- [Signals and the human loop](signals-and-human-loop.md) -- add human
  approval steps to your workflows
- [Common patterns](../how-to/common-patterns.md) -- Saga, fan-out, child
  workflows, retry policies
- [Deploying to production](../guide/deploying-to-production.md) --
  configuration, monitoring, scaling
