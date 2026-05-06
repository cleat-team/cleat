package blobstore

import (
	"context"
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

// cleanupExpired deletes expired blob_index entries and decrements ref_count
// on blob_content. Content rows with ref_count <= 0 are removed.
func (p *Plugin) cleanupExpired(ctx context.Context) error {
	// Phase 1: delete expired index entries and decrement ref_count.
	result, err := p.db.ExecContext(ctx, `
		WITH deleted AS (
			DELETE FROM blob_index
			WHERE expires_at < now()
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
		p.logger.Info("blobstore: expired index entries cleaned", "count", affected)
	}

	// Phase 2: garbage-collect blob_content with no remaining references.
	result, err = p.db.ExecContext(ctx, `
		DELETE FROM blob_content WHERE ref_count <= 0
	`)
	if err != nil {
		return err
	}
	orphaned, _ := result.RowsAffected()
	if orphaned > 0 {
		p.logger.Info("blobstore: orphaned content cleaned", "count", orphaned)
	}

	return nil
}
