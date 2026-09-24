package blobstore

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
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

	// baseCtx carries no tenant marker at all -- kept alongside the
	// AcrossAllTenants-marked ctx below because cleat#2125's SQL Server path
	// needs to layer plugin.ForTenant on top of a CLEAN context. beginTenantTx
	// checks for a cross-tenant marker before it checks for a tenant, so
	// ForTenant on top of an already-cross-tenant ctx is a no-op: the bypass
	// wins. See sweepStaleWorkflowRefsMSSQL in this file.
	baseCtx := ctx

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
	// Marked here and used for phases 2 and 3, plus phase 1 on PostgreSQL and
	// MySQL, because every one of those statements is cross-tenant for the
	// same reason. Phase 1 on SQL Server is the one exception -- see
	// sweepStaleWorkflowRefsMSSQL, which takes baseCtx instead. The handlers
	// in routes.go are deliberately NOT marked: they run on r.Context(), which
	// carries the request's tenant, and neither is the host call in
	// host_functions.go, which has carried the workflow's tenant since
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
			staleRefs, expiredEntries, orphanedBlobs, err := p.cleanupExpired(ctx, baseCtx)
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
//
// ctx is the AcrossAllTenants-marked context Run built; baseCtx is the
// unmarked one it was built from. Every phase but the SQL Server arm of
// phase 1 uses ctx -- see sweepStaleWorkflowRefs for why that one arm needs
// baseCtx instead.
func (p *Plugin) cleanupExpired(ctx, baseCtx context.Context) (staleRefs, expiredEntries, orphanedBlobs int, err error) {
	// Phase 1: clean up stale workflow blob references. A ref is stale when
	// the referencing workflow is no longer in-flight (done, failed, cancelled).
	result1, err := p.sweepStaleWorkflowRefs(ctx, baseCtx)
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

// sweepStaleWorkflowRefs removes workflow_blob_refs rows whose workflow is no
// longer in flight. PostgreSQL and MySQL do it in the one statement
// staleWorkflowRefs holds (see its own doc for why each is safe cross-tenant
// today). SQL Server has no arm there at all -- cleat#2125 -- and runs
// sweepStaleWorkflowRefsMSSQL instead, on baseCtx rather than ctx.
func (p *Plugin) sweepStaleWorkflowRefs(ctx, baseCtx context.Context) (int64, error) {
	if p.dialect == plugin.DialectMSSQL {
		return p.sweepStaleWorkflowRefsMSSQL(baseCtx)
	}
	return p.db.Exec(ctx, plugin.Rebind(staleWorkflowRefs.For(p.dialect), p.dialect))
}

// sweepStaleWorkflowRefsMSSQL is SQL Server's half of cleat#2125.
//
// WHY NOT A PER-TENANT LOOP OF THE WHOLE STATEMENT, the shape
// sweepAbandonedJobsPerTenant in plugins/jobqueue/background.go uses.
// task_queue is declared TenantScoped, so looping it per tenant scopes BOTH
// sides of that statement -- the UPDATE's own table and the workflow_instances
// subquery -- to the same tenant at once. workflow_blob_refs is not: it has no
// tenant_id column at all (migrations.go's v3), so it carries no row-level
// security to scope it. Looping THIS statement's DELETE per tenant would
// scope only the subquery, and during tenant A's turn tenant B's genuinely
// in-flight workflow ids are invisible (dbo.fn_tenant_filter admits only A),
// so the DELETE would read them as not-in-flight and remove tenant B's refs
// on tenant A's turn -- deleting exactly the rows this fix exists to protect.
//
// So the in-flight set has to be gathered whole, across every tenant, before
// anything is deleted. Each tenant's rows can only be read under its own
// SESSION_CONTEXT (RLS again, this time working as intended), one query per
// tenant; the DELETE itself is unscoped, because workflow_blob_refs has
// nothing for RLS to scope.
//
// WHY NOT WHERE workflow_id NOT IN (<every in-flight id>) IN ONE STATEMENT.
// SQL Server's driver-level parameter ceiling is 2100 per statement, and nothing
// bounds how many workflows a busy fleet has in flight across every tenant at
// once. Excluding by NOT IN also does not compose across batches -- splitting
// the exclusion list into two statements would have each one delete the other
// batch's still-in-flight refs, since neither NOT IN clause can see the ids the
// other protects. Inverted instead: read every workflow_id workflow_blob_refs
// currently references (bounded by how many distinct workflows have ever
// written a blob ref that has not yet been swept -- self-limiting, since this
// sweep is what retires them), subtract the in-flight set in Go, and delete the
// remainder with a plain IN clause, which -- unlike NOT IN -- is safe to batch:
// each batch only needs the ids it targets.
//
// THE CANDIDATE READ MUST HAPPEN BEFORE THE IN-FLIGHT READ, not after. This
// function read in-flight first until cleat-review measured what that order
// does under real concurrency: a workflow that starts and writes its first
// blob ref in the gap between the two reads is a CANDIDATE (it is in
// workflow_blob_refs by the time the second query runs) but is invisible to
// an in-flight set that was captured before it existed -- so it is deleted on
// the very sweep that should have protected it. On a 200-tenant SQL Server
// run with a live writer racing the sweep for 8s, this lost 48 of 1201 newly
// started workflows' refs (4%); a 1-tenant run lost 3 of 771. See
// TestReview2141_ConcurrentNewWorkflowKeepsItsRef.
//
// Reading candidates FIRST closes that window: anything that starts after
// the candidate read is not a candidate this round (it will be one next
// sweep, once it has actually gone stale), and anything that is in flight AT
// THE LATER in-flight read is protected regardless of how long ago it
// started. A workflow that finishes in the gap between the two reads is
// correctly identified as stale either way -- this is not a race for that
// case, since a finished workflow does not un-finish.
//
// THE RETRYWORKFLOW WINDOW IS NOT NEW HERE, AND IS NOT MSSQL-SPECIFIC.
// Between a dead-lettered workflow's refs being read as stale and the DELETE
// that removes them, RetryWorkflow could move it back to 'ready' --
// dead-lettered is not in-flight by this sweep's own definition ('ready',
// 'running'), so its refs are a legitimate deletion candidate right up until
// someone retries it. All three stores implement RetryWorkflow as a bare
// status UPDATE that does NOT recreate workflow_blob_refs rows
// (engine/store_lifecycle.go, engine/mysql_lifecycle.go,
// engine/mssql_lifecycle.go), so a retry landing in that window permanently
// loses the workflow's blob references on every dialect, not only this one.
// staleWorkflowRefs (queries.go) reads the identical in-flight set from
// Postgres's admin.in_flight_workflow_ids() and MySQL's direct subquery, in
// one statement each, and carries the same candidate-then-retried exposure --
// this function's two separate reads widen the window in wall-clock terms
// but do not change its kind. It is therefore reviewed as a pre-existing
// property of the retry/sweep relationship, not a defect this redesign
// introduced, and is left as-is rather than engineered around: closing it on
// any dialect would mean re-checking in-flight status from inside the DELETE
// itself, and here that statement is deliberately unscoped (see above) -- a
// tenant-scoped check inlined there would reintroduce the exact
// RLS-blindness this whole function exists to avoid.
//
// sweepStaleWorkflowRefsMSSQLTestHook, when non-nil, runs after the
// candidate read and before the in-flight read below -- the exact window
// the read order exists to protect. Nil in production;
// TestSweepStaleWorkflowRefsMSSQL_DeterministicInterleave sets it.
var sweepStaleWorkflowRefsMSSQLTestHook func()

func (p *Plugin) sweepStaleWorkflowRefsMSSQL(ctx context.Context) (int64, error) {
	rows, err := p.db.Query(ctx, `SELECT DISTINCT workflow_id FROM workflow_blob_refs`)
	if err != nil {
		return 0, fmt.Errorf("blobstore: list referenced workflow ids: %w", err)
	}
	var candidates []string
	for rows.Next() {
		var wfID string
		if err := rows.Scan(&wfID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("blobstore: list referenced workflow ids: scan: %w", err)
		}
		candidates = append(candidates, wfID)
	}
	rerr := rows.Err()
	_ = rows.Close()
	if rerr != nil {
		return 0, fmt.Errorf("blobstore: list referenced workflow ids: %w", rerr)
	}

	// Test-only: lets a deterministic test interleave a write in the exact
	// window the ordering above exists to protect. Nil, and free, in
	// production.
	if sweepStaleWorkflowRefsMSSQLTestHook != nil {
		sweepStaleWorkflowRefsMSSQLTestHook()
	}

	inFlight, err := p.allInFlightWorkflowIDsMSSQL(ctx)
	if err != nil {
		return 0, fmt.Errorf("blobstore: list in-flight workflow ids: %w", err)
	}

	var toDelete []string
	for _, wfID := range candidates {
		if _, live := inFlight[wfID]; !live {
			toDelete = append(toDelete, wfID)
		}
	}

	const batchSize = 500 // well under SQL Server's 2100-parameter ceiling
	var total int64
	for start := 0; start < len(toDelete); start += batchSize {
		batch := toDelete[start:min(start+batchSize, len(toDelete))]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = fmt.Sprintf("@p%d", i+1)
			args[i] = id
		}
		n, err := p.db.Exec(ctx,
			`DELETE FROM workflow_blob_refs WHERE workflow_id IN (`+strings.Join(placeholders, ", ")+`)`,
			args...)
		if err != nil {
			return total, fmt.Errorf("blobstore: delete stale refs: %w", err)
		}
		total += n
	}
	return total, nil
}

// allInFlightWorkflowIDsMSSQL reads every tenant's in-flight workflow ids,
// one tenant at a time under that tenant's own SESSION_CONTEXT. ctx must
// carry no AcrossAllTenants marker -- plugin.ForTenant on top of one is a
// no-op, since beginTenantTx checks for a cross-tenant bypass first.
func (p *Plugin) allInFlightWorkflowIDsMSSQL(ctx context.Context) (map[string]struct{}, error) {
	tenants, err := plugin.AllTenantIDs(ctx, p.db, p.dialect)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	ids := make(map[string]struct{})
	for _, tid := range tenants {
		id, perr := uuid.Parse(tid)
		if perr != nil {
			return nil, fmt.Errorf("tenant id %q is not a UUID: %w", tid, perr)
		}
		tctx := plugin.ForTenant(ctx, id)
		rows, err := p.db.Query(tctx, `SELECT id FROM workflow_instances WHERE status IN ('ready', 'running')`)
		if err != nil {
			return nil, fmt.Errorf("in-flight ids for tenant %s: %w", tid, err)
		}
		for rows.Next() {
			var wfID string
			if err := rows.Scan(&wfID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("in-flight ids for tenant %s: scan: %w", tid, err)
			}
			ids[wfID] = struct{}{}
		}
		rerr := rows.Err()
		_ = rows.Close()
		if rerr != nil {
			return nil, fmt.Errorf("in-flight ids for tenant %s: %w", tid, rerr)
		}
	}
	return ids, nil
}
