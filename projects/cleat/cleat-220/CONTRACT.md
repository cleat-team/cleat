# CONTRACT.md — cleat-220: testing.Short() Audit

## Completed

### §1 — MySQLTestDB() gate (commit c72c34b)
- ~~Add `testing.Short()` gate to `MySQLTestDB()` in `internal/host/testutil/mysql_schema.go`~~

### §2 — MSSQLTestDB() gate (commit c72c34b)
- ~~Add `testing.Short()` gate to `MSSQLTestDB()` in `internal/host/testutil/mssql_schema.go`~~

### §3 — OpenTestDB() gate (commit c8aabd8)
- ~~Add `testing.Short()` gate to `OpenTestDB()` in `tests/plugin-harness/testdb.go`~~

### §4 — as_transform_test.go gate (commit c8aabd8)
- ~~Add `testing.Short()` gate to `cmd/cleat/as_transform_test.go`~~

### §5 — cleat_pipeline_test.go gate (commit c8aabd8)
- ~~Add `testing.Short()` gate to `cmd/cleat/cleat_pipeline_test.go`~~

### §6 — vet_test.go gate (commit c8aabd8)
- ~~Add `testing.Short()` gate to `cmd/cleat/vet_test.go` (three-part fix)~~

### §7 — Verify
- ~~`go build ./...` and `go vet ./...` pass cleanly~~
- ~~`go test -short ./...` completes with no external infrastructure needed~~

### §8 — TestMSSQLStoreFactory gate (DONE)
- [x] Add `testing.Short()` gate to `TestMSSQLStoreFactory()` in `internal/host/mssql_store_test.go` (line 49) — gate is implemented. CONTRACT.md was stale.

### §9 — tests/scale/latency_test.go gate (TRANSITIVELY PROTECTED)
- [x] All latency tests call `testDB(t)`, which is gated in `throughput_test.go:20-22` — no explicit gate needed

### §10 — tests/scale/concurrent_workflows_test.go gate (TRANSITIVELY PROTECTED)
- [x] All concurrent workflow tests call `testDB(t)` — same gated helper as §9

### §11 — tests/upgrade/worker_rolling_test.go gate (TRANSITIVELY PROTECTED)
- [x] All worker rolling tests call `testDB(t)`, which is gated in `schema_migration_test.go:19-21`

### §12 — tests/upgrade/wasm_version_test.go gate (TRANSITIVELY PROTECTED)
- [x] All WASM version tests call `testDB(t)` — same gated helper as §11

### §13 — tests/integrity/ambiguity_detection_test.go gate (TRANSITIVELY PROTECTED)
- [x] DB-backed section (line 114+) calls `testDB(t)`, which is gated in `event_history_test.go:21-23`. WASM-only section (lines 91-111) has no DB dependency and correctly runs in any mode.

### §14 — tests/integrity/concurrent_test.go gate (TRANSITIVELY PROTECTED)
- [x] All concurrent tests call `testDB(t)` — same gated helper as §13

### §15 — tests/integrity/compaction_test.go gate (TRANSITIVELY PROTECTED)
- [x] All compaction tests call `testDB(t)` — same gated helper as §13

### §16 — tests/integrity/replay_determinism_test.go gate (TRANSITIVELY PROTECTED)
- [x] All replay determinism tests call `testDB(t)` — same gated helper as §13

### §17 — tests/integrity/wal_corruption_test.go gate (TRANSITIVELY PROTECTED)
- [x] All 6 WAL corruption tests call `testDB(t)` — same gated helper as §13

### §18 — tests/plugin-harness/multi_db_plugin_test.go gate (TRANSITIVELY PROTECTED)
- [x] `TestPluginCalls_MultiDB` calls `OpenTestDB(t, ...)`, which is gated in `testdb.go:30-32`

### §19 — internal/host/python_wasm_e2e_test.go gate (N/A — DISABLED)
- [x] `TestPythonWasmEndToEnd` is blanket-disabled via `t.Skip("disabled: ...")` at line 33. Adding a Short gate would be dead code until re-enabled. `TestPythonWasmAbiBoundary` is a pure unit test with no external dependencies.

## Out of scope

- `tests/cross-language/cross_language_test.go` — intentionally uses toolchain-detection
  `t.Skip()` instead of `testing.Short()`. Documented design choice.
- Adding Short() gates to any other files beyond the 4 listed above.
- CI configuration changes.

## Acceptance criteria

1. `go test -short -count=1 ./tests/plugin-harness/...` skips `OpenTestDB` (no real DB opened)
2. `go test -short -count=1 ./cmd/cleat/...` skips node, python3, wasm-tools, and go build steps
3. `go build ./...` and `go vet ./...` pass cleanly
4. Existing tests continue to work when running without `-short`
