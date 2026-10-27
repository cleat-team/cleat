package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// isPostgresRollbackGuaranteed reports whether err is one of the two
// PostgreSQL SQLSTATEs where the server guarantees the transaction was rolled
// back -- 40P01 (deadlock_detected) and 40001 (serialization_failure) -- the
// same pair wrapPostgresFinalizeDBError already classifies as ErrTransient,
// and for the identical reason: the server has definitively undone the work,
// so replaying it is sound whether or not the transaction is idempotent.
//
// Narrower than "is this retryable in general" on purpose, matching
// isMSSQLRollbackGuaranteed's own narrowing (mssql_retry.go): a connection
// drop or a context deadline leaves the outcome unknown, and retrying one of
// those can double-apply a side effect. Those stay unretried here.
func isPostgresRollbackGuaranteed(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && (pqErr.Code == "40P01" || pqErr.Code == "40001")
}

// postgresTxRetries and postgresTxRetryDelay mirror mssqlTxRetries and
// mssqlTxRetryDelay exactly -- same trade, same numbers: four attempts at
// 20/40/80ms costs at most 140ms of added latency on a path that otherwise
// fails outright, which is the right price for a deadlock.
const (
	postgresTxRetries    = 3
	postgresTxRetryDelay = 20 * time.Millisecond
)

// withPostgresRollbackGuaranteedRetry is withRollbackGuaranteedRetry
// (mssql_retry.go), with isPostgresRollbackGuaranteed in place of
// isMSSQLRollbackGuaranteed. Kept as its own small copy rather than
// generalising the MSSQL function over both predicates: the two have never
// shared a caller, and threading a predicate parameter through MSSQL's
// retry path to serve a single new PostgreSQL caller is more risk to an
// already-relied-upon helper than this ~20-line duplicate costs.
//
// cleat#3245 Phase 3 step 3 piece B: before this PR, ClaimWorkflows'
// candidate SELECT ... FOR UPDATE SKIP LOCKED locked workflow_instances rows
// directly, so the claim UPDATE later in the same transaction only ever
// re-used a lock this transaction already held -- it could never newly block,
// and so could never deadlock. The cutover moved that SKIP LOCKED lock onto
// workflow_leases, a different table, so the claim UPDATE against
// workflow_instances now acquires its row locks fresh. A concurrent writer of
// workflow_instances that does not skip locks (ReapStaleInstances' heartbeat
// sweep, in production) can now deadlock against it -- reproduced
// independently by both the coordinator session and cleat-review against
// TestClaimWorkflows_RespectsLimitUnderConcurrency (0/100 on the pre-cutover
// base, ~1.5% on this cutover, same test, same container).
//
// PostgreSQL guarantees a full rollback on 40P01 with no partial effect (the
// whole claim is one transaction), so replaying it is sound. This wrapper is
// the production fix; it is more correct than a test-side retry because it
// also protects every real caller of ClaimWorkflows/ClaimStickyWorkflows
// (rotating_claim.go's dispatch loop), not only the test that happened to
// reproduce the gap.
func withPostgresRollbackGuaranteedRetry(ctx context.Context, op string, maxRetries int, baseDelay time.Duration, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseDelay * (1 << (attempt - 1))
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: context cancelled during retry: %w", op, ctx.Err())
			case <-time.After(delay):
			}
		}

		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isPostgresRollbackGuaranteed(err) {
			return err
		}
	}
	return fmt.Errorf("%s: exhausted %d retries: %w", op, maxRetries, lastErr)
}
