package eventstore

import "github.com/cleat-team/cleat/plugin"

// Dialect-specific query variants for structurally different SQL.
var insertEventReturning = plugin.Query{
	Default: `INSERT INTO event_stream (tenant_id, stream_id, sequence, event)
VALUES ($1, $2, (
	SELECT COALESCE(MAX(sequence), 0) + 1
	FROM event_stream
	WHERE tenant_id = $1 AND stream_id = $2
), $3::jsonb)
RETURNING sequence`,
	// No CAST($3 AS JSON) on the value. It was there to satisfy the JSON
	// column type, and migration v3 made the column LONGTEXT -- but the cast
	// is not merely redundant now, it would UNDO the migration. CAST(... AS
	// JSON) applies the JSON type's number narrowing to the value before it
	// reaches the column, so it degrades on the way in even when the
	// destination is text. Measured on MySQL 8.4.11, both columns LONGTEXT,
	// one INSERT:
	//
	//     via CAST   {"x": 1.2345678901234566e29}
	//     direct     {"x":123456789012345678901234567890}
	//
	// So converting the column alone would have left a green test over a
	// still-degrading path. The engine hit exactly this and needed
	// migrations/mysql/071 for it. cleat#1622.
	MySQL: `INSERT INTO event_stream (tenant_id, stream_id, sequence, event)
VALUES ($1, $2, (
	SELECT COALESCE(MAX(sequence), 0) + 1
	FROM event_stream
	WHERE tenant_id = $1 AND stream_id = $2
), $3)`,
	MSSQL: `INSERT INTO event_stream (tenant_id, stream_id, sequence, event)
OUTPUT INSERTED.sequence
VALUES ($1, $2, (
	SELECT ISNULL(MAX(sequence), 0) + 1
	FROM event_stream
	WHERE tenant_id = $1 AND stream_id = $2
), $3)`,
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
