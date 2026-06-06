# Verification — cleat-230-race-fix4k

Independent re-verification of cleat-230-race-fix4 against the original TASK.md and CONTRACT.md.

## Method

Grep-based verification of every acceptance criterion and dead code item. Each item checked against current source files.

## Item 1: ShardedStore.Close() lock

```go
// engine/sharded_store.go:74-82
func (s *ShardedStore) Close() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, shard := range s.shards {
		if shard.Close != nil {
			shard.Close()
		}
	}
}
```

Pattern matches `ShardedStore.Shards()` which also uses `mu.RLock()`.

## Item 2: Compaction retry

```go
// engine/compaction.go:219-237
for attempt := 0; attempt < 3; attempt++ {
    if attempt > 0 {
        delay := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
        // context check + sleep
    }
    compactErr = store.CompactHistory(...)
    if compactErr == nil {
        break
    }
    if !isCompactionDeadlockError(compactErr) {
        break
    }
}
```

`isCompactionDeadlockError()` at line 248-259 covers: PostgreSQL 40P01 (via `pq.Error`), MySQL 1213 (string match), MSSQL (string match on "deadlock"/"Deadlock").

## Item 3: Compaction generation guard

- Postgres `engine/db.go:2875`: `WHERE id = $3 AND generation = $4`
- MSSQL `engine/mssql_store.go:2185`: `WHERE id = @p1 AND generation = @p4`
- MySQL `engine/mysql_ops.go:451`: `WHERE id = ? AND tenant_id = ? AND generation = ?`

## Item 4: Dead code removal

Grep for each identifier in `engine/` returned zero matches in the target file, confirming removal.
