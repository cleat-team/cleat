package eventtriggers

import "github.com/cleat-team/cleat/internal/plugin"

// Dialect-specific query variants for structurally different SQL.

var upsertAwaiter = plugin.Query{
	Default: `INSERT INTO event_awaiters (workflow_id, tenant_id, event_type, created_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (workflow_id, event_type) DO UPDATE
	SET created_at = NOW()`,
	MySQL: `INSERT INTO event_awaiters (workflow_id, tenant_id, event_type, created_at)
VALUES ($1, $2, $3, NOW())
ON DUPLICATE KEY UPDATE
	created_at = NOW()`,
	MSSQL: `MERGE event_awaiters AS target
USING (VALUES ($1, $2, $3, SYSUTCDATETIME())) AS source (workflow_id, tenant_id, event_type, created_at)
ON target.workflow_id = source.workflow_id AND target.event_type = source.event_type
WHEN MATCHED THEN UPDATE SET created_at = SYSUTCDATETIME()
WHEN NOT MATCHED THEN INSERT (workflow_id, tenant_id, event_type, created_at)
VALUES (source.workflow_id, source.tenant_id, source.event_type, source.created_at);`,
}

var insertEventIdempotent = plugin.Query{
	Default: `INSERT INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed)
VALUES ($1, $2, $3, $4, NOW(), false)
ON CONFLICT (id) DO NOTHING`,
	MySQL: `INSERT IGNORE INTO ingested_events (id, tenant_id, event_type, event_data, received_at, processed)
VALUES ($1, $2, $3, $4, NOW(), false)`,
	MSSQL: `MERGE ingested_events AS target
USING (VALUES ($1, $2, $3, $4, SYSUTCDATETIME(), 0)) AS source (id, tenant_id, event_type, event_data, received_at, processed)
ON target.id = source.id
WHEN NOT MATCHED THEN INSERT (id, tenant_id, event_type, event_data, received_at, processed)
VALUES (source.id, source.tenant_id, source.event_type, source.event_data, source.received_at, source.processed);`,
}

var insertSubscriptionReturning = plugin.Query{
	Default: `INSERT INTO event_subscriptions (tenant_id, event_type, def_name, entry_point, input_template, filter_expr, max_retries, enabled, created_at)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, 3), true, $8)
RETURNING id`,
	MySQL: `INSERT INTO event_subscriptions (id, tenant_id, event_type, def_name, entry_point, input_template, filter_expr, max_retries, enabled, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9)`,
	MSSQL: `INSERT INTO event_subscriptions (tenant_id, event_type, def_name, entry_point, input_template, filter_expr, max_retries, enabled, created_at)
OUTPUT INSERTED.id
VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, 3), 1, $8)`,
}

var queryUnprocessedEvents = plugin.Query{
	Default: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE NOT processed
  AND (status = 'pending' OR status IS NULL)
  AND received_at < NOW() - INTERVAL '10 seconds'
ORDER BY received_at
LIMIT 100`,
	MySQL: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE NOT processed
  AND (status = 'pending' OR status IS NULL)
  AND received_at < DATE_SUB(NOW(), INTERVAL 10 SECOND)
ORDER BY received_at
LIMIT 100`,
	MSSQL: `SELECT id, tenant_id, event_type, event_data, retry_count
FROM ingested_events
WHERE NOT processed
  AND (status = 'pending' OR status IS NULL)
  AND received_at < DATEADD(second, -10, SYSUTCDATETIME())
ORDER BY received_at
OFFSET 0 ROWS FETCH NEXT 100 ROWS ONLY`,
}
