# Changelog

> **UPGRADE NOTES** — Breaking changes are called out at the top of each
> release section. Read them before upgrading between versions.

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Engine test coverage for WASM backends (wasmtime, wazero) exceeding 40%
- Integration tests for MySQL and MSSQL store backends
- Unit tests for SignalWorkflow, DurableScheduleInvoke, and RegisterUpdateHandler
- WasmDiskCache and in-memory WASM cache unit tests
- CGO dispatch layer unit tests for component_cgo.go
- Engine streaming, deferral, dispatch, and callbacks unit tests
- Mock-DB coverage for WorkflowLoader DB methods
- Multi-backend coverage for MySQL and MSSQL store methods
- Concurrency key and shard error-path tests for ShardedStore
- WASM lock, memory, and scan unit tests
- Plugin loader, migration, events, credentials, and encryption tests
- Engine/app.go comprehensive test suite (22 functions, 100% coverage)
- QueryBuilder and Dialect SQL helper unit tests
- Regression tests for critical-path PostgresStore methods

### Fixed
- Multi-database CI failures in MySQL and MSSQL integration tests
- Tenant isolation: restore tenant_id filter on GetWorkflowByID
- JSONB handling in PostgreSQL store
- All 54 MySQL integration tests now pass
- MSSQL test container configuration to avoid MCR pull block
- Error message quality improvements for CLI, worker, and engine
- Race condition fixes for concurrent workflow execution
- Wasmtime memory buffer and signal handling fixes
- Plugin test failures in manifest and eventtriggers
- Auth middleware and tenant store test repairs
- Project root detection in engine tests
- ABIVersion type comparison in mssql_store_test.go
- Expand fake SQL driver coverage for admin-prefixed queries
- Corrected CompleteWorkflow status assertion from "completed" to "done"
- MySQL test schema TEXT to VARCHAR for assigned_to column

### Changed
- Project renamed from "durable" to "cleat" across the codebase

## [0.1.0] - 2026-05-13

### Added
- Durable execution engine with deterministic replay model
- Multi-database support: PostgreSQL, MySQL, SQL Server (MSSQL)
- WASM compilation pipeline (Go to wasip1, TinyGo support)
- 22 built-in plugins (blobstore, event-triggers, feature-flags, kafka-connect,
  llm, notifications, pagerduty-alert, slack-notify, webhook-ingest, and more)
- 5 language SDKs: Go, Rust, Python, Java, AssemblyScript
- Svelte 5 admin dashboard with embedded web UI
- CLI toolchain: build, vet, deploy, versions, schedule
- Prometheus metrics endpoint and OpenTelemetry tracing support
- wazero WASM runtime (pure Go, no CGo required)
- PostgreSQL-backed work queue, timer service, and blob store
- Deterministic replay from event history (Temporal-style model)
- Saga pattern with compensating transactions
- Durable promises for cross-workflow coordination
- Virtual Object (entity workflow) pattern for stateful actors
- Continue-as-new for workflow history compaction
- Heartbeat support for long-running operations
- Server-side retry with configurable backoff policy
- Signal patterns (fire-and-forget, request-response, polling)
- Update handler pattern (bi-directional RPC with validation)
- Query handlers for read-only workflow state inspection
- Sharded store for horizontal scalability
- Tenant isolation with schema-per-tenant support
- Workflow versioning with minimum version support

[Unreleased]: https://github.com/cleat-team/cleat/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/cleat-team/cleat/releases/tag/v0.1.0
