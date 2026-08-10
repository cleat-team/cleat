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
cleat build --help
# Expected output: usage for the `build` subcommand (flags: -o, -target, ...)
```

> Corrected 2026-08-09: this previously said `cleat version`. There is no
> `version` subcommand -- `cleat`'s top-level usage line lists
> `build|vet|deploy|versions|rollback|dev|schedule|run|dag|plugin|lock|init`,
> and none of them print a CLI version. (There is a `versions` subcommand,
> but it lists deployed *workflow* versions from the database, not the CLI's
> own version, and needs `--db` to do anything.) `cleat build --help` is
> used here instead because it needs no database and exits 0.

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

## 3. Apply the database schema

> Added 2026-08-09. No step in this guide previously did this at all, and
> without it, `cleat deploy` (step 6) fails with `relation "workflow_defs"
> does not exist` -- `cleat deploy` queries `workflow_defs` directly and does
> not apply migrations itself. (`cleat-worker`, started later in step 7,
> *does* apply `migrations/postgres/*.sql` automatically on boot -- but by
> then it's too late, because deploy already ran and already failed. See
> `migration/runner.go` and `cmd/cleat-worker/main.go`.) This was previously
> mentioned only in `docs/explanation/postgresql-schema.md`, unlinked from
> either quick start.

Files are idempotent (`CREATE TABLE IF NOT EXISTS`), so re-running this is
always safe -- including after `cleat-worker` has already applied them once:

```bash
for f in migrations/postgres/*.sql; do
    psql "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" -f "$f"
done
```

See [postgresql-schema.md](../explanation/postgresql-schema.md) for what
each migration file does and why applying only `001_schema.sql` is not
enough.

## 4. Scaffold a project

The `cleat init` command creates a workflow project:

```bash
cleat init my-workflow
cd my-workflow
```

This generates:
- `main.go` -- a simple "Hello, World" workflow
- `cleat.yaml` -- project configuration

Preview the generated workflow:

```bash
cat main.go
```

It should look similar to:

```go
package main

import "github.com/cleat-team/cleat/cleat"

// @cleatEntry(name="hello")
func Hello(h cleat.HostCalls, input string) (string, error) {
	h.DurableLog("hello", "greeting")
	return `{"greeting":"hello, world"}`, nil
}
```

> Corrected 2026-08-09: this previously showed a `greet(name string)`
> function with a `//go:export greet` comment and claimed `cleat init` also
> generates a `go.mod`. Neither matches `cleat init`'s actual "basic"
> template (`cmd/cleat/init.go`, `scaffoldBasic`) -- the real entry point is
> `Hello`, marked with a `// @cleatEntry(name="hello")` comment, and no
> `go.mod` is written. That last part is a real gap: `cleat build` in the
> next step needs one and fails immediately with "go.mod file not found"
> without it. Reported to the team that owns `cmd/cleat/` rather than fixed
> here (this stream doesn't own CLI code) -- the workaround below is
> necessary until that's fixed.

Because `cleat init` doesn't generate a `go.mod`, add one before building:

```bash
go mod init my-workflow
go get github.com/cleat-team/cleat@latest
```

## 5. Build the workflow

Compile your workflow to WebAssembly:

```bash
cleat build -o workflow.wasm .
```

Expected output (exact wording varies by version):

```
Analyzing package ...
Found 1 functions, 1 entry point(s), ... in cleat closure.
Generating WASM exports (1 entry point(s))... OK
Compiling WASM module (go/wasip1)...
Wrote workflow.wasm/hello.wasm ...
```

You should now see a `workflow.wasm` directory containing a `hello.wasm`
file (the `-o` flag names an output *directory*, and the compiled binary
inside it is named after the entry point -- see the note on this in the
top-level [README](../../README.md#quick-start)):

```bash
ls -lh workflow.wasm/
# Expected output (size will vary):
# -rwxr-xr-x ... hello.wasm
```

## 6. Deploy the workflow

Register the compiled WASM binary with the cleat runtime:

```bash
cleat deploy \
    --db "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" \
    --name my-workflow \
    workflow.wasm/hello.wasm
```

Expected output includes a line like:

```
  Workflow: hello v1 (ABI v1, min compatible: 1)
```

If you see `connection refused`, make sure Postgres is running (step 2). If
you see `relation "workflow_defs" does not exist`, go back to step 3.

## 7. Start the worker

The worker runs deployed workflows and exposes an HTTP API:

```bash
cleat-worker \
    --db "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" \
    --api-addr :8080
```

Leave this terminal running and open a new one for the next steps.

## 8. Run the workflow

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

## 9. See the result

Query the workflow's state using the ID from the previous step (replace `<id>`):

```bash
curl http://localhost:8080/api/workflows/<id>
```

Expected response includes:

```json
{
    "workflow_id": "wf_2a7b9f3e1c8d",
    "status": "completed",
    "result": "{\"greeting\":\"hello, world\"}"
}
```

## 10. What just happened?

In a few minutes you:

1. Started Postgres as the durable backend
2. Applied the database schema
3. Scaffolded a cleat workflow project
4. Built the workflow to WebAssembly
5. Deployed the WASM binary to the runtime
6. Ran a worker that listens for execution requests
7. Triggered a workflow and read its output

The worker recorded every step in Postgres. If the worker had crashed and
restarted, it would have resumed the workflow exactly where it left off --
that is the durability guarantee.

## 11. Clean up

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
