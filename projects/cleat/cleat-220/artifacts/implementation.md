# Implementation — cleat-220: testing.Short() Audit

## Summary

Two mechanical 3-line insertions matching the existing `TestDB()` pattern exactly.

## Files changed

### `internal/host/testutil/mysql_schema.go` (line 256)

Added `testing.Short()` gate to `MySQLTestDB()`:

```go
if testing.Short() {
    t.Skip("Skipping MySQL test in short mode")
}
```

### `internal/host/testutil/mssql_schema.go` (line 280)

Added `testing.Short()` gate to `MSSQLTestDB()`:

```go
if testing.Short() {
    t.Skip("Skipping MSSQL test in short mode")
}
```

## Verification

| Check | Result |
|---|---|
| `go build ./internal/host/testutil/...` | PASS (no output) |
| `go vet ./internal/host/testutil/...` | PASS (no output) |
| `go test -short` with `CLEAT_TEST_MYSQL` set | SKIP confirmed ("Skipping MySQL test in short mode") |
| `go test` without `-short` | Connection attempted normally (Short() gate not triggered) |

## Notes

- Pattern matches `TestDB()` at `schema.go:508-510` identically except for the skip message
- Callers that already have their own `testing.Short()` gate are double-gated harmlessly — the helper's gate fires first
- `MSSQLTestDB()` gate confirmed structurally identical; runtime test skipped because `CLEAT_TEST_MSSQL` was not set in the test environment, but the gate is in the correct position

---

## §§3-6 Implementation (remaining 4 gaps)

### `tests/plugin-harness/testdb.go` (line 30)

Added `testing.Short()` gate to `OpenTestDB()`:

```go
if testing.Short() {
    t.Skip("Skipping database test in short mode")
}
```

Covers `multi_db_plugin_test.go` transitively through the `OpenTestDB()` → caller path.
Matches the existing `TestDB()` convention exactly.

### `cmd/cleat/as_transform_test.go` (line 34)

Added `testing.Short()` gate to `TestASTransform()` before the toolchain-detection checks
(`node --version`, `npx --version`). This test shells out to `node`, `npx`, and `asc`.

### `cmd/cleat/cleat_pipeline_test.go` (line 1453)

Added `testing.Short()` gate to `TestRunBuild_PythonTarget_WasmRoundtrip()` before the
`componentize-py` LookPath check. This test shells out to `python3`, `componentize-py`,
and `wasm-tools`.

### `cmd/cleat/vet_test.go` — three coordinated changes

1. **TestMain** (line 18): Wrapped the `go build` step in `if !testing.Short()` so pure
   unit tests in the same package (e.g., `TestDetectVetLang`, and ~20+ tests in
   `cleat_pipeline_test.go`) can still run in short mode. Added `flag.Parse()` call
   before `testing.Short()` — required by Go (panics otherwise).

2. **runVetCmd** (line 54): Added Short() gate, transitively covering 13 of 16 vet test
   functions that shell out to the compiled `cleat` binary.

3. **TestVetExitCode** (line 376): Added direct Short() gate (uses `cleatBinary` directly,
   not through `runVetCmd`).

`TestDetectVetLang` intentionally not gated — pure unit test, no binary needed.

## Verification (all packages)

| Check | Result |
|---|---|
| `go build ./tests/plugin-harness/... ./cmd/cleat/...` | PASS |
| `go vet ./tests/plugin-harness/... ./cmd/cleat/...` | PASS |
| `go test -short -count=1 ./tests/plugin-harness/...` | PASS (all skip/pass appropriately) |
| `go test -short -count=1 ./cmd/cleat/...` | PASS (vet tests SKIP, pure unit tests PASS) |
| `TestASTransform` in short mode | SKIP ✓ |
| `TestRunBuild_PythonTarget_WasmRoundtrip` in short mode | SKIP ✓ |
| `TestDetectVetLang` in short mode | PASS ✓ (pure unit test, still runs) |

---

## §8 Addendum — TestMSSQLStoreFactory gate (2026-05-17 re-audit)

### `internal/host/mssql_store_test.go` (line 48)

Added `testing.Short()` gate to `TestMSSQLStoreFactory()` after the existing env var check:

```go
if testing.Short() {
    t.Skip("Skipping MSSQL database test in short mode")
}
```

This test opens a real SQL Server connection via `sql.Open("sqlserver", connStr)`,
bypassing the Short()-gated `MSSQLTestDB()` helper that other MSSQL tests use.

### Verification

| Check | Result |
|---|---|
| `go build ./internal/host/...` | PASS |
| `go vet ./internal/host/...` | PASS |
| `go test -short -run TestMSSQLStoreFactory ./internal/host/` | SKIP (no env var) |
| `CLEAT_TEST_MSSQL=... go test -short -v -run TestMSSQLStoreFactory ./internal/host/` | SKIP (short mode, env var set) |

---

## §§9-19 Re-audit (2026-05-18)

### Summary

No code changes needed. All 11 files identified in the exploration phase are
**transitively protected** via gated package-level `testDB()` or `OpenTestDB()` helpers.
In Go, all `_test.go` files in the same package share package-level functions —
duplicating gates in each file would add redundancy without improving coverage.

### How transitive protection works

Each affected test package defines exactly one `testDB()` helper with a single
`testing.Short()` gate. Every test function in that package calls the same gated
helper, so the gate applies uniformly across all test files in the package.

### Gated helpers

| Package | Helper | Gate location |
|---|---|---|
| `tests/scale/` | `testDB()` | `throughput_test.go:20-22` |
| `tests/upgrade/` | `testDB()` | `schema_migration_test.go:19-21` |
| `tests/integrity/` | `testDB()` | `event_history_test.go:21-23` |
| `tests/plugin-harness/` | `OpenTestDB()` | `testdb.go:30-32` |

### Per-section disposition

| Section | File | Disposition |
|---------|------|-------------|
| §9 | tests/scale/latency_test.go | Calls gated testDB() |
| §10 | tests/scale/concurrent_workflows_test.go | Calls gated testDB() |
| §11 | tests/upgrade/worker_rolling_test.go | Calls gated testDB() |
| §12 | tests/upgrade/wasm_version_test.go | Calls gated testDB() |
| §13 | tests/integrity/ambiguity_detection_test.go | DB section calls gated testDB(); WASM-only section has no DB dep |
| §14 | tests/integrity/concurrent_test.go | Calls gated testDB() |
| §15 | tests/integrity/compaction_test.go | Calls gated testDB() |
| §16 | tests/integrity/replay_determinism_test.go | Calls gated testDB() |
| §17 | tests/integrity/wal_corruption_test.go | Calls gated testDB() |
| §18 | tests/plugin-harness/multi_db_plugin_test.go | Calls gated OpenTestDB() |
| §19 | internal/host/python_wasm_e2e_test.go | Already disabled via t.Skip(); ABI boundary test is pure unit test |

### Verification

```bash
$ go test -short -count=1 ./tests/scale/...           # ok  0.007s
$ go test -short -count=1 ./tests/integrity/...        # ok  0.008s
$ go test -short -count=1 ./tests/plugin-harness/...   # ok  0.011s
$ go test -short -count=1 ./internal/host/...          # ok  0.085s
```

`tests/upgrade` has a pre-existing build error (StartNewRun signature mismatch in
wasm_version_test.go) that is unrelated to the testing.Short() audit.

### Files changed in this phase

- `projects/cleat/cleat-220/CONTRACT.md` — Mark §§9-19 as resolved
- `projects/cleat/cleat-220/STATUS.md` — Mark task complete
- `projects/cleat/cleat-220/artifacts/implementation.md` — Append this section
