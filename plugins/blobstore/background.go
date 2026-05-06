package blobstore

import (
	"context"
	"encoding/hex"
	"time"
)

// Run starts the TTL cleanup goroutine. It runs every hour, deleting expired
// blob_index entries and garbage-collecting blob_content rows whose ref_count
// reaches zero. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("blobstore: no database, TTL cleanup disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	p.logger.Info("blobstore: TTL cleanup started, interval=1h")

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("blobstore: TTL cleanup stopped")
			return nil

		case <-ticker.C:
			if err := p.cleanupExpired(ctx); err != nil {
				p.logger.Error("blobstore: TTL cleanup failed", "error", err)
			}
		}
	}
}

// cleanupExpired handles expired and soft-deleted blobs while respecting
// in-flight workflow references. Blobs are never physically deleted from S3
// while any workflow with status 'ready' or 'running' references them.
//
// Phase 1: Clean up stale workflow_blob_refs for workflows that are no longer
// in-flight (status is 'done', 'failed', or 'cancelled').
//
// Phase 2: Delete expired blob_index entries (expires_at < now()) and
// soft-deleted entries (deleted_at IS NOT NULL) and decrement ref_count
// on the corresponding blob_content rows.
//
// Phase 3: Garbage-collect blob_content rows with ref_count <= 0, but only
// if no in-flight workflow references the content via workflow_blob_refs.
func (p *Plugin) cleanupExpired(ctx context.Context) error {
	// Phase 1: clean up stale workflow blob references. A ref is stale when
	// the referencing workflow is no longer in-flight (done, failed, cancelled).
	_, err := p.db.ExecContext(ctx, `
		DELETE FROM workflow_blob_refs
		WHERE workflow_id NOT IN (
			SELECT id FROM workflow_instances WHERE status IN ('ready', 'running')
		)
	`)
	if err != nil {
		return err
	}

	// Phase 2: delete expired and soft-deleted index entries, decrementing
	// ref_count on blob_content.
	result, err := p.db.ExecContext(ctx, `
		WITH deleted AS (
			DELETE FROM blob_index
			WHERE (expires_at < now() OR deleted_at IS NOT NULL)
			RETURNING sha256
		)
		UPDATE blob_content
		SET ref_count = ref_count - (SELECT count(*) FROM deleted WHERE deleted.sha256 = blob_content.sha256)
		WHERE sha256 IN (SELECT sha256 FROM deleted)
	`)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		p.logger.Info("blobstore: expired/deleted index entries cleaned", "count", affected)
	}

	// Phase 3: garbage-collect blob_content with no remaining references,
	// but only if no in-flight workflow references the content.
	orphanRows, err := p.db.QueryContext(ctx, `
		DELETE FROM blob_content
		WHERE ref_count <= 0
		  AND NOT EXISTS (
			SELECT 1 FROM workflow_blob_refs r
			WHERE r.sha256 = blob_content.sha256
		  )
		RETURNING sha256, storage_backend
	`)
	if err != nil {
		return err
	}
	defer orphanRows.Close()

	var orphaned int
	for orphanRows.Next() {
		var sha256Bytes []byte
		var storageBackend string
		if err := orphanRows.Scan(&sha256Bytes, &storageBackend); err != nil {
			p.logger.Error("blobstore: scan orphan", "error", err)
			continue
		}
		if storageBackend == "s3" {
			sha256Hex := hex.EncodeToString(sha256Bytes)
			if err := p.backend.Delete(ctx, sha256Hex); err != nil {
				p.logger.Error("blobstore: s3 delete orphan", "sha256", sha256Hex, "error", err)
			}
		}
		orphaned++
	}
	if orphaned > 0 {
		p.logger.Info("blobstore: orphaned content cleaned", "count", orphaned)
	}

	return nil
}
