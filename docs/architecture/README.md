# Architecture Documentation

This directory contains detailed architecture documentation for the cleat
durable workflow engine.

## Documents

| Document | Description |
|----------|-------------|
| [system-overview.md](system-overview.md) | High-level system architecture, component descriptions, data flow |
| [execution-engine.md](execution-engine.md) | Claim loop, replay mechanism, checkpointing, heartbeat, reaper |
| [wasm-compilation.md](wasm-compilation.md) | Transformer pipeline (5 stages), Go/WASM compilation, host imports |
| [postgresql-schema.md](postgresql-schema.md) | Core tables, indexes, migration strategy, connection management |
| [plugin-system.md](plugin-system.md) | Plugin interface, registration, lifecycle, host functions |
| [security-model.md](security-model.md) | WASM sandbox, API key auth, secrets handling, input validation |

## Quick links

- [README.md](../../README.md) -- Project README with architecture overview and
  CLI reference.
- [schema.sql](../../schema.sql) -- Canonical PostgreSQL schema.
- [cleat-execution-design.md](../../cleat-execution-design.md) -- Full design
  document with comparisons to Temporal, Azure Durable Functions, AWS Step
  Functions, Restate, and Inngest.

## Conventions

- Architecture diagrams use ASCII art with left-to-right data flow.
- Component names are capitalized (e.g., Worker, CLI, Engine).
- All cross-references are relative links within the docs directory.
