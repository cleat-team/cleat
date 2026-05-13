package kvstore

import "github.com/cleat-team/cleat/internal/plugin"

// Dialect-specific query variants for structurally different SQL.
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
	MSSQL: `MERGE kv_store AS target
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
