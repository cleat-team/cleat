# CONTRACT — cleat-230-race-fix4

## Deliverables

1. `mu.RLock()` / `mu.RUnlock()` in `ShardedStore.Close()` (engine/sharded_store.go:74-80)
2. Retry loop in `CompactWorkflowHistory` or `CompactHistory` — 3 attempts, exponential backoff (100ms, 200ms, 400ms), only retrying on deadlock (40P01) errors
3. `generation = ?` guard on compaction UPDATE — add to `PostgresStore.CompactHistory`, `MySQLStore.CompactHistory`, `MSSQLStore.CompactHistory`
4. Dead code removal:
   - `engine/runtime.go`: remove `completeMu`, `completeResult`, `completeErr` fields and `cleatComplete` type if only used by these fields
   - `engine/runtime.go`: remove `workEntryPoint`, `workInput` fields
   - `engine/engine.go`: remove `signals` map from `execSession`
   - `engine/engine.go`: remove `QueryHandlers()` method
   - `engine/backend_wasmtime.go`: remove `executeViaDispatcher` function
   - `engine/compaction.go`: remove `loadAllEventsForCompaction` function
5. Verify compilation after each removal

## Invariants

- No functional behavior change except: compaction retries on deadlock (previously failed immediately)
- All dead code items confirmed zero references before removal
- Compilation succeeds after each individual removal (incremental verification)
- `ShardedStore.Shards()` already uses `mu.RLock()` — `Close()` pattern must match

## API Surface

- `ShardedStore.Close()` — no API change, internal lock added
- `QueryHandlers()` — removed from public API (was never called)
- No other API changes

## Integration Points

- `engine/sharded_store.go` — `Close()` method (line 74-80)
- `engine/compaction.go` — `CompactWorkflowHistory` (line 189-223)
- `engine/db.go` — `PostgresStore.CompactHistory` (line 2848-2872)
- `engine/mysql_ops.go` — `MySQLStore.CompactHistory` (line 424)
- `engine/mssql_store.go` — `MSSQLStore.CompactHistory` (line 2155)
- `engine/runtime.go` — Runtime struct (remove dead fields)
- `engine/engine.go` — execSession struct (remove dead fields/methods)
- `engine/backend_wasmtime.go` — remove dead function

## Test Requirements

- Unit test: `ShardedStore.Close()` with concurrent `Shards()` call (verify no race with `-race`)
- Unit test: compaction retry on mock store that fails with deadlock error twice then succeeds
- Existing tests must pass (`go test ./engine/...`)

## Coupling

- LOOSE with `cleat-230-race-fix1` (both touch engine/runtime.go — fix1 changes buffer fields, fix4 removes dead fields; different lines, no conflict)
- NONE with `cleat-230-race-fix2`, `cleat-230-race-fix3`
