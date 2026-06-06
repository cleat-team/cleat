# STATUS — cleat-230-logse

**Phase:** done
**Started:** 2026-06-06T07:14:56Z
**Completed:** 2026-06-06T07:30:00Z
**Budget:** $5
**Spent:** $2 (exploration only — no implementation)

## Summary

Exploration complete. Full report at `artifacts/exploration-report.md`.

### Found

- **85 `log.Printf` calls** in production code (engine + worker + internal)
- **30 `fmt.Printf` debug calls** in WASM backend WIT scanner code
- **3 hot-path log.Printf** in `engine/engine.go` child workflow path — these are debug artifacts, should be deleted
- **20 store-layer log.Printf** in `engine/db.go`, `engine/mysql_store.go`, `engine/mssql_store.go` — need structured logging
- **4 worker-loop log.Printf** in `memory_monitor.go`, `memory_controller.go`, `app.go`
- **12 startup/one-time log.Printf** — lower priority
- **15+15 debug fmt.Printf** in `backend_wasmtime.go` and `wit_dylib_stack.go` — should be removed or gated

### Already Clean

- `cmd/cleat-worker/main.go` — 0 log.Printf, fully on slog
- All 22 plugins — use slog via Environment.Logger
- `engine/app.go` — slog throughout

### Recommendation

Hand off to a developer agent for implementation. The fix is ~4 hours of work
across 5 phases (see report). Phase 1 (delete debug lines) is 15 minutes and
removes the most egregious hot-path spam. The store-layer plumbing (Phase 2) is
the bulk of the work but follows a consistent pattern across 3 store backends.
