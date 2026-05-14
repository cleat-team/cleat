package blobstore

import "github.com/cleat-team/cleat/internal/plugin"

// Dialect-specific query variants for structurally different SQL.
var upsertBlobContent = plugin.Query{
	Default: `INSERT INTO blob_content (sha256, size, ref_count, storage_backend, s3_key)
VALUES ($1, $2, 1, $3, $4)
ON CONFLICT (sha256) DO UPDATE
SET ref_count = blob_content.ref_count + 1`,
	MySQL: `INSERT INTO blob_content (sha256, size, ref_count, storage_backend, s3_key)
VALUES ($1, $2, 1, $3, $4)
ON DUPLICATE KEY UPDATE
ref_count = ref_count + 1`,
	MSSQL: `MERGE blob_content AS target
USING (VALUES ($1, $2, 1, $3, $4)) AS source (sha256, size, ref_count, storage_backend, s3_key)
ON target.sha256 = source.sha256
WHEN MATCHED THEN UPDATE SET ref_count = target.ref_count + 1
WHEN NOT MATCHED THEN INSERT (sha256, size, ref_count, storage_backend, s3_key)
VALUES (source.sha256, source.size, source.ref_count, source.storage_backend, source.s3_key);`,
}

var upsertBlobContentData = plugin.Query{
	Default: `INSERT INTO blob_content (sha256, size, data, ref_count, storage_backend)
VALUES ($1, $2, $3, 0, 'memory')
ON CONFLICT (sha256) DO UPDATE
SET data = EXCLUDED.data`,
	MySQL: `INSERT INTO blob_content (sha256, size, data, ref_count, storage_backend)
VALUES ($1, $2, $3, 0, 'memory')
ON DUPLICATE KEY UPDATE
data = VALUES(data)`,
	MSSQL: `MERGE blob_content AS target
USING (VALUES ($1, $2, $3, 0, 'memory')) AS source (sha256, size, data, ref_count, storage_backend)
ON target.sha256 = source.sha256
WHEN MATCHED THEN UPDATE SET data = source.data
WHEN NOT MATCHED THEN INSERT (sha256, size, data, ref_count, storage_backend)
VALUES (source.sha256, source.size, source.data, source.ref_count, source.storage_backend);`,
}

var upsertBlobRef = plugin.Query{
	Default: `INSERT INTO workflow_blob_refs (workflow_id, sha256)
VALUES ($1, $2) ON CONFLICT DO NOTHING`,
	MySQL: `INSERT IGNORE INTO workflow_blob_refs (workflow_id, sha256) VALUES ($1, $2)`,
	MSSQL: `MERGE workflow_blob_refs AS target
USING (VALUES ($1, $2)) AS source (workflow_id, sha256)
ON target.workflow_id = source.workflow_id AND target.sha256 = source.sha256
WHEN NOT MATCHED THEN INSERT (workflow_id, sha256) VALUES (source.workflow_id, source.sha256);`,
}

var upsertBlobIndex = plugin.Query{
	Default: `INSERT INTO blob_index (key, tenant_id, sha256, size, content_type, tags)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, key) DO UPDATE
SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size,
    content_type = EXCLUDED.content_type, tags = EXCLUDED.tags,
    expires_at = NULL`,
	MySQL: `INSERT INTO blob_index (` + "`key`" + `, tenant_id, sha256, size, content_type, tags)
VALUES ($1, $2, $3, $4, $5, $6)
ON DUPLICATE KEY UPDATE
sha256 = VALUES(sha256), size = VALUES(size),
content_type = VALUES(content_type), tags = VALUES(tags),
expires_at = NULL`,
	MSSQL: `MERGE blob_index AS target
USING (VALUES ($1, $2, $3, $4, $5, $6)) AS source ([key], tenant_id, sha256, size, content_type, tags)
ON target.tenant_id = source.tenant_id AND target.[key] = source.[key]
WHEN MATCHED THEN UPDATE SET
    sha256 = source.sha256, size = source.size,
    content_type = source.content_type, tags = source.tags,
    expires_at = NULL
WHEN NOT MATCHED THEN INSERT ([key], tenant_id, sha256, size, content_type, tags)
VALUES (source.[key], source.tenant_id, source.sha256, source.size, source.content_type, source.tags);`,
}

var upsertBlobIndexWithTTL = plugin.Query{
	Default: `INSERT INTO blob_index (key, tenant_id, sha256, size, content_type, tags, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, key) DO UPDATE
SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size,
    content_type = EXCLUDED.content_type, tags = EXCLUDED.tags,
    expires_at = EXCLUDED.expires_at`,
	MySQL: `INSERT INTO blob_index (` + "`key`" + `, tenant_id, sha256, size, content_type, tags, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON DUPLICATE KEY UPDATE
sha256 = VALUES(sha256), size = VALUES(size),
content_type = VALUES(content_type), tags = VALUES(tags),
expires_at = VALUES(expires_at)`,
	MSSQL: `MERGE blob_index AS target
USING (VALUES ($1, $2, $3, $4, $5, $6, $7)) AS source ([key], tenant_id, sha256, size, content_type, tags, expires_at)
ON target.tenant_id = source.tenant_id AND target.[key] = source.[key]
WHEN MATCHED THEN UPDATE SET
    sha256 = source.sha256, size = source.size,
    content_type = source.content_type, tags = source.tags,
    expires_at = source.expires_at
WHEN NOT MATCHED THEN INSERT ([key], tenant_id, sha256, size, content_type, tags, expires_at)
VALUES (source.[key], source.tenant_id, source.sha256, source.size, source.content_type, source.tags, source.expires_at);`,
}

var jsonbContains = plugin.Query{
	Default: `i.tags @> $1`,
	MySQL:   `JSON_CONTAINS(i.tags, CAST($1 AS JSON))`,
	MSSQL:   `EXISTS (SELECT 1 FROM OPENJSON(i.tags) AS t1 INNER JOIN OPENJSON($1) AS t2 ON t1.[key] = t2.[key] AND t1.value = t2.value)`,
}

var deleteChunksReturning = plugin.Query{
	Default: `WITH deleted AS (
	DELETE FROM blob_index
	WHERE (expires_at < now() OR deleted_at IS NOT NULL)
	RETURNING sha256
)
UPDATE blob_content
SET ref_count = ref_count - (SELECT count(*) FROM deleted WHERE deleted.sha256 = blob_content.sha256)
WHERE sha256 IN (SELECT sha256 FROM deleted)`,
	MySQL: `UPDATE blob_content bc
INNER JOIN (
	SELECT bi.sha256, COUNT(*) AS cnt
	FROM blob_index bi
	WHERE (bi.expires_at < NOW() OR bi.deleted_at IS NOT NULL)
	GROUP BY bi.sha256
) d ON bc.sha256 = d.sha256
SET bc.ref_count = bc.ref_count - d.cnt`,
	MSSQL: `WITH deleted AS (
	DELETE FROM blob_index
	OUTPUT DELETED.sha256
	WHERE (expires_at < SYSUTCDATETIME() OR deleted_at IS NOT NULL)
)
UPDATE bc
SET ref_count = ref_count - cnt.cnt
FROM blob_content bc
INNER JOIN (
	SELECT sha256, COUNT(*) AS cnt
	FROM deleted
	GROUP BY sha256
) cnt ON bc.sha256 = cnt.sha256`,
}

var deleteBlobIndexExpired = plugin.Query{
	Default: `DELETE FROM blob_index
WHERE (expires_at < now() OR deleted_at IS NOT NULL)`,
	MySQL: `DELETE FROM blob_index
WHERE (expires_at < NOW() OR deleted_at IS NOT NULL)`,
	MSSQL: `DELETE FROM blob_index
WHERE (expires_at < SYSUTCDATETIME() OR deleted_at IS NOT NULL)`,
}

var deleteBlobReturning = plugin.Query{
	Default: `DELETE FROM blob_content
WHERE ref_count <= 0
  AND NOT EXISTS (
	SELECT 1 FROM workflow_blob_refs r
	WHERE r.sha256 = blob_content.sha256
  )
RETURNING sha256, storage_backend`,
	MySQL: `SELECT sha256, storage_backend FROM blob_content
WHERE ref_count <= 0
  AND NOT EXISTS (
	SELECT 1 FROM workflow_blob_refs r
	WHERE r.sha256 = blob_content.sha256
  )`,
	MSSQL: `DELETE FROM blob_content
OUTPUT DELETED.sha256, DELETED.storage_backend
WHERE ref_count <= 0
  AND NOT EXISTS (
	SELECT 1 FROM workflow_blob_refs r
	WHERE r.sha256 = blob_content.sha256
  )`,
}

var deleteOrphanBlobs = plugin.Query{
	Default: `DELETE FROM blob_content
WHERE ref_count <= 0
  AND NOT EXISTS (
	SELECT 1 FROM workflow_blob_refs r
	WHERE r.sha256 = blob_content.sha256
  )`,
	MySQL: `DELETE bc FROM blob_content bc
LEFT JOIN workflow_blob_refs r ON bc.sha256 = r.sha256
WHERE bc.ref_count <= 0 AND r.sha256 IS NULL`,
	MSSQL: `DELETE bc FROM blob_content bc
LEFT JOIN workflow_blob_refs r ON bc.sha256 = r.sha256
WHERE bc.ref_count <= 0 AND r.sha256 IS NULL`,
}
