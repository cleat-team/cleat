package eventstore

import "github.com/rcownie/cleat/internal/plugin"

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
