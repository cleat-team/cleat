package blobstore

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/cleat-team/cleat/internal/plugin"
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
			start := time.Now()
			staleRefs, expiredEntries, orphanedBlobs, err := p.cleanupExpired(ctx)
			if err != nil {
				p.logger.Error("blobstore: TTL cleanup failed",
					"plugin", p.Info().Name,
					"error", err,
				)
				continue
			}
			p.logger.Info("blobstore: work cycle completed",
				"plugin", p.Info().Name,
				"duration_ms", time.Since(start).Milliseconds(),
				"stale_refs", staleRefs,
				"expired_entries", expiredEntries,
				"orphaned_blobs", orphanedBlobs,
			)
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
func (p *Plugin) cleanupExpired(ctx context.Context) (staleRefs, expiredEntries, orphanedBlobs int, err error) {
	// Phase 1: clean up stale workflow blob references. A ref is stale when
	// the referencing workflow is no longer in-flight (done, failed, cancelled).
	result1, err := p.db.Exec(ctx, plugin.Rebind(`
		DELETE FROM workflow_blob_refs
		WHERE workflow_id NOT IN (
			SELECT id FROM workflow_instances WHERE status IN ('ready', 'running')
		)
	`, p.dialect))
	if err != nil {
		return staleRefs, expiredEntries, orphanedBlobs, err
	}
	if result1 > 0 {
		staleRefs = int(result1)
	}

	// Phase 2: delete expired and soft-deleted index entries, decrementing
	// ref_count on blob_content.
	var affected int64
	if p.dialect == plugin.DialectMySQL {
		// MySQL: DELETE first, then UPDATE (no DELETE..RETURNING)
		_, err = p.db.Exec(ctx, plugin.Rebind(deleteBlobIndexExpired.For(p.dialect), p.dialect))
		if err != nil {
			return staleRefs, expiredEntries, orphanedBlobs, err
		}
		affected, err = p.db.Exec(ctx, plugin.Rebind(deleteChunksReturning.For(p.dialect), p.dialect))
	} else {
		affected, err = p.db.Exec(ctx, plugin.Rebind(deleteChunksReturning.For(p.dialect), p.dialect))
	}
	if err != nil {
		return staleRefs, expiredEntries, orphanedBlobs, err
	}
	expiredEntries = int(affected)
	if affected > 0 {
		p.logger.Info("blobstore: expired/deleted index entries cleaned", "count", affected)
	}

	// Phase 3: garbage-collect blob_content with no remaining references,
	// but only if no in-flight workflow references the content.
	var orphanRows plugin.Rows
	if p.dialect == plugin.DialectMySQL {
		// MySQL: SELECT first (no DELETE..RETURNING)
		orphanRows, err = p.db.Query(ctx, plugin.Rebind(deleteBlobReturning.For(p.dialect), p.dialect))
	} else {
		orphanRows, err = p.db.Query(ctx, plugin.Rebind(deleteBlobReturning.For(p.dialect), p.dialect))
	}
	if err != nil {
		return staleRefs, expiredEntries, orphanedBlobs, err
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
	orphanedBlobs = orphaned

	// MySQL: the SELECT above did not delete, so clean up explicitly.
	if p.dialect == plugin.DialectMySQL && orphaned > 0 {
		_, _ = p.db.Exec(ctx, plugin.Rebind(deleteOrphanBlobs.For(p.dialect), p.dialect))
	}

	return staleRefs, expiredEntries, orphanedBlobs, nil
}
