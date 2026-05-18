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
