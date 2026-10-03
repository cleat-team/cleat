# Quick start: from zero to running workflow in 5 minutes

This guide walks you through deploying and running your first cleat workflow
end-to-end. Every command is a copy-paste snippet.

**No checkout is required.** The CLI comes from the tap, the database from plain
Docker, and the schema from migrations **embedded in the `cleat-worker` binary**
-- so nothing below reads `migrations/` or any other repo-relative path. A
checkout is still needed if you would rather build the CLI from this tree than
install the latest release, and step 1 gives both.

> Corrected 2026-10-03. This previously opened by requiring a checkout, on the
> grounds that steps 2 and 3 read `docker-compose.partner.yml` and `migrations/`
> and that *"there is no packaged distribution of the migration files"*.
> **The migration half was false, and the binary says so itself.**
> `migrationsOverrideFS` (`cmd/cleat-worker/main.go`) returns `migrations.FS`,
> the embedded tree, and `--migrations-dir`'s own help calls the embedded copy
> *"what makes migrating work regardless of the worker's own working
> directory"*. Only the compose file was ever repo-relative, and step 2 no
> longer uses it (cleat#2995).
>
> Measured 2026-10-03 on an **empty** database, from a built `cleat-worker` with
> no `--migrations-dir` and no checkout on the path: `cleat-worker
> --migrate-only` applied every migration from the binary and left **122
> tables**, `workflow_defs` among them.

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

> Corrected 2026-08-09: this previously said `cleat version`. There is no
> `version` subcommand -- `cleat`'s top-level usage line lists
> `build|vet|deploy|versions|rollback|dev|schedule|run|dag|plugin|lock|init`,
> and none of them print a CLI version. (There is a `versions` subcommand,
> but it lists deployed *workflow* versions from the database, not the CLI's
> own version, and needs `--db` to do anything.) `cleat build --help` is
> used here instead because it needs no database and exits 0.

> Corrected 2026-09-27: this step previously said `go install
> github.com/cleat-team/cleat/cmd/cleat@latest`. That resolves to a CLI from a
> **different lineage** than this checkout -- the published v0.2.0 predates
> nothing here, its `deploy` has no `--db` flag, so step 6 fails with a usage
> error rather than deploying -- and it installs no `cleat-worker`, which step
> 7 needs. Build both from the tree you are working in. Measured on a30e5769;
> see cleat#2473.

> Also worth knowing: `go build -o cleat ./cmd/cleat` (without the `bin/`)
> does not work in this repository. `cleat/` is a directory here -- the Go SDK
> -- and `go build -o <existing-directory>` writes the binary *inside* it, so
> that command exits 0, leaves `./cleat` a directory, and buries a 59 MB binary
> in the SDK source tree. `go build ./cmd/cleat` with no `-o` refuses outright
> with `build output "cleat" already exists and is a directory`.

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

> These are the same settings `docker-compose.partner.yml` uses in this
> repository -- `postgres:16`, user and password `postgres`, database `cleat`
> -- so if you *are* in a checkout, `docker compose -f
> docker-compose.partner.yml up -d postgres` is equivalent and the rest of this
> guide is unchanged. Corrected 2026-10-03: this step gave only the compose
> form, which is the one thing that actually required a checkout (cleat#2995).

## 3. Apply the database schema

> Added 2026-08-09. No step in this guide previously did this at all, and
> without it, `cleat deploy` (step 6) fails with `relation "workflow_defs"
> does not exist` -- `cleat deploy` queries `workflow_defs` directly and does
> not apply migrations itself. This was previously mentioned only in
> `docs/explanation/postgresql-schema.md`, unlinked from either quick start.
>
> Corrected 2026-10-03: this note used to end *"(`cleat-worker`, started later
> in step 7, does apply `migrations/postgres/*.sql` automatically on boot -- but
> by then it's too late, because deploy already ran and already failed)"*.
> **That is now false, and it was the sentence a reader could act on wrongly**
> -- someone who believed the worker migrates on boot would reasonably skip
> this step. Since cleat#2117 migration is an explicit deploy step:
> `--migrate-only` applies it and exits, and a normal start only **verifies**
> and refuses. Measured 2026-10-03 against an empty database:
>
> ```
> refusing to start: the database schema is behind this worker: the database has
> no schema_migrations table: it has never been migrated, and all 10 migration(s)
> this binary ships are pending.
> A worker does not migrate the database on start. Run the migrations as a deploy step:
>
>     cleat-worker --migrate-only --db <dsn> [--migrate-db <owner dsn>]
> ```
>
> Note what the message says in passing: *"all 10 migration(s) **this binary
> ships**"* -- the migrations travel inside the binary, which is why this step
> needs no checkout (cleat#2995).

Run it through the migration runner:

```bash
export CLEAT_OWNER_DSN="postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable"
cleat-worker --migrate-only --db "$CLEAT_OWNER_DSN"
```

> Corrected 2026-09-27: this was a `for f in migrations/postgres/*.sql; do psql
> ... -f "$f"; done` loop. That loop builds the schema but **not**
> `schema_migrations` -- only the migration *runner* writes that table -- and a
> worker started against the result refuses with:
>
> ```
> refusing to start: the database schema is behind this worker: the database has
> no schema_migrations table: it has never been migrated, and all 3 migration(s)
> this binary ships are pending.
> ```
>
> which reads as *"you skipped this step"* at the moment you did it correctly.
> Measured on a fresh database: the loop leaves **86 tables** and no
> `schema_migrations`; `--migrate-only` leaves 3 rows in it.
>
> Those two counts are from 2026-09-27 and have grown since, because a migration
> set only ever gains members. Re-measured 2026-10-03 on an empty database with a
> develop build: `--migrate-only` leaves **122 tables** and **10 rows**, and the
> refusal above quotes the same 10. Read the pair as the *shape* of the
> difference -- many tables, and the `schema_migrations` the loop never writes --
> rather than as constants.
>
> `--migrate-only` also runs as the owner. That matters for step 7: the worker
> refuses a superuser connection, and the app role it wants instead has no DDL
> rights, so it cannot migrate.

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

> Corrected 2026-08-09: this previously showed a `greet(name string)`
> function with a `//go:export greet` comment. That does not match `cleat
> init`'s "basic" template (`cmd/cleat/init.go`, `scaffoldBasic`) -- the real
> entry point is `Hello`, marked with a `// @cleatEntry(name="hello")`
> comment.

> **Corrected 2026-09-27, and this one removed a step rather than adding one.**
> The 2026-08-09 correction above also claimed `cleat init` writes no `go.mod`,
> and this section carried a two-command workaround for that. **Both are now
> false.** `cleat init` does write `go.mod` and `go.sum`, and it pins the SDK
> correctly:
>
> ```
> module my-workflow
> go 1.27.0
> require github.com/cleat-team/cleat/cleat v0.0.0-20260927035713-a30e57693613
> ```
>
> Following the old workaround is now actively harmful. `go mod init my-workflow`
> fails outright (`go.mod already exists`), and `go get
> github.com/cleat-team/cleat@latest` **succeeds while adding the wrong thing**:
> it pulls in the *root* module at `v0.2.0 // indirect`, from the other lineage
> described in step 1, into a project that never needed it. Nothing fails --
> which is why it is worth deleting rather than leaving as harmless.
>
> There is nothing to run here. `cleat init` produces a buildable project.
>
> The sample above was also stale in a way that would not compile: it called
> `h.DurableLog("hello", "greeting")`, and this SDK's `DurableLog` takes a
> single message argument. The template emits `h.DurableLog("hello: greeting")`.

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

> Corrected 2026-09-30: this previously said `hello.wasm`. The `-o` flag
> names an output *directory*, and the compiled binary inside it is named
> after `cleat.yaml`'s `name:` field when the project has one -- every
> `cleat init` scaffold does, set to the project name given in step 4 -- and
> only falls back to the entry point's *source file* when there is no
> manifest (see `wasmOutputName`, `cmd/cleat/main.go`; cleat#2692). README's
> Quick Start builds `testdata/hello/` directly, which has no `cleat.yaml`,
> so its artifact is `hello.wasm` under the fallback rule -- a different
> fixture taking a different branch of the same rule, not a different rule.
> Found and fixed building cleat#2802's CI guard, which runs this exact
> scaffold-build-deploy sequence and caught the mismatch at the deploy step
> (`no such file or directory` for the documented `hello.wasm`).

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
you see `relation "workflow_defs" does not exist`, go back to step 3.

## 7. Start the worker

The worker runs deployed workflows and exposes an HTTP API. It needs a
connection it will accept, so give the app role a password first:

```bash
psql "$CLEAT_OWNER_DSN" -c "ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'"
export CLEAT_APP_DSN="postgres://cleat_app:cleat_app@localhost:5432/cleat?sslmode=disable"
cleat-worker --db "$CLEAT_APP_DSN" --api-addr :8080
```

> Corrected 2026-09-27: this used the owner DSN above. **The worker refuses
> it**, and correctly:
>
> ```
> refusing to start: this connection is not subject to row-level security, so
> tenant isolation is not enforced:
>   - the connecting role "postgres" is a superuser, and PostgreSQL never applies
>     row-level security to a superuser
> ```
>
> The schema already creates `cleat_app` with its grants, so what is missing is
> only `LOGIN` and a password -- the one `ALTER ROLE` above. Note the role
> cannot migrate: it has no DDL rights, so `--migrate-only` (step 3) stays on
> the owner DSN.

Leave this terminal running and open a new one for the next steps.

## 8. Run the workflow

Every `/api/workflows/*` route requires an API key. Mint one for the default
tenant -- the command prints it to stderr and shows it only once:

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
- [Deploying to production](../guide/deploying-to-production.md) --
  configuration, monitoring, scaling
