# cleat

[![CI](https://github.com/cleat-team/cleat/actions/workflows/ci.yml/badge.svg)](https://github.com/cleat-team/cleat/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/doc/devel/release)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://github.com/cleat-team/cleat/blob/main/LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/cleat-team/cleat)](https://goreportcard.com/report/github.com/cleat-team/cleat)
[![Discord](https://img.shields.io/badge/Discord-join%20chat-5865F2?logo=discord&logoColor=white)](https://discord.gg/cleat)
[![Go Reference](https://pkg.go.dev/badge/github.com/cleat-team/cleat.svg)](https://pkg.go.dev/github.com/cleat-team/cleat)

> **Durable workflow engine -- runs on PostgreSQL, MySQL, or SQL Server. Write in Go, compile to WASM, deploy via INSERT.**

```bash
go install github.com/cleat-team/cleat/cmd/cleat@latest
docker compose up -d postgres
cleat dev start
```

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
# 1. Install the CLI
go install github.com/cleat-team/cleat/cmd/cleat@latest

# 2. Compile a workflow package to WASM
cleat build -o ./out ./testdata/basic/

# 3. Deploy to your database
cleat deploy --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --name place_order ./out/place_order.wasm

# 4. Start the worker daemon
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable"

# 5. Trigger a workflow (via REST API)
curl -X POST http://localhost:8080/api/workflows \
    -d '{"def_name":"place_order","input":"{\"user_id\":\"u1\"}"}'
```

See the [Quick Start Tutorial](docs/tutorials/quick-start.md) for a complete
walkthrough with a real-world example.

## Key Features

- **Durable execution** -- deterministic replay via event history; workflows survive
  worker crashes, restarts, and network partitions.
- **Multi-DB backends** -- PostgreSQL 14+, MySQL 8.0+, SQL Server 2017+ with full
  feature parity across all three.
- **Plugin system** -- extensible via LLM, Slack, webhooks, and custom plugins;
  plugins run in-process with lifecycle hooks.
- **WASM workflows** -- write in Go or Rust, compile to WebAssembly, zero sandbox
  overhead with the wazero runtime.
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
| [Reference](docs/reference/) | CLI reference, SDK API, worker configuration |
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

## License

Apache 2.0. See [LICENSE](LICENSE) for details.
