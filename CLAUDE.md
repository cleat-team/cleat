# cleat-agent2 — Durable Workflow Engine

The core cleat engine: workflow execution, WASM runtime, plugin system, worker daemon, CLI, admin dashboard.

## Repo structure

- `cmd/` — CLI entrypoints (cleat, cleatctl, cleat-worker, cleat-bench, cleat-gen)
- `internal/` — Core engine (host, wasm, plugin, analyzer, transform)
- `cleat/` — Public Go API (cleattest, embedded, localdev, wasmtest, ai)
- `plugins/` — 20 built-in plugins (llm, slacknotify, pagerdutyalert, scheduler, etc.)
- `web/` — Svelte 5 admin dashboard
- `crates/` — Rust SDK + Java SDK
- `python-sdk/` — Python SDK
- `packages/` — AssemblyScript SDK
- `examples/` — Example workflows
- `tests/` — Integration test suites (cluster, cross-language, integrity, scale, soak, upgrade)
- `benchmarks/` — Go benchmarks + comparative Temporal/DBOS benchmarks

## Plugin development

When building new plugins, read the full guidance at:
/localssd/rcownie/cleat-internal/prompts/plugins-and-apps-guidance.md

Key conventions:
- Plugin names are hyphenated: `"slack-notify"`, `"email-notify"`, `"pagerduty-alert"`
- HostCall operations are snake_case: `"send_message"`, `"trigger_incident"`
- Plugins share the main go.mod (no separate go.mod per plugin)
- Study `plugins/slacknotify/` and `plugins/scheduler/` as reference implementations
- The Plugin interface is in `internal/plugin/plugin.go`

## Build

- Go 1.25+, module `github.com/cleat-team/cleat`
- WASM workflows are compiled with the standard Go toolchain (`--target go`, default) or TinyGo (`--target tinygo`)
- Tests use `go test`, fuzz tests, and behavioral test suites
