package blobstore

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/cleat-team/cleat/plugin"
)

// cleanupInterval is how often Run sweeps.
//
// A var rather than a literal so that the test which proves Run marks its own
// sweep can drive a real tick. At an hour, nothing in a test can make the loop
// fire, so the AcrossAllTenants call below would be covered by no test at all
// -- and it is the one line whose absence breaks the worker outright rather
// than degrading it. cleat#1512.
var cleanupInterval = time.Hour

// Run starts the TTL cleanup goroutine. It runs on cleanupInterval, deleting
// expired blob_index entries and garbage-collecting blob_content rows whose
// ref_count reaches zero. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("blobstore: no database, TTL cleanup disabled")
		<-ctx.Done()
		return nil
	}

	// THE SWEEP NAMES ITSELF CROSS-TENANT. cleat#1512. blob_index carries a
	// row-level policy from migration v4, and the policy calls
	// cleat.assert_tenant_set(), which RAISEs rather than filtering when no
	// tenant is in scope. Without this the loop does not degrade -- it fails
	// outright on its first statement against blob_index after the migration
	// lands.
	//
	// AcrossAllTenants rather than ForTenant, because there is no tenant to be
	// had: cleanupExpired deletes by expires_at and by deleted_at across every
	// tenant, and its other two tables -- blob_content, keyed by sha256, and
	// workflow_blob_refs, keyed by workflow -- have no tenant_id to narrow to
	// even in principle. The discrimination matters: a writer that HAS a tenant
	// and loses it on the way to context.Background() wants ForTenant, and
	// bypassing there compiles, passes every test, and silently disables
	// isolation on that path.
	//
	// Marked ONCE here rather than at the seven statements in cleanupExpired,
	// because every one of them is cross-tenant for the same reason. The
	// handlers in routes.go are deliberately NOT marked: they run on
	// r.Context(), which carries the request's tenant, and neither is the host
	// call in host_functions.go, which has carried the workflow's tenant since
	// cleat#1492 bridged it at the PluginCall boundary.
	ctx = plugin.AcrossAllTenants(ctx,
		"blobstore TTL cleanup: expiry and orphan collection run over every tenant's index")

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	p.logger.Info("blobstore: TTL cleanup started", "interval", cleanupInterval)

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
	result1, err := p.db.Exec(ctx, plugin.Rebind(staleWorkflowRefs.For(p.dialect), p.dialect))
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
		// MySQL has no DELETE..RETURNING, so the decrement and the delete are
		// separate statements. The UPDATE counts the index rows matching the
		// expiry predicate, so it MUST run first: with the DELETE first its
		// subquery finds nothing, the join is empty, and ref_count is never
		// decremented at all (cleat#1148).
		affected, err = p.db.Exec(ctx, plugin.Rebind(deleteChunksReturning.For(p.dialect), p.dialect))
		if err != nil {
			return staleRefs, expiredEntries, orphanedBlobs, err
		}
		_, err = p.db.Exec(ctx, plugin.Rebind(deleteBlobIndexExpired.For(p.dialect), p.dialect))
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
