# Cleat User Guide

> **Durable execution on YOUR PostgreSQL — write in Go, compile to WASM, deploy via INSERT.**

This guide takes you from your first workflow through production deployment. Each
tutorial is designed to be followed without reading source code. All commands
and code examples are pulled from the project's test suites and examples
directory.

## What you need

To use cleat, you need exactly one piece of infrastructure:

- **A PostgreSQL 14+ connection string** — that's it. Cleat connects to your
  existing database as a client, the same way your application does. It does not
  provision, own, or manage your database.

If you already run PostgreSQL in production, you can add cleat to your existing
cluster with zero new stateful services. If you need a local PostgreSQL for
development:

```bash
docker run -d --name cleat-pg -e POSTGRES_PASSWORD=cleat -p 5432:5432 postgres:16
```

## Learning path

### Beginner

| Tutorial | Description | Time |
|----------|-------------|------|
| [Getting started](getting-started.md) | Install cleat, start PostgreSQL, run your first "hello world" workflow | 15 minutes |
| [Your first workflow](your-first-workflow.md) | End-to-end order processing workflow with HostCalls, DurableCall, error handling | 30 minutes |

Start here if you are new to cleat. The getting-started guide requires only Go
and Docker. No prior durable workflow experience is needed.

### Intermediate

| Guide | Description | Time |
|-------|-------------|------|
| [Common patterns](common-patterns.md) | Saga, fan-out, cron, signals, child workflows, heartbeating, polling, retry policies | 45 minutes |

Once you have a working workflow, this guide shows how to compose them into
reliable distributed applications.

### Advanced

| Guide | Description | Time |
|-------|-------------|------|
| [Deploying to production](deploying-to-production.md) | Configuration, monitoring, backups, scaling, health checks, graceful shutdown | 20 minutes |
| [Zero-downtime deployment](zero-downtime-deploy.md) | Blue/green worker pool replacement without interrupting running workflows | 15 minutes |
| [Upgrading cleat](upgrading.md) | Worker binary upgrades, schema migrations, workflow rollback, PostgreSQL upgrades | 15 minutes |
| [Disaster recovery](disaster-recovery.md) | Recovery from database restore, reaper reclaim, RPO/RTO, cross-region failover | 15 minutes |
| [Migrating from Temporal](migration-from-temporal.md) | Key differences and migration path for Temporal users | 15 minutes |

For teams running cleat in production or migrating from another system.

## Reference documentation

- **CLI reference** -- `cleat build`, `cleat vet`, `cleat deploy`, `cleat schedule` --
  see the [README](../README.md#cli-reference)
- **Worker flags** -- `cleat-worker --help` for all runtime options (including `--schema` to isolate worker pools by schema, and `--peer-schemas` to cooperate across schemas)
- **Database schema** -- `schema.sql` and [README table docs](../README.md#tables)
- **Go SDK docs** -- `go doc github.com/rcownie/cleat/cleat`
- **API reference** -- REST API at `/api/*` when `--api-addr` is set

## Prerequisites

Before starting, make sure you have:

- **Go 1.26+** with `GOOS=wasip1 GOARCH=wasm` target support
- **PostgreSQL 14+** (Docker is fine for development)
- **Docker Compose** (recommended for local PostgreSQL)
- **curl** (for testing workflow triggers)

Optional tools that enhance the experience:

- **TinyGo** -- produces smaller WASM binaries (`cleat build --target tinygo`)
- **Rust toolchain** -- for writing workflows in Rust (`cleat build --target rust`)
- **Node.js** -- needed only if building the Svelte web UI from source

## Quick links

- [GitHub repository](https://github.com/rcownie/cleat)
- [Issue tracker](https://github.com/rcownie/cleat/issues)
- [Discord](https://discord.gg/cleat)
