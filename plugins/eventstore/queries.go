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
	MySQL: `INSERT INTO event_stream (tenant_id, stream_id, sequence, event)
VALUES ($1, $2, (
	SELECT COALESCE(MAX(sequence), 0) + 1
	FROM event_stream
	WHERE tenant_id = $1 AND stream_id = $2
), CAST($3 AS JSON))`,
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
