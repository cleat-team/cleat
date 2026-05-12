# cleat CLI Reference

The `cleat` CLI compiles workflow packages to WASM, deploys them, and manages
the full workflow lifecycle. Usage:

    cleat <command> [flags] <args>

Global flags:

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--db` | `CLEAT_DATABASE_URL` | `""` | PostgreSQL connection string for commands that need a database |

---

## build

Compile a workflow package to WASM. Runs the full transformer pipeline: package
loading, call graph construction, cleat closure computation, HostCalls
threading verification, WASM import/export generation, host adapter generation,
and compilation.

    cleat build [flags] <package-path>

| Flag | Default | Description |
|------|---------|-------------|
| `-o <dir>` | temp dir | Output directory for generated WASM and auxiliary files |
| `--target <name>` | `go` | Compilation target: `go`, `tinygo`, `rust`, `java`, `assemblyscript`, or `python` |
| `--entry <file:func>` | `""` | Entry point in `file.py:func_name` format (Python target only) |
| `--json` | `false` | Output diagnostics as JSON to stdout (progress goes to stderr) |

Example:

    cleat build -o ./out ./testdata/basic/
    cleat build --target rust -o ./out ./examples/rust-workflow/
    cleat build --target python --entry workflow.py:handler -o ./out ./

---

## init

Scaffold a new workflow project from a template.

    cleat init [--template <name>] <project-name>

| Flag | Default | Description |
|------|---------|-------------|
| `--template <name>` | `basic` | Project template: `basic` or `agent` |

The `basic` template creates a single `main.go` with a `Hello` entry point.
The `agent` template creates a multi-file AI agent project with
`docker-compose.yml`.

Example:

    cleat init --template agent my-agent
    cleat init my-project

---

## deploy

Upload a compiled WASM workflow to the database.

    cleat deploy [flags] <wasm-file>

| Flag | Default | Description |
|------|---------|-------------|
| `--name <name>` | derived from WASM metadata or filename | Workflow name |
| `--namespace <ns>` | `default` | Workflow namespace |
| `--task-queue <queue>` | `default` | Task queue (e.g. `default`, `gpu`, `high-memory`) |

Requires `--db` or `CLEAT_DATABASE_URL`. In dry-run mode (no DB configured),
prints what would be deployed.

Example:

    cleat deploy --db "postgres://..." --name place_order ./out/place_order.wasm

---

## dev

Run a workflow locally with live-reload for development.

    cleat dev [flags] <package-path>

| Flag | Default | Description |
|------|---------|-------------|
| `--input <json>` / `-i` | `{}` | Workflow input as JSON |
| `--entry-point <name>` / `-e` | `""` | Entry point function name |
| `--concurrency-key <key>` / `-c` | `""` | Concurrency key for virtual object scope |

When `--input` is not provided and stdin is a pipe, input is read from stdin.

Example:

    cleat dev --input '{"userID":"u1","cart":[]}' ./testdata/basic/

---

## run

Build (if needed) and execute a workflow in-process.

    cleat run [flags] <package-path>
    cleat run --wasm <file.wasm> [flags]

| Flag | Default | Description |
|------|---------|-------------|
| `--wasm <file>` | `""` | Path to pre-built `.wasm` file (skip build step) |
| `--entry-point <name>` | `place_order` | Entry point function name |
| `--input <json>` | `{}` | Workflow input as JSON |
| `--api-addr <addr>` | `:8080` | HTTP API + web UI listen address (empty to disable) |
| `--target <name>` | `go` | Build target when WASM is not pre-built: `go`, `tinygo`, or `rust` |

Example:

    cleat run --input '{}' ./testdata/basic/
    cleat run --wasm ./out/place_order.wasm --entry-point place_order

---

## vet

Validate a workflow package without compiling.

    cleat vet [flags] <package-path>

| Flag | Default | Description |
|------|---------|-------------|
| `--lang <name>` | auto-detected | Language target: `go`, `rust`, `java`, `as`, `python` |
| `--json` | `false` | Output results as JSON |
| `--ci` | `false` | Output in GitHub Actions annotation format (takes precedence over `--json`) |

Auto-detection checks for `go.mod`, `Cargo.toml`, `build.gradle.kts`,
`build.gradle`, `.py` files, or `package.json` (in that order).

Example:

    cleat vet ./testdata/basic/
    cleat vet --lang rust --ci ./my-crate/

---

## versions

List deployed versions of a workflow, latest first.

    cleat versions <workflow-name>

Requires `--db` or `CLEAT_DATABASE_URL`.

Example:

    cleat versions place_order

---

## rollback

Pin new workflow instances to a previous version.

    cleat rollback <workflow-name> <version-number>

Requires `--db` or `CLEAT_DATABASE_URL`.

Example:

    cleat rollback place_order 3

---

## schedule

CRUD management of cron-triggered workflow schedules.

    cleat schedule <add|list|delete|enable|disable> [args]

### add

    cleat schedule add <name> --cron <expr> --def <wf-name> \
        [--entry-point <name>] [--input <json>]

| Flag | Required | Description |
|------|----------|-------------|
| `--cron <expr>` | yes | 5-field cron expression |
| `--def <name>` | yes | Workflow definition name |
| `--entry-point <name>` | no | Entry point function name |
| `--input <json>` | no | Workflow input JSON (default `{}`) |

### list

    cleat schedule list

### delete

    cleat schedule delete <name>

### enable / disable

    cleat schedule enable <name>
    cleat schedule disable <name>

Example:

    cleat schedule add hourly-report --cron "0 * * * *" --def place_order
    cleat schedule list
    cleat schedule disable hourly-report

---

## dag

Inspect and generate from a DAG (directed acyclic graph) specification.

    cleat dag <validate|run|generate> [options] <spec.json>

| Subcommand | Description |
|------------|-------------|
| `validate` | Parse a DAG spec, check for errors |
| `run` | Generate and run a dev workflow from a DAG spec (`--input <json>`, `--output <file>`) |
| `generate` | Generate a compilable workflow file (`--output <file>`) |

Example:

    cleat dag validate ./dag-spec.json
    cleat dag generate --output ./workflow.go ./dag-spec.json

---

## plugin

Manage workflow plugins.

    cleat plugin <validate|install|list|update|uninstall> [flags]

| Subcommand | Description |
|------------|-------------|
| `validate` | Validate a plugin package |
| `install` | Install a plugin from a source path |
| `list` | List installed plugins |
| `update` | Update an installed plugin |
| `uninstall` | Remove a plugin |

Example:

    cleat plugin list
    cleat plugin install ./path/to/plugin

---

## lock

Resolve and pin child workflow versions for reproducible builds.

    cleat lock [--db <conn>] [--update] <package-path>

| Flag | Default | Description |
|------|---------|-------------|
| `--db <conn>` | `CLEAT_DATABASE_URL` | PostgreSQL connection string |
| `--update` | `false` | Re-resolve all child versions from database (reads existing lock file) |

Writes a `cleat.lock` file with pinned child workflow versions.

Example:

    cleat lock ./testdata/basic/
    cleat lock --update
