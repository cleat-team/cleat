package kvstore

import "github.com/cleat-team/cleat/plugin"

// upsertKV creates or overwrites a key's row with per-dialect atomicity.
//
// MSSQL's MERGE needs WITH (HOLDLOCK): without it, two concurrent MERGE
// statements against the SAME not-yet-existing key can both evaluate "WHEN
// NOT MATCHED" true under READ COMMITTED (neither sees the other's
// uncommitted insert) and both attempt the INSERT branch, which collides on
// kv_store's primary key -- the caller (handlePut) has no retry loop around
// this statement, so a losing racer surfaces that as a raw 500 rather than
// self-healing. This is a documented SQL Server MERGE hazard, not a
// cleat-specific one; see TestConcurrentMergesToABrandNewKeyRaceWithoutHoldlock
// for the measurement, and eventstore/queries.go's upsertStreamHead, which
// has the identical shape and was the first of the two fixed (cleat#2268).
// HOLDLOCK takes a serializable-range lock that forces the second session to
// block instead of racing.
//
// cleat#2890 (this fix): filed as a "latent instance of the same hazard"
// during cleat#2268's review, unconfirmed until falsified here -- a
// generic HTTP-level concurrency test (TestConcurrentPutsToABrandNewKeyDoNotRace,
// same package) did NOT reproduce it even at n=60 over 15 rounds, because
// the per-request round trips ahead of the MERGE (BEGIN TRAN,
// sp_set_session_context) stagger concurrent callers enough to usually miss
// the sub-millisecond race window. A tighter, lower-overhead probe did: see
// the test referenced above for both the standalone pre-commit measurement
// (80 of 1200 raw attempts) and the in-tree regression test.
var upsertKV = plugin.Query{
	Default: `INSERT INTO kv_store (tenant_id, key, value)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, key) DO UPDATE
SET value = EXCLUDED.value,
    version = kv_store.version + 1,
    updated_at = now()
RETURNING version`,
	MySQL: `INSERT INTO kv_store (tenant_id, ` + "`key`" + `, value)
VALUES ($1, $2, $3)
ON DUPLICATE KEY UPDATE
value = VALUES(value),
version = version + 1,
updated_at = NOW()`,
	MSSQL: `MERGE kv_store WITH (HOLDLOCK) AS target
USING (VALUES ($1, $2, $3)) AS source (tenant_id, [key], value)
ON target.tenant_id = source.tenant_id AND target.[key] = source.[key]
WHEN MATCHED THEN UPDATE SET
    value = source.value,
    version = target.version + 1,
    updated_at = SYSUTCDATETIME()
WHEN NOT MATCHED THEN INSERT (tenant_id, [key], value)
VALUES (source.tenant_id, source.[key], source.value)
OUTPUT INSERTED.version;`,
}

var updateKVReturning = plugin.Query{
	Default: `UPDATE kv_store
SET value = $1, version = version + 1, updated_at = now()
WHERE tenant_id = $2 AND key = $3 AND version = $4
RETURNING version`,
	MySQL: `UPDATE kv_store
SET value = $1, version = version + 1, updated_at = NOW()
WHERE tenant_id = $2 AND ` + "`key`" + ` = $3 AND version = $4`,
	MSSQL: `UPDATE kv_store
SET value = $1, version = version + 1, updated_at = SYSUTCDATETIME()
OUTPUT INSERTED.version
WHERE tenant_id = $2 AND [key] = $3 AND version = $4`,
}
