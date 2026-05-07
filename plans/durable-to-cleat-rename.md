# Pervasive Rename Plan: "durable" → "cleat"

May 2026

---

## Objective

Rename every user-visible and internal "durable" reference to "cleat"
across the entire codebase. No compatibility concerns — the project is
pre-public. This plan covers ~300 files across 6 languages plus config,
docs, build files, and on-disk file/directory renames.

---

## Rename Categories

The work breaks into **independent categories**. Each category can be
done in a separate commit (or separate subagent) with no ordering
constraint between categories. Within a category, order matters.

---

## Category A: Go Module Path

**Impact**: Every `.go` file that imports the module (~182 files).
Every `go.mod` and `go.sum` in the repo and sub-modules.

**What changes**:
```
github.com/rcownie/cleat          → github.com/rcownie/cleat
github.com/rcownie/cleat/durable  → github.com/rcownie/cleat/cleat
github.com/rcownie/cleat/...      → github.com/rcownie/cleat/...
```

**Files to change**:

| Scope | Count | Method |
|-------|-------|--------|
| Root `go.mod` line 1 | 1 | Edit `module github.com/rcownie/cleat` → `module github.com/rcownie/cleat` |
| `.go` files importing `github.com/rcownie/cleat/...` | ~182 | `find . -name '*.go' -exec sed -i 's|github.com/rcownie/cleat|github.com/rcownie/cleat|g'` |
| Sub-module `go.mod` files | ~8 | Manual edit each: `wasm-demo/go.mod`, `ports-round2/*/go.mod`, `benchmarks/comparative/*/temporal/go.mod` |
| `go.sum` | 1 | Delete and run `go mod tidy` |

**Post-rename verification**:
```bash
# Verify no old import paths remain
grep -r 'github.com/rcownie/cleat' --include='*.go' .
# should produce no output

# Rebuild everything
go build ./...
go vet ./...
go test -count=1 ./...
```

**Special case — `durable/` Go package directory**: After the module rename,
the directory `durable/` (which provides the `package durable` SDK) must be
renamed to match. This is a two-step:
1. Rename directory `durable/` → `cleat/`
2. Rename `durable/durabletest/` → `cleat/cleattest/`
3. Within those files, rename `package durable` → `package cleat` and
   `package durabletest` → `package cleattest`
4. Update all imports from `github.com/rcownie/cleat/durable` → `github.com/rcownie/cleat/cleat`

---

## Category B: Binary and Command Names

**What changes**:

| Old | New |
|-----|-----|
| `cmd/durable/` | `cmd/cleat/` |
| `cmd/durable-worker/` | `cmd/cleat-worker/` |
| `cmd/cleat-gen/` | `cmd/cleat-gen/` |
| `cmd/cleat-bench/` | `cmd/cleat-bench/` |
| Binary `durable` | Binary `cleat` |
| Binary `durable-worker` | Binary `cleat-worker` |
| Binary `cleat-gen` | Binary `cleat-gen` |
| Binary `cleat-bench` | Binary `cleat-bench` |

**Files to change**:

| File | What |
|------|------|
| `cmd/durable/` → `cmd/cleat/` | `git mv` directory |
| `cmd/durable-worker/` → `cmd/cleat-worker/` | `git mv` directory |
| `cmd/cleat-gen/` → `cmd/cleat-gen/` | `git mv` directory |
| `cmd/cleat-bench/` → `cmd/cleat-bench/` | `git mv` directory |
| `cmd/cleat/main.go` | Any internal references to old binary names in help text |
| `Makefile` | `go build -o cleat ./cmd/cleat`, etc. — 4 binary targets |
| `.gitignore` | `cleat`, `cleat-worker`, `cleat-gen`, `cleat-bench` |
| `.github/workflows/ci.yml` | Binary names in build job (line ~310) |
| `README.md` | `go install ./cmd/cleat`, all CLI usage examples |
| `docker-compose.cluster.yml` | Worker container args reference binary name |
| `k8s/deployment.yaml` | Container args |
| `charts/cleat/values.yaml` | Image name |
| `cmd/cleat/templates/agent/docker-compose.yml` | Image name |
| `cmd/cleat/templates/agent/durable.yaml` → `cleat.yaml` | Template file rename |

**Additional internal string changes** within renamed `cmd/` directories:
- Help text, usage strings, error messages that print binary names
- `cmd/cleat/templates/` — template files that generate docker-compose or k8s YAML referencing `durable-worker`
- `cmd/cleat/dev.go` — generates code referencing `DURABLE_DEV_URL_*` → `CLEAT_DEV_URL_*`

---

## Category C: Go SDK Package and Types

**What changes**:

After Category A renames the directory `durable/` → `cleat/`, the Go
package name and its exported types change:

| Old | New |
|-----|-----|
| `package durable` | `package cleat` |
| `durable.HostCalls` | `cleat.HostCalls` |
| `durable.NewHostCalls(...)` | `cleat.NewHostCalls(...)` |
| `durable.NewSaga()` | `cleat.NewSaga()` |
| `durable.NewTerminalError(...)` | `cleat.NewTerminalError(...)` |
| `durable.ChildResult` | `cleat.ChildResult` |
| `durable.HostCallsOptions` | `cleat.HostCallsOptions` |
| `durable.Saga` | `cleat.Saga` |
| `import "github.com/rcownie/cleat/cleat"` | (new import path) |
| `durabletest` package | `cleattest` package |

**Files affected**: Every `.go` file that references `durable.` — ~50+
files including benchmarks, examples, ported projects, tests.

**Method**: The Go SDK package rename is mostly driven by the directory
rename. After `git mv durable/ cleat/`, update all `package durable`
declarations to `package cleat`, then update all callers.

---

## Category D: WASM ABI Host Functions

**What changes**: The 15+ host functions imported by WASM modules.
Currently all use the `durable_` prefix. Change to `cleat_`.

| Old | New |
|-----|-----|
| `durable_call` | `cleat_call` |
| `durable_call_heartbeat` | `cleat_call_heartbeat` |
| `durable_call_retry` | `cleat_call_retry` |
| `durable_sleep` | `cleat_sleep` |
| `durable_now` | `cleat_now` |
| `durable_random` | `cleat_random` |
| `durable_log` | `cleat_log` |
| `durable_version` | `cleat_version` |
| `durable_min_version` | `cleat_min_version` |
| `durable_defer` | `cleat_defer` |
| `durable_continue_as_new` | `cleat_continue_as_new` |
| `durable_child_workflow` | `cleat_child_workflow` |
| `durable_await_child` | `cleat_await_child` |
| `durable_await_all_children` | `cleat_await_all_children` |
| `durable_poll_cancellation` | `cleat_poll_cancellation` |
| `durable_poll_signal` | `cleat_poll_signal` |
| `durable_await_signals` | `cleat_await_signals` |
| `durable_create_promise` | `cleat_create_promise` |
| `durable_await_promise` | `cleat_await_promise` |

**Files to change in the Go host**:

| File | What |
|------|------|
| `internal/host/imports.go` | 15+ `"env"."durable_*"` → `"env"."cleat_*"` Wazero function export names |
| `internal/wasm/usage.go` | Line 36–100: 15+ import name registrations |
| `internal/wasm/adapter.go` | ~13 error messages referencing `durable_*` by name |

**Files to change in each SDK**:

| SDK | File(s) | What |
|-----|---------|------|
| Python | `python-sdk/cleat_sdk/host_calls.py` | Method names: `durable_call` → `cleat_call`, etc. |
| AssemblyScript | `packages/cleat-as/assembly/host-calls.ts` | `@external("env", "durable_*")` declarations, `import_durable_*` → `import_cleat_*` |
| AssemblyScript | `packages/cleat-as/assembly/durable-entry.ts` | Internal references (also rename file to `cleat-entry.ts`) |
| Rust | `crates/cleat-macro/src/entry.rs` | `#[durable_entry]` → `#[cleat_entry]` |
| Rust | `crates/cleat-sdk/src/host_calls.rs` | Host function import names |
| Java | `crates/cleat-java/src/main/java/cleat/HostCalls.java` | `durableCall(...)` → `cleatCall(...)` |

**Files to change in examples/ported projects**:
- Every `.py` file using `@durable_entry` → `@cleat_entry`
- Every `.ts` file using `durableEntry()` → `cleatEntry()`
- Every `.rs` file using `#[durable_entry]` → `#[cleat_entry]`
- Every `.go` file using `durable_call` in WASM-related strings (rare)

**WIT files** (if any define WASM interfaces):
- Check `python-sdk/wit/` for `durable-*` world/interface names

---

## Category E: SDK Decorators and Entry Points

**What changes**:

| Language | Old | New |
|----------|-----|-----|
| Python decorator | `@durable_entry` | `@cleat_entry` |
| Python function | `durable_entry` | `cleat_entry` |
| AS decorator | `durableEntry()` | `cleatEntry()` |
| AS filename | `durable-entry.ts` | `cleat-entry.ts` |
| Rust macro | `#[durable_entry]` | `#[cleat_entry]` |
| Rust crate | `durable-macro` | `cleat-macro` |
| Rust crate | `durable-sdk` | `cleat-sdk` |
| Java annotation | `@DurableEntry` | `@CleatEntry` |
| Java classes | `DurableResult`, `DurableEntryProcessor`, `DurableEntryAggregator` | `CleatResult`, `CleatEntryProcessor`, `CleatEntryAggregator` |

**Python files**:
```
python-sdk/cleat_sdk/entry.py          — function name, decorator class
python-sdk/cleat_sdk/types.py          — references to @durable_entry
python-sdk/tests/test_entry.py         — imports and usage
ports-round2/cleat-*/cleat_sdk/        — vendored copies of decorator
python-sdk/examples/*.py               — example workflow files
```

**AssemblyScript files**:
```
packages/cleat-as/assembly/durable-entry.ts → cleat-entry.ts
packages/cleat-as/assembly/index.ts    — re-exports
packages/cleat-as/assembly/saga.ts     — references durableCall
```

**Rust files**:
```
crates/durable-macro/ → crates/cleat-macro/
crates/durable-sdk/ → crates/cleat-sdk/
crates/cleat-macro/Cargo.toml          — crate name
crates/cleat-sdk/Cargo.toml            — crate name
crates/cleat-macro/src/entry.rs        — #[durable_entry] → #[cleat_entry]
crates/cleat-macro/src/lib.rs          — exports
crates/cleat-sdk/src/lib.rs            — exports
crates/cleat-sdk/src/host_calls.rs     — any durable_ references
crates/cleat-sdk/src/memory.rs         — doc references
```

**Java files**:
```
crates/durable-java/ → crates/cleat-java/
crates/cleat-java/build.gradle         — group name (already "com.cleat" — check)
crates/cleat-java/src/main/java/cleat/DurableEntry.java → CleatEntry.java
crates/cleat-java/src/main/java/cleat/DurableEntryProcessor.java → CleatEntryProcessor.java
crates/cleat-java/src/main/java/cleat/DurableResult.java → CleatResult.java
crates/cleat-java/src/test/java/cleat/DurableResultTest.java → CleatResultTest.java
All Java `import cleat.Durable*` → `import cleat.Cleat*`
```

---

## Category F: Environment Variables

**What changes**:

| Old | New |
|-----|-----|
| `DURABLE_DATABASE_URL` | `CLEAT_DATABASE_URL` |
| `DURABLE_TINYGO_GOROOT` | `CLEAT_TINYGO_GOROOT` |
| `DURABLE_TEST_DB` | `CLEAT_TEST_DB` |
| `DURABLE_DEV_URL_<SERVICE>` | `CLEAT_DEV_URL_<SERVICE>` |
| `DURABLE_OTLP_INCLUDE_PAYLOADS` | `CLEAT_OTLP_INCLUDE_PAYLOADS` |
| `DURABLE_PAYMENTS_TOKEN` | `CLEAT_PAYMENTS_TOKEN` |
| `DURABLE_NAMESPACE` | `CLEAT_NAMESPACE` |
| `DURABLE_ALLOWED_NAMESPACES` | `CLEAT_ALLOWED_NAMESPACES` |

**Files to change**:

| File(s) | Count |
|---------|-------|
| `cmd/cleat/main.go` | ~8 references |
| `cmd/cleat/dev.go` | 2 references |
| `internal/host/fault_test.go` | 2 references |
| `internal/host/concurrency_test.go` | 2 references |
| `tests/cluster/*.go` (5 files) | ~8 references |
| `tests/integrity/event_history_test.go` | 1 reference |
| `tests/scale/throughput_test.go` | 1 reference |
| `tests/upgrade/schema_migration_test.go` | 1 reference |
| `README.md` | ~6 references |
| `durable-execution-design.md` | ~10 references |
| `transformer-plan.md` | 1 reference |
| `docs/review-status.md` | 1 reference |
| `ports-round2/cleat-payment-state-machine/cleat_sdk/_decorators.py` | 1 reference (`_DURABLE_ENTRY_REGISTRY` → `_CLEAT_ENTRY_REGISTRY`) |

---

## Category G: Docker and K8s Resource Names

**What changes**:

| Old | New |
|-----|-----|
| Docker image `durable-worker:latest` | `cleat-worker:latest` |
| Docker image `ghcr.io/rcownie/durable-worker` | `ghcr.io/rcownie/cleat-worker` |
| K8s deployment `durable-worker` | `cleat-worker` |
| K8s service `durable-worker` | `cleat-worker` |
| K8s ConfigMap `durable-worker-config` | `cleat-worker-config` |
| K8s HPA `durable-worker-hpa` | `cleat-worker-hpa` |
| K8s label `app: durable-worker` | `app: cleat-worker` |

**Files**:
```
docker-compose.cluster.yml             — 4 image references, all service names
docker-compose.monitoring.yml          — network references
k8s/deployment.yaml                    — deployment name, labels, HPA name, image
k8s/service.yaml                       — service name, labels
k8s/configmap.yaml                     — configmap name, labels
charts/cleat/values.yaml              — image repository
charts/cleat/templates/*.yaml          — any hardcoded names
.github/workflows/ci.yml              — image tag in cluster-tests job
cmd/cleat/templates/agent/docker-compose.yml — image name
```

---

## Category H: Prometheus and Grafana Naming

**Status**: Mostly already "cleat" but has a few "durable" remnants.

**What changes**:

| File | What |
|------|------|
| `monitoring/grafana/dashboard.json` | Tags: remove `'durable'` from tags array; description: "Cleat Durable Workflow Engine" → "Cleat Workflow Engine" |
| `monitoring/grafana-dashboard.json` | Title: "Cleat Durable Execution" → "Cleat Workflow Execution"; tags: remove `'durable-execution'` |
| `monitoring/prometheus/metrics.go` | Meter path string references old repo path — update to `github.com/rcownie/cleat/monitoring/prometheus` |

---

## Category I: Documentation and Config Files

**What changes**:

| File | What |
|------|------|
| `README.md` | All `durable` → `cleat` (CLI commands, import paths, env vars) |
| `durable-execution-design.md` | Rename file to `cleat-execution-design.md`; all `durable_*` ABI refs, env vars, system names |
| `durable_context.md` | Rename file to `cleat_context.md`; update content |
| `ABI.md` | ABI function names |
| `COMPARISON.md` | Any `durable.HostCalls` → `cleat.HostCalls`; `durable.NewSaga()` → `cleat.NewSaga()` |
| `docs/review-status.md` | Env var references |
| `transformer-plan.md` | Env var references |
| `.github/workflows/ci.yml` | Comment: `# CI/CD Pipeline for cleat/durable` → `# CI/CD Pipeline for cleat`; crate directory references |
| `.pre-commit-config.yaml` | Comment: `# Pre-commit hook configuration for cleat/durable` → `# Pre-commit hook configuration for cleat` |
| `Makefile` | Comments and binary targets |
| `benchmarks/comparative/README.md` | Any import path examples |
| `benchmarks/comparative/runner.sh` | Any framework references |
| `benchmarks/README.md` | Import paths |
| `plans/comparative-benchmarks.md` | Any `durable` references |
| `plans/cluster-testing-ci-benchmarks.md` | Any `durable` references |

---

## Category J: In-Code String References

These are strings inside source files that print/display "durable" but
aren't covered by the categories above.

**Go string references** (error messages, log output, CLI help):
```
cmd/cleat/main.go       — CLI description, help text, usage examples
cmd/cleat-worker/main.go — startup messages, health check responses
cmd/cleat-gen/main.go    — generated code comments, tool description
internal/wasm/adapter.go — error format strings ("durable_call: error code %d")
```

**Rust string references**:
```
crates/cleat-macro/src/entry.rs  — generated code comments, error messages
crates/cleat-sdk/src/lib.rs      — doc comments
```

**Python string references**:
```
python-sdk/cleat_sdk/host_calls.py — docstrings, error messages
```

**AS string references**:
```
packages/cleat-as/assembly/host-calls.ts — JSDoc comments ("durable_sleep: Suspend...")
packages/cleat-as/assembly/saga.ts       — any string references
```

---

## On-Disk Renames Summary

| Old Path | New Path |
|----------|----------|
| `cmd/durable/` | `cmd/cleat/` |
| `cmd/durable-worker/` | `cmd/cleat-worker/` |
| `cmd/cleat-gen/` | `cmd/cleat-gen/` |
| `cmd/cleat-bench/` | `cmd/cleat-bench/` |
| `durable/` | `cleat/` |
| `durable/durabletest/` | `cleat/cleattest/` |
| `durable/durabletest/durabletest.go` | `cleat/cleattest/cleattest.go` |
| `durable/durabletest/durabletest_test.go` | `cleat/cleattest/cleattest_test.go` |
| `crates/durable-java/` | `crates/cleat-java/` |
| `crates/durable-macro/` | `crates/cleat-macro/` |
| `crates/durable-sdk/` | `crates/cleat-sdk/` |
| `packages/cleat-as/assembly/durable-entry.ts` | `packages/cleat-as/assembly/cleat-entry.ts` |
| `crates/cleat-java/.../DurableEntry.java` | `crates/cleat-java/.../CleatEntry.java` |
| `crates/cleat-java/.../DurableEntryProcessor.java` | `crates/cleat-java/.../CleatEntryProcessor.java` |
| `crates/cleat-java/.../DurableResult.java` | `crates/cleat-java/.../CleatResult.java` |
| `crates/cleat-java/.../DurableResultTest.java` | `crates/cleat-java/.../CleatResultTest.java` |
| `durable-execution-design.md` | `cleat-execution-design.md` |
| `durable_context.md` | `cleat_context.md` |
| `cmd/cleat/templates/agent/durable.yaml` | `cmd/cleat/templates/agent/cleat.yaml` |

Build artifact directories (`target/`, `.gradle/`, `build/`, `node_modules/`)
do NOT need manual renaming — they are `.gitignore`d and will regenerate
on next build.

---

## Execution Order

Categories are independent and can run in parallel. Within each category,
order matters as listed.

```
Phase 1 (parallel, no dependencies):
  ├── Category B: Binary/command names (git mv + string updates)
  ├── Category D: WASM ABI host functions
  ├── Category E: SDK decorators and entry points
  ├── Category F: Environment variables
  ├── Category G: Docker and K8s resource names
  ├── Category H: Prometheus/Grafana naming
  ├── Category I: Documentation and config files
  └── Category J: In-code string references

Phase 2 (depends on Phase 1 completing):
  └── Category A: Go module path (imports the renamed packages)

Phase 3 (depends on Category A):
  └── Category C: Go SDK package rename (the durable/ → cleat/ directory)
```

Note: Categories A and C are intentionally last. Renaming the Go module
path is the largest mechanical change and should happen after all other
renames are stable so that `go build` / `go vet` can verify correctness
immediately.

---

## Verification Checklist

After all categories complete, verify:

```bash
# 1. No "durable" in source files (excluding .git, build artifacts, go.sum)
grep -r 'durable' --include='*.go' --include='*.rs' --include='*.py' \
  --include='*.ts' --include='*.java' --include='*.yaml' --include='*.yml' \
  --include='*.json' --include='*.md' --include='*.sh' --include='Makefile' \
  --include='*.toml' --include='*.mod' . \
  --exclude-dir=.git --exclude-dir=target --exclude-dir=node_modules \
  --exclude-dir=.gradle --exclude-dir=build --exclude-dir=dist
# Expected: zero results

# 2. No "DURABLE_" in source files
grep -r 'DURABLE_' --include='*.go' --include='*.py' --include='*.sh' \
  --include='*.md' . --exclude-dir=.git
# Expected: zero results

# 3. Go build succeeds
go build ./...

# 4. Go vet passes
go vet ./...

# 5. Go tests pass
go test -count=1 ./...

# 6. Rust builds
cargo build --manifest-path crates/cleat-macro/Cargo.toml
cargo build --manifest-path crates/cleat-sdk/Cargo.toml

# 7. Python imports work
cd python-sdk && python -c "from cleat_sdk.entry import cleat_entry; print('OK')"

# 8. Java compiles
cd crates/cleat-java && ./gradlew build

# 9. No directories named "durable" remain
find . -type d -name '*durable*' -not -path '*/.git/*' -not -path '*/target/*' \
  -not -path '*/node_modules/*' -not -path '*/.gradle/*' -not -path '*/build/*'
# Expected: zero results (build artifact dirs are excluded)

# 10. No files named "*durable*" remain
find . -type f -name '*durable*' -not -path '*/.git/*' -not -path '*/target/*' \
  -not -path '*/node_modules/*' -not -path '*/.gradle/*' -not -path '*/build/*'
# Expected: zero results
```

---

## Estimate

| Phase | Categories | Files | Effort |
|-------|-----------|-------|--------|
| 1 | B, D–J | ~120 | 4–6 hours (parallel) |
| 2 | A | ~190 | 2–3 hours |
| 3 | C | ~60 | 1–2 hours |
| Verification | — | — | 1–2 hours |
| **Total** | | **~300** | **~1.5 days** |

Using subagents to parallelize Phase 1 categories reduces wall-clock time
to roughly 1 day.
