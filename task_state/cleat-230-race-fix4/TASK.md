# cleat-230-race-fix4 — Defensive hardening and dead code removal

**Parent:** cleat-230-race (Race Condition Audit)
**Budget:** $10 (~0.5 engineer-day)
**Priority:** 3 (polish)
**Type:** Hardening + cleanup

## Task

Defensive hardening: add missing lock to ShardedStore.Close(), add retry logic for compaction deadlock, add generation guard to compaction UPDATE, and remove confirmed dead code.

### Items

1. **ShardedStore.Close() lock**: Add `s.mu.RLock()` / `s.mu.RUnlock()` around the shards iteration in `Close()` (line 74-80). Safe today because shards are immutable, but fragile for future changes.

2. **Compaction retry**: Add retry loop (3 attempts, exponential backoff) around `CompactHistory` to handle 40P01 deadlock errors from Postgres.

3. **Compaction generation guard**: Add `generation = ?` to the UPDATE in `CompactHistory` for consistency with other workflow operations that use optimistic locking.

4. **Remove dead code**:
   - `engine/runtime.go:54-56` — `completeMu`, `completeResult`, `completeErr` (never accessed; context-based mechanism used instead)
   - `engine/engine.go:1671` — `signals` map (never read or written in production code)
   - `engine/engine.go:4086-4087` — `QueryHandlers()` method (never called)
   - `engine/backend_wasmtime.go:335-409` — `executeViaDispatcher` (never called)
   - `engine/compaction.go:504` — `loadAllEventsForCompaction` (never called)
   - `engine/runtime.go:46-47` — `workEntryPoint`/`workInput` fields (never read; wazero path uses a no-op stub)

### Acceptance criteria

1. `ShardedStore.Close()` holds `mu.RLock()` during iteration
2. Compaction retries on 40P01 deadlock error, up to 3 attempts
3. Compaction UPDATE includes `generation = ?` guard
4. All dead code items removed without compilation errors
5. Existing tests pass (no functional changes except retry)

### Out of scope

- Watchdog generation counter (larger change, defer to separate task)
- Dynamic shard reconfiguration support
- Compaction scheduling changes
