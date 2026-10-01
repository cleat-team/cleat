package eventstore

import "github.com/cleat-team/cleat/plugin"

// Dialect-specific query variants for structurally different SQL.
//
// insertEvent used to pair with nextSequenceForStream, a read-MAX-then-insert
// shape that needed handleAppend's own retry loop for safety under
// concurrency (cleat#2260). cleat#2268 replaced that pair with
// upsertStreamHead (and, on MySQL, selectStreamHead): a per-stream head
// counter whose row lock serializes concurrent appenders to the SAME stream
// instead of racing them. See upsertStreamHead's comment for the mechanism;
// see appendOnce in routes.go for how the two queries below are still used
// together.
var insertEvent = plugin.Query{
	// No CAST($3 AS JSON) on the MySQL value. It was there to satisfy the
	// JSON column type, and migration v3 made the column LONGTEXT -- but the
	// cast is not merely redundant now, it would UNDO the migration.
	// CAST(... AS JSON) applies the JSON type's number narrowing to the
	// value before it reaches the column, so it degrades on the way in even
	// when the destination is text. Measured on MySQL 8.4.11, both columns
	// LONGTEXT, one INSERT:
	//
	//     via CAST   {"x": 1.2345678901234566e29}
	//     direct     {"x":123456789012345678901234567890}
	//
	// So converting the column alone would have left a green test over a
	// still-degrading path. The engine hit exactly this and needed
	// migrations/mysql/071 for it. cleat#1622.
	Default: `INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES ($1, $2, $3, $4::jsonb)`,
	MySQL:   `INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES ($1, $2, $3, $4)`,
	MSSQL:   `INSERT INTO event_stream (tenant_id, stream_id, sequence, event) VALUES ($1, $2, $3, $4)`,
}

// upsertStreamHead atomically creates or increments a stream's head-sequence
// counter in event_stream_head (migration v4) and, where the dialect
// supports it, returns the new value in the same statement. This replaces
// nextSequenceForStream's read-MAX-then-insert-with-retry shape (cleat#2260)
// with real serialization (cleat#2268): the row lock these statements take
// -- on the one row for (tenant_id, stream_id) -- is held for the lifetime
// of the enclosing transaction (appendOnce runs this and insertEvent inside
// one tx), so a SECOND concurrent appender to the SAME stream BLOCKS on this
// statement until the first transaction commits or rolls back, rather than
// reading the same MAX(sequence) and racing a duplicate-key error on the
// INSERT into event_stream. Appends to DIFFERENT streams touch different
// rows and don't contend at all. Mirrors plugins/kvstore/queries.go's
// upsertKV, which establishes the same per-dialect shape for a single-row
// counter.
//
// MSSQL's MERGE needs WITH (HOLDLOCK): without it, two concurrent MERGE
// statements against the SAME not-yet-existing key can both evaluate "WHEN
// NOT MATCHED" true under READ COMMITTED (neither sees the other's
// uncommitted insert) and both attempt the INSERT branch, which is exactly
// the duplicate-key race this migration exists to remove -- just moved from
// event_stream onto event_stream_head. HOLDLOCK takes a serializable-range
// lock that forces the second session to block instead. This is a
// documented SQL Server MERGE hazard, not a cleat-specific one; see the
// regression test for the concurrent-brand-new-stream case this guards
// (plugins/kvstore's MERGE has no HOLDLOCK and is a latent instance of the
// same hazard, out of scope here).
var upsertStreamHead = plugin.Query{
	Default: `INSERT INTO event_stream_head (tenant_id, stream_id, head_sequence)
VALUES ($1, $2, 1)
ON CONFLICT (tenant_id, stream_id) DO UPDATE
SET head_sequence = event_stream_head.head_sequence + 1
RETURNING head_sequence`,
	MySQL: `INSERT INTO event_stream_head (tenant_id, stream_id, head_sequence)
VALUES ($1, $2, 1)
ON DUPLICATE KEY UPDATE
head_sequence = head_sequence + 1`,
	MSSQL: `MERGE event_stream_head WITH (HOLDLOCK) AS target
USING (VALUES ($1, $2)) AS source (tenant_id, stream_id)
ON target.tenant_id = source.tenant_id AND target.stream_id = source.stream_id
WHEN MATCHED THEN UPDATE SET head_sequence = target.head_sequence + 1
WHEN NOT MATCHED THEN INSERT (tenant_id, stream_id, head_sequence)
VALUES (source.tenant_id, source.stream_id, 1)
OUTPUT INSERTED.head_sequence;`,
}

// selectStreamHead reads back the value upsertStreamHead just wrote, for
// MySQL only -- ON DUPLICATE KEY UPDATE has no RETURNING equivalent. Safe to
// read in the SAME transaction as the upsert: a transaction always sees its
// own uncommitted writes regardless of isolation level, and the row lock
// upsertStreamHead took blocks any OTHER transaction from changing the value
// in between. No per-dialect arm: a plain SELECT is portable, and only the
// MySQL caller uses it (see appendOnce).
var selectStreamHead = plugin.Query{
	Default: `SELECT head_sequence FROM event_stream_head WHERE tenant_id = $1 AND stream_id = $2`,
}

var deleteEventsOlderThan = plugin.Query{
	Default: `DELETE FROM event_stream
WHERE created_at < NOW() - make_interval(days => $1)`,
	MySQL: `DELETE FROM event_stream
WHERE created_at < DATE_SUB(NOW(), INTERVAL $1 DAY)`,
	MSSQL: `DELETE FROM event_stream
WHERE created_at < DATEADD(day, -$1, SYSUTCDATETIME())`,
}

// A page of a stream, from a sequence number, with a caller-supplied limit.
//
// The row limit here is a PARAMETER, not a literal, which rules out the usual
// T-SQL answer: `SELECT TOP n` takes a parenthesised expression for a variable
// (`TOP (@p4)`), while `OFFSET … FETCH NEXT @p4 ROWS ONLY` takes one directly
// and reads closer to the original. Both need an ORDER BY, which this has.
//
// The $4 is left as $4 in every arm on purpose: plugin.Rebind turns it into
// @p4 for SQL Server and ? for MySQL, so writing @p4 here would be rewritten
// from a placeholder the adapter no longer recognises. Placeholders are the
// adapter's job; the clause structure is this file's (cleat#1133).
var queryStreamPage = plugin.Query{
	Default: `SELECT sequence, event, created_at
FROM event_stream
WHERE tenant_id = $1 AND stream_id = $2 AND sequence > $3
ORDER BY sequence ASC
LIMIT $4`,
	MySQL: `SELECT sequence, event, created_at
FROM event_stream
WHERE tenant_id = $1 AND stream_id = $2 AND sequence > $3
ORDER BY sequence ASC
LIMIT $4`,
	MSSQL: `SELECT sequence, event, created_at
FROM event_stream
WHERE tenant_id = $1 AND stream_id = $2 AND sequence > $3
ORDER BY sequence ASC
OFFSET 0 ROWS FETCH NEXT $4 ROWS ONLY`,
}
