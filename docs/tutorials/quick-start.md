# Quick start: from zero to running workflow in 5 minutes

This guide walks you through deploying and running your first cleat workflow
end-to-end. Every command is a copy-paste snippet.

**No checkout is required.** The CLI comes from the tap, the database from plain
Docker, and the schema from migrations **embedded in the `cleat-worker` binary**
-- so nothing below reads `migrations/` or any other repo-relative path. A
checkout is still needed if you would rather build the CLI from this tree than
install the latest release, and step 1 gives both.

---

## 1. Prerequisites

- **Go 1.27+** -- [Download](https://go.dev/dl/). Needed in step 5, which
  compiles your workflow to WebAssembly.
- **Docker** -- for running Postgres locally
- **The `cleat` CLI and `cleat-worker`** -- install both with Homebrew:

```bash
brew install cleat-team/tap/cleat
```

Installs `cleat`, `cleat-worker` and `cleat-gen` from source, and the tap is
bumped on every release (cleat#2068), so there is no version to track by hand.
macOS needs a from-source `cleat-worker` because no CGO worker is published for
it; see [Installation](../../README.md#installation).

> **Alternatively, install from a checkout of this repository.** The tree
> tracks ahead of the last release (v0.3.2 at this writing), so this is the
> right choice if you are working on cleat itself, or building against the SDK
> in the tree:
>
> ```bash
> git clone https://github.com/cleat-team/cleat && cd cleat
> go install ./cmd/cleat ./cmd/cleat-worker
> ```
>
> Both land in `$(go env GOPATH)/bin` (usually `~/go/bin`), which is already on
> your `PATH`. Installing rather than building into the tree matters here: step
> 7 asks you to open a **second terminal**, and neither an exported `PATH` nor
> a relative `./bin/...` would survive that -- and step 4 moves you into the
> project directory, where `./bin/...` does not resolve anyway.

Verify the installation:

```bash
cleat build --help
# Expected output: usage for the `build` subcommand (flags: -o, -target, ...)
```

In a checkout, use `go install` as above: `go build -o cleat ./cmd/cleat` writes the binary
*inside* the `cleat/` SDK directory instead of creating a file.

## 2. Start Postgres

Cleat uses Postgres as its durable store. Start one with Docker -- no checkout
needed:

```bash
docker run -d --name cleat-postgres \
    -e POSTGRES_USER=postgres \
    -e POSTGRES_PASSWORD=postgres \
    -e POSTGRES_DB=cleat \
    -p 5432:5432 \
    postgres:16
```

Wait a moment for it to be ready, then verify:

```bash
docker exec cleat-postgres pg_isready -U postgres
# Expected output: /var/run/postgresql:5432 - accepting connections
```

In a checkout, `docker compose -f docker-compose.partner.yml up -d postgres` starts the same
container (`postgres:16`, user and password `postgres`, database `cleat`); the rest of this
guide is unchanged.

## 3. Apply the database schema

A worker does not migrate the database when it starts: `--migrate-only` applies the
migrations and exits, and a normal start only checks them and refuses. Skipping this step
shows up as `relation "workflow_defs" does not exist` from `cleat deploy`, or as a worker that
refuses to start because the database "has no schema_migrations table". The migrations travel
inside the `cleat-worker` binary, so nothing here reads a file from a checkout.

Run it through the migration runner:

```bash
export CLEAT_OWNER_DSN="postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable"
cleat-worker --migrate-only --db "$CLEAT_OWNER_DSN"
```

`--migrate-only` runs as the owner. That matters in step 7: the worker refuses a superuser
connection, and the app role it wants instead has no DDL rights, so it cannot migrate.

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
- `go.mod` and `go.sum` -- module files that pin the SDK to the tree the CLI
  was built from

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
	h.DurableLog("hello: greeting")
	return `{"greeting":"hello, world"}`, nil
}
```

A scaffolded project builds against the latest *released* SDK, not the tree the CLI was built
from. There is nothing to run here: `cleat init` produces a buildable project.

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
Wrote workflow.wasm/my-workflow.wasm ...
```

The `-o` flag names an output *directory*. The file inside it is named after the `name:` in
`cleat.yaml`, which `cleat init` sets to the project name, so here it is `my-workflow.wasm`.

You should now see a `workflow.wasm` directory containing a `my-workflow.wasm`
file:

```bash
ls -lh workflow.wasm/
# Expected output (size will vary):
# -rwxr-xr-x ... my-workflow.wasm
```

## 6. Deploy the workflow

Register the compiled WASM binary with the cleat runtime:

```bash
cleat deploy \
    --db "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" \
    --name my-workflow \
    workflow.wasm/my-workflow.wasm
```

Expected output includes a line like:

```
  Workflow: my-workflow v1 (ABI v1, min compatible: 1)
```

If you see `connection refused`, make sure Postgres is running (step 2). If
you see `relation "workflow_defs" does not exist`, go back to step 3. If you see
`Error reading WASM file ...: no such file or directory`, the artifact is named
something other than `my-workflow.wasm` (a CLI older than 0.4.0 names it after the
entry point, `hello.wasm`): `ls workflow.wasm/` shows the name, and the deploy
command takes that one.

## 7. Start the worker

The worker runs deployed workflows and exposes an HTTP API. It needs a
connection it will accept, so give the app role a password first:

```bash
docker exec cleat-postgres psql -U postgres -d cleat \
    -c "ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'"
export CLEAT_APP_DSN="postgres://cleat_app:cleat_app@localhost:5432/cleat?sslmode=disable"
cleat-worker --db "$CLEAT_APP_DSN" --api-addr :8080
```

Use the app DSN here, not the owner DSN: the worker refuses a superuser connection
("this connection is not subject to row-level security"), because PostgreSQL never applies
row-level security to a superuser. `cleat_app` is created without `LOGIN`, which is what the
`ALTER ROLE` above fixes; and it has no DDL rights, so migrations stay on the owner DSN (step 3).

Leave this terminal running and open a new one for the next steps.

## 8. Run the workflow

Every `/api/workflows/*` route requires an API key. Mint one for the default
tenant -- the command prints it to **stdout** and shows it only once:

```bash
cleat-worker --db "$CLEAT_APP_DSN" \
    --generate-api-key 00000000-0000-0000-0000-000000000000
export CLEAT_API_KEY='cleat_sk_...'   # paste the key it printed
```

Then trigger a workflow execution via the REST API:

```bash
curl -X POST http://localhost:8080/api/workflows/my-workflow/start \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $CLEAT_API_KEY" \
    -d '{"input": "World"}'
```

Expected response (formatted for readability):

```json
{
    "id": "e39ede1b-2a7b-9f3e-1c8d-000000000000",
    "idempotent_replay": false
}
```

Copy the `id` value -- you will need it in the next step.

## 9. See the result

Query the workflow's state using the ID from the previous step (replace `<id>`):

```bash
curl -H "Authorization: Bearer $CLEAT_API_KEY" \
    http://localhost:8080/api/workflows/<id>
```

Expected response includes:

```json
{
    "id": "e39ede1b-2a7b-9f3e-1c8d-000000000000",
    "status": "done",
    "result": "{\"greeting\":\"hello, world\"}"
}
```

`status` is `workflow_instances.status` copied verbatim, so it is one of `ready`, `running`,
`done`, `failed`, `dead_lettered`, `terminated`, `cancelled` or `terminating` -- **not**
`completed`, and **not** the `running` the start call answered with. Branch on terminal-versus-not
rather than on `running`: anything that sleeps, awaits a child or waits on a signal is `ready` for
nearly all of its life. See
[Workflow lifecycle](../reference/workflow-lifecycle.md#outcomes).

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
docker rm -f cleat-postgres
```

The container has no named volume, so its data lives in the container's writable
layer and `docker rm` takes it with it. **If you used the compose file instead**
(step 2), use `docker compose -f docker-compose.partner.yml down -v` -- `-v` is
what removes that file's `pgdata` volume, which a plain `docker rm` would leave
behind.

## Next steps

- [Your first workflow: order processing](your-first-workflow.md) -- build a
  realistic workflow with multiple steps and compensation
- [Signals and the human loop](signals-and-human-loop.md) -- add human
  approval steps to your workflows
- [Common patterns](../how-to/common-patterns.md) -- Saga, fan-out, child
  workflows, retry policies
- [Deploying to production](../operations/deploying-to-production.md) --
  configuration, monitoring, scaling
