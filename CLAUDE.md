# cleat-agent2 — Durable Workflow Engine

The core cleat engine: workflow execution, WASM runtime, plugin system, worker daemon, CLI, admin dashboard.

## Repo structure

- `cmd/` — CLI entrypoints (cleat, cleatctl, cleat-worker, cleat-bench, cleat-gen,
  cleat-gen-plugin, cleat-plugin-verify, deploy-workflow, wit-rewrite)
- `engine/` — Core engine: workflow execution, host functions, DB backends (~174 files)
- `wasm/` — WASM build, module loading, and codegen
- `wasmrw/` — WASM read/write helpers (small; production code duplicates this inline)
- `plugin/` — Plugin runtime and interface
- `auth/` — Tenant and auth stores
- `pluginapi/` — Public re-exports for external plugin authors
- `internal/` — Non-public support packages (analyzer, callgraph, closure, plugingen,
  telemetry, transform)
- `cleat/` — Public Go API (cleattest, embedded, localdev, wasmtest, ai, backendkit)
- `plugins/` — 21 built-in plugins (llm, slacknotify, pagerdutyalert, scheduler, etc.)
- `web/` — Svelte 5 admin dashboard
- `crates/` — Rust SDK + Java SDK
- `python-sdk/` — Python SDK
- `packages/` — AssemblyScript SDK
- `examples/` — Example workflows
- `tests/` — Integration test suites (cluster, cross-language, integrity, plugin-harness,
  scale, soak, upgrade)
- `benchmarks/` — Go benchmarks + comparative Temporal/DBOS benchmarks

> **Note on paths in older commits and branches.** Commit `3eeb74e` (2026-06-01),
> "promote internal packages to public — engine as a library", moved `internal/host/` →
> `engine/`, `internal/wasm/` → `wasm/`, `internal/plugin/` → `plugin/`, and
> `internal/wasmrw/` → `wasmrw/`. Anything referring to those `internal/` paths predates
> that commit. Branches based before it will not merge cleanly.

## Plugin development

Fuller guidance lives outside this repo at
`cleat-internal/prompts/plugins-and-apps-guidance.md`. That checkout is not present on
every machine — if it is missing, the conventions below are sufficient to start; do not
spend turns hunting for it.

Key conventions:
- Plugin names are hyphenated: `"slack-notify"`, `"email-notify"`, `"pagerduty-alert"`
- HostCall operations are snake_case: `"send_message"`, `"trigger_incident"`
- Plugins share the main go.mod (no separate go.mod per plugin)
- Study `plugins/slacknotify/` and `plugins/scheduler/` as reference implementations
- The Plugin interface is in `plugin/plugin.go`

## Build

- Go 1.25+, module `github.com/cleat-team/cleat`
- WASM workflows are compiled with the standard Go toolchain (`--target go`, default)
- Tests use `go test`, fuzz tests, and behavioral test suites

### Two WASM backends

- **wasmtime** (`engine/backend_wasmtime.go`) — via CGo. **The primary backend.** It is the
  standard engine, and in practice substantially more reliable. Preferred automatically
  whenever CGO is available (`cmd/cleat-worker/main.go`).
- **wazero** (`engine/backend_wazero.go`) — pure Go. Used when CGO is unavailable. The worker
  logs this as "legacy wazero". It has a real bug tail — do not treat a wazero-only failure
  as evidence about the engine as a whole.

  It is **no longer the fallback for any language.** That sentence used to read "retained as
  a fallback for the languages that do not work under wasmtime"; as of 2026-08-05 that set is
  empty — `engine.WasmtimeLanguages` names all five, Python included. What wazero is *for*
  now is an open question, tracked as IMPROVEMENT-PLAN §3.30, and it matters because the
  execution fence does not fire there (§2.28): a runaway guest on wazero is not stopped.

Prefer wasmtime when reproducing or debugging anything execution-related. If you find
yourself on wazero unexpectedly, check whether CGO got disabled.

They are not equivalent: resource limits and determinism enforcement differ. When changing
execution paths, check both, but treat wasmtime as the behaviour of record.

> **Do not build or test the engine with `CGO_ENABLED=0`.** `NewWasmtimeBackend` is behind
> `//go:build cgo`, so disabling CGO does not merely skip a check — it removes the primary
> backend from the binary entirely and silently runs everything on wazero, the fallback with
> the known bug tail. An engine result obtained that way is not evidence about the engine.
>
> This note used to read: *"`go build ./...` fails with `'wasmtime.h' file not found` because
> `engine/cgo_test_helpers.go` hardcodes machine-specific paths. Use `CGO_ENABLED=0` until
> that is fixed."* That was true when written and was fixed by `c26c332` without the note
> being updated, so it went on steering engine work onto the wrong backend. Verified
> 2026-08-04 on a clean checkout: `go build ./...`, `go vet ./engine/` and `go test ./engine/`
> all pass with CGO at its default `1`, in the same 14s the CGO-less run took.
>
> If you hit a genuine toolchain failure that forces `CGO_ENABLED=0`, say so in the PR rather
> than leaving the reader to assume wasmtime was exercised.

## Project state

Two working documents at the repo root, both current as of 2026-08-02:

- `IMPROVEMENT-PLAN.md` — prioritised remediation plan. Start at Phase 0; CI is currently
  reporting green while the `engine` test package does not compile.
- `BRANCH-TRIAGE.md` — state and risk assessment of the 47 unmerged remote branches.
