# cleat

[![CI](https://github.com/cleat-team/cleat/actions/workflows/ci.yml/badge.svg)](https://github.com/cleat-team/cleat/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev/doc/devel/release)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://github.com/cleat-team/cleat/blob/main/LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/cleat-team/cleat)](https://goreportcard.com/report/github.com/cleat-team/cleat)
[![Discord](https://img.shields.io/badge/Discord-join%20chat-5865F2?logo=discord&logoColor=white)](https://discord.gg/cleat)
[![Go Reference](https://pkg.go.dev/badge/github.com/cleat-team/cleat.svg)](https://pkg.go.dev/github.com/cleat-team/cleat)

> **Durable workflow engine -- runs on PostgreSQL, MySQL, or SQL Server. Write in Go, compile to WASM, deploy via INSERT.**

```bash
go install github.com/cleat-team/cleat/cmd/cleat@latest
cleat dev --entry-point PlaceOrder \
    --input '{"userID":"u1","cart":[{"sku":"widget","quantity":2}]}' \
    ./testdata/basic/
```

<!-- Corrected 2026-08-09: this previously read
       docker compose up -d postgres
       cleat dev start
     Neither line worked. `docker-compose.yml` doesn't exist at the repo root
     (only docker-compose.{partner,dev,cluster,monitoring}.yml do), and there
     is no `dev start` subcommand -- `cleat dev` parsed "start" as a Go
     package path and exited 1 with "package start is not in std". `cleat
     dev` also runs entirely locally, without a database, so the compose
     line was never needed for it in the first place; that's the point of
     `dev` mode -- see the full Quick Start below for the build/deploy/worker
     path that does need Postgres. -->

## What is Cleat

Cleat is a durable workflow engine that turns your existing PostgreSQL, MySQL, or
SQL Server database into an orchestration backend. Workflows are written in Go (or
Rust), compiled to WebAssembly, and stored directly in the database. A stateless
Go worker daemon polls the database, claims ready workflows, and drives execution
with deterministic replay, checkpointing, and failover -- no new infrastructure required.

Cleat ships with an embedded Svelte web UI for monitoring, a CLI for build/deploy/
management, and a WASM-free test framework for fast unit tests. It is self-hosted,
Apache 2.0 licensed, and designed for teams that already run a supported relational
database.

## Quick Start

```bash
# 0. Verify your toolchain (one command)
make setup

# 1. Install the CLI
go install github.com/cleat-team/cleat/cmd/cleat@latest

# 2. Start Postgres and apply the schema. `cleat-worker` (step 5) also
#    applies migrations/postgres/*.sql automatically on boot, but `cleat
#    deploy` (step 4) does not, and deploy runs first in this walkthrough --
#    so the schema has to exist before that. See
#    docs/explanation/postgresql-schema.md for the full procedure.
docker compose -f docker-compose.partner.yml up -d postgres
for f in migrations/postgres/*.sql; do
    psql "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" -f "$f"
done

# 3. Compile a workflow package to WASM
cleat build -o ./out ./testdata/basic/
# Wrote ./out/cancel_order.wasm -- cleat build bundles every entry point
# in the package (PlaceOrder, CancelOrder, LongRunning) into one module,
# named after the first entry point it found. All three are still callable
# from that one file; --entry-point at trigger time (step 6) picks one.

# 4. Deploy to your database
cleat deploy --db "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable" \
    --name place_order ./out/cancel_order.wasm

# 5. Start the worker daemon
cleat-worker --db "postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable"

# 6. Trigger a workflow (via REST API) -- POST .../<name>/start, not POST
#    .../workflows (that route is GET-only and returns 405 on POST)
curl -X POST http://localhost:8080/api/workflows/place_order/start \
    -d '{"input":{"userID":"u1","cart":[{"sku":"widget","quantity":2}]},"entry_point":"PlaceOrder"}'
```

<!-- Corrected 2026-08-09: `cleat build ./testdata/basic/` was previously
     followed by `cleat deploy ... ./out/place_order.wasm`, a file `cleat
     build` never writes (it writes cancel_order.wasm -- the first entry
     point found, all three bundled inside). Step 6 previously POSTed to
     `/api/workflows` with a `def_name` body, which routes to the GET-only
     list handler and returns 405; the real route is
     `/api/workflows/<name>/start`, and the input field name for PlaceOrder
     is `userID` (camelCase, matching the Go parameter name), not
     `user_id`. Verified by building testdata/basic and reading
     cmd/cleat-worker/server.go's route table and cmd/cleat/main.go's
     runDeploy. -->

See the [Quick Start Tutorial](docs/tutorials/quick-start.md) for a complete
walkthrough with a real-world example.

## Key Features

- **Durable execution** -- deterministic replay via event history; workflows survive
  worker crashes, restarts, and network partitions.
- **Multi-DB backends** -- PostgreSQL 16+, MySQL 8.0+, SQL Server 2022+, each with an
  independent implementation of the full workflow store. Database-enforced tenant
  isolation (row-level security) exists on PostgreSQL and SQL Server -- FORCEd RLS
  policies on PostgreSQL, a native `SECURITY POLICY`/`FILTER PREDICATE` on SQL Server.
  MySQL has no row-level security feature at all, so it is documented single-tenant
  only rather than emulating isolation the database can't back up (see
  `docs/reference/multi-tenancy.md`).
  **All of the above is engine support, not CLI support**: the `cleat` CLI (`deploy`,
  `versions`, `rollback`, `schedule`, `lock`, `plugin`) only connects to PostgreSQL
  today, and refuses a MySQL or SQL Server connection string with an explicit error
  rather than a confusing driver failure. `cmd/deploy-workflow --driver mysql|mssql`
  is the one multi-dialect entry point, and it covers `deploy` only (see `tiers.yaml`).
- **Plugin system** -- extensible via LLM, Slack, webhooks, and custom plugins;
  plugins run in-process with lifecycle hooks.
- **WASM workflows** -- write in Go, Rust, Python, Java, or AssemblyScript, compile to
  WebAssembly. wasmtime is the backend of record (CPU/wall-clock/memory limits via
  epoch interruption, fuel, and store limits); wazero is a pure-Go, CGO-less fallback
  with no compute-bound fencing -- see `docs/explanation/security-model.md`.
- **Signals and human-in-the-loop** -- `AwaitSignals` pauses workflows for external
  input; signals are recorded in the event history for deterministic replay.
- **Saga / compensating transactions** -- structured rollback with `DurableDefer`,
  `DurableDeferFunc`, and `cleat.NewSaga()`.
- **Horizontal scaling** -- stateless workers, `SELECT ... FOR UPDATE SKIP LOCKED`
  claim model, scale out by adding worker processes.
- **CLI toolchain** -- `cleat build`, `vet`, `deploy`, `versions`, `rollback`, and
  cron `schedule` management.
- **Observability** -- embedded Svelte web UI, Prometheus metrics, structured logging.

## Documentation

| Section | Description |
|---------|-------------|
| [Tutorials](docs/tutorials/) | Step-by-step walkthroughs: quick start, first workflow, signals |
| [How-To Guides](docs/how-to/) | Practical guides: plugins, testing, deployment |
| [Reference](docs/reference/) | CLI reference, SDK API, worker configuration, [workflow lifecycle](docs/reference/workflow-lifecycle.md) |
| [Explanation](docs/explanation/) | Architecture, execution model, security, WASM compilation |
| [Operations](docs/operations/) | Production deployment, disaster recovery, upgrading |
| [Migration Guides](docs/migration/) | Migrating from Temporal, DBOS, Restate |
| [Contributor Guide](CONTRIBUTING.md) | Setting up a dev environment, coding standards, PR process |

Start at the [Documentation Home](docs/index.md) to find the right page for your goal.

## Installation

```bash
# Install all CLI tools
go install github.com/cleat-team/cleat/cmd/cleat@latest
go install github.com/cleat-team/cleat/cmd/cleat-worker@latest
go install github.com/cleat-team/cleat/cmd/cleat-gen@latest
```

Or build from source: `git clone https://github.com/cleat-team/cleat.git && cd cleat && go install ./cmd/...`

### macOS: use Homebrew or `go install`, not the release archives

The release archives contain **no macOS `cleat-worker`**. The worker needs CGO
for the wasmtime runtime — wasmtime is the only WASM backend cleat has, and a
CGO-less build exits 1 at startup — and the release job runs on Linux, which
cannot link a CGO macOS binary. `cleat` and `cleat-gen` are unaffected and ship
for macOS as usual.

Either of the two commands above works, or build the Homebrew formula from
source:

```bash
brew install --build-from-source packaging/homebrew/Formula/cleat.rb
```

It compiles the worker with CGO on your machine, where the Xcode Command Line
Tools that Homebrew already requires guarantee a C toolchain. Its test block
runs `cleat-worker --verify-backend`, so a worker that cannot construct the
backend fails the install rather than being discovered later.

There is no published tap yet, so the formula is installed from a path in a
clone rather than with `brew tap`.

To check any `cleat-worker`, however you installed it:

```bash
cleat-worker --verify-backend      # exits 0 only if the wasmtime backend is live
```

## License

Apache 2.0. See [LICENSE](LICENSE) for details.
