package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

func (s *PostgresStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	return s.claimWorkflowImpl(ctx, workerID)
}

func (s *PostgresStore) claimWorkflowImpl(ctx context.Context, workerID string) (*WorkflowInstance, error) {
	wfs, err := s.ClaimWorkflows(ctx, workerID, 1)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	return wfs[0], nil
}

// ClaimWorkflows atomically claims up to limit runnable workflow instances.
// Like ClaimWorkflow but batches multiple claims into one query.

// CountRunnableWorkflows mirrors ClaimWorkflows' candidate predicate exactly,
// minus the lock and the LIMIT.
//
// Tenant scoping is beginTxWithRLS, as the claim's is: PostgreSQL carries no
// explicit tenant_id predicate here because the application role is genuinely
// subject to RLS. Adding one would not be harmless -- it would make this count
// disagree with the claim on a connection where RLS is off, which is exactly
// the case cross_tenant_claim_test.go exists to catch.
func (s *PostgresStore) CountRunnableWorkflows(ctx context.Context) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("count runnable workflows: begin: %w", err)
	}
	defer tx.Rollback()

	// Mirrors ClaimWorkflows' candidate predicate exactly, minus the lock and
	// the LIMIT -- the same relationship the old count had to the old claim.
	// A registered queue at capacity is not runnable, exactly as it is not
	// claimable.
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM workflow_instances w
		LEFT JOIN queues q ON q.tenant_id = w.tenant_id
		                  AND q.name = w.concurrency_key
		                  AND q.disabled_at IS NULL
		WHERE w.status IN ('ready', 'terminating')
		  AND w.next_wake_at <= now()
		  AND w.task_queue = ANY($1)
		  AND (
		    (q.name IS NULL AND (
		      w.concurrency_key_hash IS NULL
		      OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
		                      WHERE (ck.key_hash = w.concurrency_key_hash
		                        AND ck.tenant_id = w.tenant_id
		                        AND ck.workflow_id <> w.id)
		                        AND EXISTS (SELECT 1 FROM workflow_instances wi
		                                     WHERE wi.id = ck.workflow_id AND wi.tenant_id = ck.tenant_id
		                                       AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')))
		    ))
		    OR
		    (q.name IS NOT NULL AND (
		      (
		        (SELECT count(*) FROM queue_holders qh
		          WHERE qh.tenant_id = w.tenant_id
		            AND qh.queue_name = q.name
		            AND qh.workflow_id <> w.id
		            AND EXISTS (SELECT 1 FROM workflow_instances wi
		                         WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
		                           AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))) < q.concurrency_limit
		      )
		      AND (
		        q.rate_limit IS NULL OR
		        (SELECT count(*) FROM queue_rate_tokens qrt
		          WHERE qrt.tenant_id = w.tenant_id
		            AND qrt.queue_name = q.name
		            AND qrt.expires_at > now()) < q.rate_limit
		      )
		    ))
		  )
	`, pq.Array(s.taskQueues)).Scan(&n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// claimCandidate is one runnable workflow read by a claim statement, carrying
// the fields the acquisition step needs to decide and take its concurrency key.
// `registered` is true when the key names a registered, non-disabled queue.
type claimCandidate struct {
	id         string
	tenantID   string
	key        *string
	hash       []byte
	registered bool
}

// registeredQueueLimits is what lockRegisteredQueueLimits reads off a locked
// queues row: the concurrency semaphore's capacity, and -- cleat#1918 -- the
// rate limiter's capacity and window. RateLimit and RatePeriodSeconds are nil
// together when the queue has no rate limit, the same nil/nil convention
// Queue itself uses (queue_store.go). workerConcurrency is cleat#1917: nil
// means no per-worker cap, the same lone-nullable shape Queue.WorkerConcurrency
// carries.
type registeredQueueLimits struct {
	concurrencyLimit  int
	rateLimit         *int
	ratePeriodSeconds *int
	workerConcurrency *int
}

// logClaimKeyDecision records what a claim decided about one candidate's
// concurrency key: which key, whether that key named a registered queue, and
// whether the candidate was admitted.
//
// WHY THE ENGINE LOGS THIS AND NOT THE WORKER. cleat#1955 is a nightly failure
// whose whole subject is queue admission -- a queue declaring a limit of 2 that
// appeared to admit 1 -- and it could not be diagnosed from the run's own
// artifacts. The worker's "claimed workflow" line carries worker_id,
// workflow_id, def_name and def_version; across all 38 claim lines of the
// failing leg the concurrency key appeared ZERO times. Two workflows claimed in
// the same millisecond were therefore equally consistent with "the semaphore
// admitted two on one queue" and with "two unrelated keys ran at once", which
// is precisely the distinction the failure turns on.
//
// The worker cannot log it: engine.WorkflowInstance has no concurrency-key
// field, and adding one would mean every construction site that did not
// populate it reported the empty string -- indistinguishable from "this run had
// no key", which is the same ambiguity moved rather than removed. The claim
// candidate holds the key already and cannot be wrong about it, so the decision
// is logged where it is made.
//
// REFUSALS ARE LOGGED, NOT JUST ADMISSIONS, and the refusal is the more
// informative half: "admitted 1 of 3" and "refused 2 of 3 at capacity" answer
// different questions, and only the second distinguishes a semaphore at its
// limit from three runs that never overlapped.
//
// Unkeyed candidates log nothing. Most workflows carry no key, so logging them
// would make the volume proportional to all claims rather than to keyed ones,
// for a line that would say only that there was nothing to decide.
func logClaimKeyDecision(log *slog.Logger, c claimCandidate, admitted bool) {
	if log == nil || c.key == nil {
		return
	}
	log.Info("concurrency key decision",
		"workflow_id", c.id,
		"concurrency_key", *c.key,
		"registered", c.registered,
		"admitted", admitted,
	)
}

func (s *PostgresStore) ClaimWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// cleat#1116. This claim is now three statements inside one transaction,
	// where it used to be one data-modifying CTE. The generalisation from mutex
	// (N=1) to semaphore (N>1 for a registered queue) cannot live in a single
	// statement: enforcing "at most N holders" needs a serialisation point that
	// a statement's own snapshot cannot provide -- two concurrent claims could
	// each read "one slot free" and both insert, silently exceeding the limit.
	// The serialisation point is the queues row (the capacity), locked FOR
	// UPDATE before any count of holders.
	//
	// Statement 1 (below) reads the candidates and locks them FOR UPDATE SKIP
	// LOCKED, as before. Its predicate branches on whether the key names a
	// registered queue. A bare key keeps the NOT EXISTS mutex predicate; a
	// registered queue uses a snapshot count (< concurrency_limit) that is
	// deliberately NOT the guarantee -- it only stops a full queue from wasting
	// a candidate slot. The guarantee is statement 3, re-counted under the lock.
	//
	// Statement 2 locks the registered queues' rows in sorted (name) order, so
	// two claims spanning the same queues in opposite orders cannot deadlock.
	//
	// Statement 3 acquires each candidate's key. A bare key keeps the
	// ON CONFLICT (key_hash, tenant_id) DO NOTHING that is the whole of today's
	// mutex; a registered queue re-counts under the lock and inserts a
	// queue_holders row only if the count admits it. Both arms handle the
	// re-claim case -- a run re-claiming its own held key after a lost fence.
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.tenant_id, c.concurrency_key, c.concurrency_key_hash, c.registered
		FROM (
			SELECT w.id, w.tenant_id, w.concurrency_key, w.concurrency_key_hash,
			       q.name IS NOT NULL AS registered
			FROM workflow_instances w
			LEFT JOIN queues q ON q.tenant_id = w.tenant_id
			                  AND q.name = w.concurrency_key
			                  AND q.disabled_at IS NULL
			WHERE w.status IN ('ready', 'terminating')
			  AND w.next_wake_at <= now()
			  AND w.task_queue = ANY($1)
			  AND (
			    (q.name IS NULL AND (
			      w.concurrency_key_hash IS NULL
			      OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
			                      WHERE (ck.key_hash = w.concurrency_key_hash
			                        AND ck.tenant_id = w.tenant_id
			                        AND ck.workflow_id <> w.id)
			                        AND EXISTS (SELECT 1 FROM workflow_instances wi
			                                     WHERE wi.id = ck.workflow_id AND wi.tenant_id = ck.tenant_id
			                                       AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')))
			    ))
			    OR
			    (q.name IS NOT NULL AND (
			      (
			        (SELECT count(*) FROM queue_holders qh
			          WHERE qh.tenant_id = w.tenant_id
			            AND qh.queue_name = q.name
			            AND qh.workflow_id <> w.id
			            AND EXISTS (SELECT 1 FROM workflow_instances wi
			                         WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
			                           AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))) < q.concurrency_limit
			      )
			      AND (
			        q.rate_limit IS NULL OR
			        (SELECT count(*) FROM queue_rate_tokens qrt
			          WHERE qrt.tenant_id = w.tenant_id
			            AND qrt.queue_name = q.name
			            AND qrt.expires_at > now()) < q.rate_limit
			      )
			    ))
			  )
			ORDER BY w.priority ASC, w.created_at
			LIMIT $2
			FOR UPDATE OF w SKIP LOCKED
		) c
	`, pq.Array(s.taskQueues), limit)
	if err != nil {
		return nil, fmt.Errorf("claim workflows: select candidates: %w", err)
	}
	var cands []claimCandidate
	for rows.Next() {
		var c claimCandidate
		if err := rows.Scan(&c.id, &c.tenantID, &c.key, &c.hash, &c.registered); err != nil {
			rows.Close()
			return nil, fmt.Errorf("claim workflows: scan candidate: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("claim workflows: candidates rows: %w", err)
	}
	rows.Close()

	// Statement 2: lock the registered queues among the candidate keys, sorted.
	limits, err := s.lockRegisteredQueueLimits(ctx, tx, cands)
	if err != nil {
		return nil, err
	}

	// Statement 3: acquire each candidate's key and keep the winners.
	var ids []string
	for _, c := range cands {
		ok, err := s.acquireCandidateConcurrencyKey(ctx, tx, c, limits, workerID)
		if err != nil {
			return nil, err
		}
		logClaimKeyDecision(s.log(), c, ok)
		if ok {
			ids = append(ids, c.id)
		}
	}
	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	// The UPDATE is the same as before, except the winners were already decided
	// above, so the predicate is just membership in ids.
	rows2, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances w
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
		    promise_seq_at_claim = promise_seq,
		    assigned_to = $1,
		    heartbeat_at = now(),
		    started_at = COALESCE(started_at, now()),
		    generation = generation + 1
		WHERE w.id = ANY($2)
		RETURNING w.id, w.def_name, w.def_version, w.status, w.input, w.assigned_to, w.next_wake_at, w.tenant_id, w.created_at, w.error_code, w.error_op, w.generation, COALESCE(w.priority, 0) AS priority, COALESCE(w.trace_id, '') AS trace_id, COALESCE(w.pending_terminal_status, '') AS pending_terminal_status
	`, workerID, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("claim workflows: update: %w", err)
	}
	defer rows2.Close()

	wfs, err := scanClaimedWorkflows(rows2)
	if err != nil {
		return nil, err
	}
	if err := rows2.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}
	if err := s.claimLeaseRows(ctx, tx, workerID, ids); err != nil {
		return nil, fmt.Errorf("claim workflows: %w", err)
	}
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

// claimLeaseRows mirrors the claim UPDATE's SET clause onto workflow_leases
// -- cleat#3245 Phase 3 step 2, piece 2 (dual-write). Called from both
// ClaimWorkflows and ClaimStickyWorkflows, on the SAME tx, BEFORE
// finishClaim: finishClaim commits that tx as its first action, so this
// cannot run inside it or after it -- only before.
//
// ids is the already-decided set of winning workflow ids -- ClaimWorkflows
// passes the same ids slice its own UPDATE used; ClaimStickyWorkflows
// collects them from its claimed wfs after scanning. Re-deriving a second
// "who won" computation here would risk disagreeing with the UPDATE that
// already ran against workflow_instances, which is exactly the kind of
// drift this dual-write exists to not introduce.
func (s *PostgresStore) claimLeaseRows(ctx context.Context, tx *sql.Tx, workerID string, ids []string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
		    promise_seq_at_claim = promise_seq,
		    assigned_to = $1,
		    heartbeat_at = now(),
		    started_at = COALESCE(started_at, now()),
		    generation = generation + 1
		WHERE id = ANY($2)
	`, workerID, pq.Array(ids)); err != nil {
		return fmt.Errorf("update lease rows: %w", err)
	}
	return nil
}

// lockRegisteredQueueLimits locks, in sorted (name) order, the rows of the
// registered non-disabled queues named by the candidates, and returns each
// name's concurrency_limit. The locks are held until the transaction commits,
// which is what makes the count in acquireCandidateConcurrencyKey see a stable
// number of holders: no other claim can be between its own count and insert for
// the same queue while this transaction holds the queue's row.
func (s *PostgresStore) lockRegisteredQueueLimits(ctx context.Context, tx *sql.Tx, cands []claimCandidate) (map[string]registeredQueueLimits, error) {
	limits := map[string]registeredQueueLimits{}
	seen := map[string]bool{}
	var keys []string
	for _, c := range cands {
		if c.registered && !seen[*c.key] {
			seen[*c.key] = true
			keys = append(keys, *c.key)
		}
	}
	if len(keys) == 0 {
		return limits, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT name, concurrency_limit, rate_limit, rate_period_seconds, worker_concurrency FROM queues
		WHERE tenant_id = $1 AND name = ANY($2) AND disabled_at IS NULL
		ORDER BY name
		FOR UPDATE
	`, s.tenantID, pq.Array(keys))
	if err != nil {
		return nil, fmt.Errorf("claim workflows: lock registered queues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ql registeredQueueLimits
		var rateLimit, ratePeriodSeconds, workerConcurrency sql.NullInt64
		if err := rows.Scan(&name, &ql.concurrencyLimit, &rateLimit, &ratePeriodSeconds, &workerConcurrency); err != nil {
			return nil, fmt.Errorf("claim workflows: scan queue limit: %w", err)
		}
		ql.rateLimit, ql.ratePeriodSeconds = nullInt64Pair(rateLimit, ratePeriodSeconds)
		ql.workerConcurrency = nullableIntFromSQL(workerConcurrency)
		limits[name] = ql
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim workflows: queue limits rows: %w", err)
	}
	return limits, nil
}

// acquireCandidateConcurrencyKey acquires one candidate's concurrency key and
// reports whether the candidate is claimed. A bare key keeps the mutex path; a
// registered queue uses the semaphore path, safe because the queue's row was
// locked by lockRegisteredQueueLimits. cleat#1918: a registered queue's rate
// limit, if it has one, is a second and independent gate checked under the
// same lock. cleat#1917: workerID's own per-worker cap, if the queue has one,
// is a third -- all three must admit for the candidate to be claimed.
func (s *PostgresStore) acquireCandidateConcurrencyKey(ctx context.Context, tx *sql.Tx, c claimCandidate, limits map[string]registeredQueueLimits, workerID string) (bool, error) {
	if !c.registered {
		if c.hash == nil {
			return true, nil // no key at all
		}
		// Bare key: mutex via ON CONFLICT, exactly the old acquired-CTE shape.
		var returned string
		err := tx.QueryRowContext(ctx, `
			INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
			VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5)
			ON CONFLICT (key_hash, tenant_id) DO NOTHING
			RETURNING workflow_id
		`, c.hash, c.key, c.id, claimedKeyTTL.Seconds(), c.tenantID).Scan(&returned)
		if err == nil {
			return true, nil // this call took the key
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("claim workflows: acquire bare key: %w", err)
		}
		// Conflict: either this run already holds the key (a re-claim after a
		// lost fence) or another run took it. Only the first is claimable.
		var holder string
		err = tx.QueryRowContext(ctx,
			`SELECT workflow_id FROM concurrency_keys WHERE key_hash = $1 AND tenant_id = $2`,
			c.hash, c.tenantID).Scan(&holder)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // released between insert and read; not ours
		}
		if err != nil {
			return false, fmt.Errorf("claim workflows: concurrency key holder: %w", err)
		}
		return holder == c.id, nil
	}

	// Registered queue: semaphore under the queues lock.
	ql, ok := limits[*c.key]
	if !ok {
		// The candidate predicate saw it registered, but the lock step did not
		// (disabled or deleted between the two statements). Not claimable; the
		// next poll re-evaluates it, this time as a bare key.
		return false, nil
	}
	// Does this workflow already hold a live slot on this queue, and whose
	// worker_id does it carry? cleat#1917 decision 4: a holder owned by the
	// CLAIMING worker is the original re-claim shortcut, unchanged. A holder
	// that exists but belongs to nobody (pre-#1917 row) or to a DIFFERENT
	// worker (a parked run waking and being claimed elsewhere) is not free --
	// it must pass every gate below for the claiming worker, and if admitted,
	// the holder MOVES to it rather than a second row being inserted.
	var existingWorker sql.NullString
	holderExists := true
	err := tx.QueryRowContext(ctx, `
		SELECT worker_id FROM queue_holders
		WHERE tenant_id = $1 AND queue_name = $2 AND workflow_id = $3
	`, c.tenantID, *c.key, c.id).Scan(&existingWorker)
	if errors.Is(err, sql.ErrNoRows) {
		holderExists = false
	} else if err != nil {
		return false, fmt.Errorf("claim workflows: queue self-hold check: %w", err)
	}
	if holderExists && existingWorker.Valid && existingWorker.String == workerID {
		// Re-claim: a run that already holds its own slot on THIS worker claims
		// again after a lost fence, without counting against any limit or
		// taking a new rate token -- it is continuing an admission already
		// granted, not a new one.
		return true, nil
	}
	// A fresh admission, or a holder about to move to this worker. Either way,
	// count OTHER holders: `<> $3` excludes this workflow's own row so a move
	// (which changes no total) is not double-counted against the global cap.
	var held int
	err = tx.QueryRowContext(ctx, `
		SELECT count(*) FROM queue_holders qh
		WHERE qh.tenant_id = $1 AND qh.queue_name = $2 AND qh.workflow_id <> $3
		  AND EXISTS (SELECT 1 FROM workflow_instances wi
		               WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
		                 AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))
	`, c.tenantID, *c.key, c.id).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("claim workflows: count queue holders: %w", err)
	}
	if held >= ql.concurrencyLimit {
		return false, nil // at capacity
	}
	// cleat#1917: the per-worker cap, if declared, is checked under this same
	// lock -- a free global slot does not admit past a worker already at its
	// own cap. Unlike the count above, this one does not need to exclude this
	// workflow's own row: the same-worker case already returned true above, so
	// any existing holder for this workflow belongs to nobody or to a
	// DIFFERENT worker and cannot match `worker_id = workerID`.
	if ql.workerConcurrency != nil {
		var workerHeld int
		err = tx.QueryRowContext(ctx, `
			SELECT count(*) FROM queue_holders qh
			WHERE qh.tenant_id = $1 AND qh.queue_name = $2 AND qh.worker_id = $3
			  AND EXISTS (SELECT 1 FROM workflow_instances wi
			               WHERE wi.id = qh.workflow_id AND wi.tenant_id = qh.tenant_id
			                 AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))
		`, c.tenantID, *c.key, workerID).Scan(&workerHeld)
		if err != nil {
			return false, fmt.Errorf("claim workflows: count worker queue holders: %w", err)
		}
		if workerHeld >= *ql.workerConcurrency {
			return false, nil // this worker is at its own cap
		}
	}
	// cleat#1918: the rate limit, if declared, is checked under this same
	// queues-row lock -- independent of the concurrency and worker checks
	// above. It applies only to a genuinely fresh admission: a holder that
	// already exists consumed its rate token when it was first admitted, and
	// moving it to a new worker is not a new admission.
	if !holderExists && ql.rateLimit != nil {
		var rateHeld int
		err = tx.QueryRowContext(ctx, `
			SELECT count(*) FROM queue_rate_tokens qrt
			WHERE qrt.tenant_id = $1 AND qrt.queue_name = $2 AND qrt.expires_at > now()
		`, c.tenantID, *c.key).Scan(&rateHeld)
		if err != nil {
			return false, fmt.Errorf("claim workflows: count queue rate tokens: %w", err)
		}
		if rateHeld >= *ql.rateLimit {
			return false, nil // rate-limited
		}
	}
	if holderExists {
		// Move: the slot already exists (owned by nobody or by a different
		// worker). Refresh its lease and reassign it to the claiming worker.
		res, err := tx.ExecContext(ctx, `
			UPDATE queue_holders
			SET worker_id = $4, expires_at = now() + make_interval(secs => $5)
			WHERE tenant_id = $1 AND queue_name = $2 AND workflow_id = $3
		`, c.tenantID, *c.key, c.id, workerID, claimedKeyTTL.Seconds())
		if err != nil {
			return false, fmt.Errorf("claim workflows: move queue holder: %w", err)
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO queue_holders (tenant_id, queue_name, workflow_id, expires_at, worker_id)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5)
		ON CONFLICT (tenant_id, queue_name, workflow_id) DO NOTHING
	`, c.tenantID, *c.key, c.id, claimedKeyTTL.Seconds(), workerID)
	if err != nil {
		return false, fmt.Errorf("claim workflows: acquire queue holder: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if ql.rateLimit != nil {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO queue_rate_tokens (tenant_id, queue_name, workflow_id, expires_at)
			VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		`, c.tenantID, *c.key, c.id, *ql.ratePeriodSeconds); err != nil {
			return false, fmt.Errorf("claim workflows: record rate token: %w", err)
		}
	}
	return true, nil
}

// ClaimStickyWorkflows atomically claims up to limit runnable workflow instances
// that are sticky to this worker. Filters on sticky_worker_id to use the
// idx_instances_sticky partial index for low-contention claiming.

func (s *PostgresStore) ClaimStickyWorkflows(ctx context.Context, workerID string, limit int) ([]*WorkflowInstance, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// CTE rather than an IN (SELECT ... LIMIT n) sublink, for consistency with
	// ClaimWorkflows above -- see the note there, including why the
	// EvalPlanQual explanation this comment used to give is wrong.
	rows, err := tx.QueryContext(ctx, `
		WITH candidates AS (
			SELECT id FROM workflow_instances
			WHERE status = 'ready'
			  AND next_wake_at <= now()
			  AND sticky_worker_id = $1
			  AND task_queue = ANY($2)
  AND (workflow_instances.concurrency_key_hash IS NULL
       OR NOT EXISTS (SELECT 1 FROM concurrency_keys ck
                       WHERE (ck.key_hash = workflow_instances.concurrency_key_hash
                         AND ck.tenant_id = workflow_instances.tenant_id
                         AND ck.workflow_id <> workflow_instances.id)
                         AND EXISTS (SELECT 1 FROM workflow_instances wi
                                      WHERE wi.id = ck.workflow_id AND wi.tenant_id = ck.tenant_id
                                        AND wi.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled'))))
			ORDER BY priority ASC, created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE workflow_instances w
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
		    promise_seq_at_claim = promise_seq,
		    assigned_to = $1,
		    heartbeat_at = now(),
		    started_at = COALESCE(started_at, now()),
		    generation = generation + 1
		FROM candidates c
		WHERE w.id = c.id
		RETURNING w.id, w.def_name, w.def_version, w.status, w.input, w.assigned_to, w.next_wake_at, w.tenant_id, w.created_at, w.error_code, w.error_op, w.generation, COALESCE(w.priority, 0) AS priority, COALESCE(w.trace_id, '') AS trace_id
	`, workerID, pq.Array(s.taskQueues), limit)
	if err != nil {
		return nil, fmt.Errorf("claim sticky workflows: %w", err)
	}
	defer rows.Close()

	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt sql.NullTime
		var tenantID sql.NullString
		var createdAt sql.NullTime
		var errorCode, errorOp sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp, &wf.Generation, &wf.Priority, &wf.TraceID); err != nil {
			return nil, fmt.Errorf("claim sticky workflows scan: %w", err)
		}

		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim sticky workflows rows: %w", err)
	}

	if len(wfs) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}
	// No separate "winning ids" slice exists here the way ClaimWorkflows has
	// one -- the CTE decided membership and RETURNING already reported it,
	// so wfs IS the winning set. Collecting ids from it rather than
	// re-querying candidates keeps this agreeing with the UPDATE that just
	// ran, by construction.
	ids := make([]string, len(wfs))
	for i, wf := range wfs {
		ids[i] = wf.ID
	}
	if err := s.claimLeaseRows(ctx, tx, workerID, ids); err != nil {
		return nil, fmt.Errorf("claim sticky workflows: %w", err)
	}
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
}

// LoadEventHistory returns all event records for a workflow, ordered by step.

func (s *PostgresStore) ContinueAsNew(ctx context.Context, currentRunID, workerID string, generation int64, defName string, defVersion int, newInput json.RawMessage, newEvents []EventRecord, result string, queryState map[string]string, priority int) (string, error) {
	// Coerce, as FinalizeWorkflowSegment does. The result column is jsonb on
	// PostgreSQL and JSON on MySQL, and the raw string is not guaranteed to be
	// either -- a workflow that continues as new never returned a value, so the
	// result here is "", which is not valid JSON. Writing it raw failed the
	// whole run with
	//
	//	pq: invalid input syntax for type json (22P02)
	//
	// so continue-as-new did not work at all on PostgreSQL. coerceResultJSON
	// existed for exactly this and was called from one path out of three.
	resultJSON := coerceResultJSON(ctx, s.log(), currentRunID, result)

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", fmt.Errorf("continue as new: begin: %w", err)
	}
	defer tx.Rollback()

	// Append events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, currentRunID, newEvents); err != nil {
		return "", fmt.Errorf("continue as new: append events: %w", err)
	}

	// cleat#2312: the new run's own input is sensitive-at-rest, same as the
	// old run's result and query_state below -- one derivation covers all
	// three, since they belong to the same tenant and the same call.
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return "", fmt.Errorf("continue as new: derive tenant key: %w", err)
	}
	sealedNewInput, err := encryptJSONColumnForStorage(string(newInput), tc)
	if err != nil {
		return "", fmt.Errorf("continue as new: encrypt input: %w", err)
	}

	// Create the new workflow run.
	// Use the store's tenant scope to preserve tenant isolation.
	var newRunID, resolvedTaskQueue string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at, continued_from, parent_workflow_id, parent_close_policy)
		-- parent_workflow_id and parent_close_policy are INHERITED from the run
		-- being continued, not left NULL. A child that continues as new is
		-- still its parent's child: enforceParentClosePolicy selects on
		-- parent_workflow_id and NULL matches nothing, so a TERMINATE parent
		-- left every continued iteration running while reporting that it had
		-- stopped its child (cleat#955). Measured against a plain sibling
		-- child as a control -- that one WAS stopped by the same call, so the
		-- policy was working and only the link was missing.
		VALUES (gen_random_uuid(), $1, $2, 'ready', $3,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $1 AND version = $2 AND tenant_id = $4), 'default'),
			$4, $5, now() - INTERVAL '1 millisecond', $6,
			(SELECT parent_workflow_id FROM workflow_instances WHERE id = $6),
			(SELECT parent_close_policy FROM workflow_instances WHERE id = $6))
		RETURNING id, task_queue
		`, defName, defVersion, sealedNewInput, s.tenantID, priority, currentRunID).Scan(&newRunID, &resolvedTaskQueue)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// cleat#3245 Phase 3 step 2 piece 4d: create the new run's
	// workflow_leases/workflow_payloads rows, same as startNewRun (piece
	// 1) -- resolvedTaskQueue is captured via RETURNING rather than
	// re-evaluated, for the identical reason startNewRun's own call does.
	if err := s.insertLeaseAndPayloadRows(ctx, tx, newRunID, s.tenantID, resolvedTaskQueue, priority, sealedNewInput); err != nil {
		return "", fmt.Errorf("continue as new: %w", err)
	}

	// Complete the current run.
	qsJSON := marshalQueryState(queryState)
	sealedResult, err := encryptJSONColumnForStorage(resultJSON, tc)
	if err != nil {
		return "", fmt.Errorf("continue as new: encrypt result: %w", err)
	}
	sealedQS, err := encryptJSONColumnForStorage(string(qsJSON), tc)
	if err != nil {
		return "", fmt.Errorf("continue as new: encrypt query_state: %w", err)
	}
	// `assigned_to = NULL` is the exclusion, not the WHERE clause.
	//
	// Nothing here bumps `generation`. Claiming does, so `generation = $5`
	// excludes a caller from an EARLIER claim and `assigned_to = $2` excludes a
	// DIFFERENT worker -- but two callers holding the SAME claim are
	// indistinguishable to both. What refuses the second is that the first set
	// assigned_to to NULL, so `assigned_to = $2` no longer matches.
	//
	// Measured, not asserted: delete the generation predicate and
	// TestASecondContinueAsNewOnTheSameClaimIsRefused_MultiBackend still
	// passes; keep assigned_to instead of NULLing it and the second call
	// SUCCEEDS, leaving the predecessor with two successors -- a forked chain.
	//
	// That second mutation is the cheapest implementation of decision 4, a
	// durable record of which worker ran a workflow. CLAUDE.md 3.112 records
	// the same clause doing the same unnamed job in the finalize path, where a
	// test named for the marker predicate stayed green with that predicate
	// deleted. cleat#1175.
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = $3, completed_at = now(), completed_by = assigned_to, assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2 AND generation = $5
	`, currentRunID, workerID, sealedResult, sealedQS, generation)
	if err != nil {
		return "", fmt.Errorf("continue as new: complete old run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("continue as new: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: this also discards the new run row we just inserted, so
		// a lost fence leaves no orphaned, unreachable continuation run
		// behind.
		return "", ErrFenceLost
	}

	// cleat#3245 Phase 3 step 2 piece 4d: mirror the old run's completion
	// onto workflow_leases/workflow_payloads, same shape as CompleteWorkflow
	// (piece 4a) -- the same two helpers, same fence.
	if err := s.completeLeaseRow(ctx, tx, currentRunID, workerID, "done", generation); err != nil {
		return "", fmt.Errorf("continue as new: %w", err)
	}
	if err := s.writeResultPayload(ctx, tx, currentRunID, sealedResult, sealedQS); err != nil {
		return "", fmt.Errorf("continue as new: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	releaseWorkflowResources(s.log(), s, currentRunID)
	s.enforceParentClosePolicy(context.Background(), currentRunID,
		fmt.Sprintf("parent continued as new (run %s)", newRunID))

	return newRunID, nil
}

// FinalizeWorkflowSegment atomically appends new events and updates the
// workflow status in a single database transaction.  This eliminates the
// race between AppendEventHistoryBatch and the subsequent CompleteWorkflow /
// FailWorkflow / ReleaseWorkflow call.
//
// finalStatus must be one of:
//   - "done"  — marks the workflow as completed with the given result
//   - "ready" — returns the workflow to the ready queue (suspend)
//
// There is no "failed" here. A real failure goes through FailWorkflow, not
// this method or the finalize_workflow_status procedure it calls -- cleat#1973.
//
// Fields not relevant to the chosen status are ignored.

func (s *PostgresStore) finalizeWorkflowSegmentInner(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	if !validFinalStatus(finalStatus) {
		return fmt.Errorf("finalize workflow: unknown final status: %s", finalStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finalize workflow: begin tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.setRLSOnTx(tx); err != nil {
		return fmt.Errorf("finalize workflow: set rls: %w", err)
	}

	// Append new events within the same transaction.
	if err := s.appendEventsInTx(ctx, tx, runID, newEvents); err != nil {
		return fmt.Errorf("finalize workflow: append events: %w", err)
	}

	// Delegate the terminal UPDATEs (status, idempotency, parent wake,
	// await_child population, pg_notify) to a server-side PL/pgSQL function.
	// This replaces 5 individual round-trips with 1 function call.
	qsJSON := marshalQueryState(queryState)
	resultJSON := coerceResultJSON(ctx, s.log(), runID, result)

	// cleat#2312: result and query_state are the only two of this
	// procedure's arguments it actually persists (finalize_workflow_status
	// writes p_result and p_query_state; p_error_code/p_error_op are passed
	// but unused for 'done'/'ready' -- a real failure never reaches this
	// procedure at all, see this function's own doc comment), so those are
	// the only two sealed here.
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return fmt.Errorf("finalize workflow: derive tenant key: %w", err)
	}
	sealedResult, err := encryptJSONColumnForStorage(resultJSON, tc)
	if err != nil {
		return fmt.Errorf("finalize workflow: encrypt result: %w", err)
	}
	sealedQS, err := encryptJSONColumnForStorage(string(qsJSON), tc)
	if err != nil {
		return fmt.Errorf("finalize workflow: encrypt query_state: %w", err)
	}

	var fenceHeld bool
	if err := tx.QueryRowContext(ctx, `
		SELECT finalize_workflow_status($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, runID, workerID, generation, finalStatus, sealedResult, errorCode, errorOp, sealedQS, nextWakeAt, s.notifyChannel).Scan(&fenceHeld); err != nil {
		return fmt.Errorf("finalize workflow: %w", err)
	}

	if !fenceHeld {
		// Another worker now owns this workflow (e.g. this worker stalled,
		// was reaped, and the workflow was reclaimed). Roll back rather
		// than commit: the events we just appended belong to a segment
		// that is no longer valid, and none of the post-commit cleanup
		// below is safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Best-effort cleanup for terminal statuses (post-commit).
	if finalStatus == "done" || finalStatus == "failed" {
		releaseWorkflowResources(s.log(), s, runID)
		s.enforceParentClosePolicy(context.Background(), runID, parentOutcomeMessage(finalStatus))
	}

	return nil
}

// validFinalStatus returns true for status values accepted by finalize_workflow_status.
//
// No "failed" here: the procedure's 'failed' arm was dead code -- nothing
// ever called it that way -- and was removed in migration
// .../101_the_finalize_procedure_stops_deleting_failed_history.sql
// (cleat#1973). A real failure goes through FailWorkflow instead.
func validFinalStatus(status string) bool {
	switch status {
	case "done", "ready", "suspended":
		return true
	}
	return false
}

// AppendEventHistory appends a single event to the history.

func (s *PostgresStore) CompleteWorkflow(ctx context.Context, workflowID, workerID string, generation int64, result string, queryState map[string]string) error {
	// Coerce, as FinalizeWorkflowSegment does. The result column is jsonb on
	// PostgreSQL and JSON on MySQL, and the raw string is not guaranteed to be
	// either -- a workflow that continues as new never returned a value, so the
	// result here is "", which is not valid JSON. Writing it raw failed the
	// whole run with
	//
	//	pq: invalid input syntax for type json (22P02)
	//
	// so continue-as-new did not work at all on PostgreSQL. coerceResultJSON
	// existed for exactly this and was called from one path out of three.
	resultJSON := coerceResultJSON(ctx, s.log(), workflowID, result)

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("complete workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON := marshalQueryState(queryState)

	// cleat#2312: result and query_state are both sensitive-at-rest.
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return fmt.Errorf("complete workflow: derive tenant key: %w", err)
	}
	sealedResult, err := encryptJSONColumnForStorage(resultJSON, tc)
	if err != nil {
		return fmt.Errorf("complete workflow: encrypt result: %w", err)
	}
	sealedQS, err := encryptJSONColumnForStorage(string(qsJSON), tc)
	if err != nil {
		return fmt.Errorf("complete workflow: encrypt query_state: %w", err)
	}

	// `assigned_to = NULL` is the exclusion, not the WHERE clause.
	//
	// Nothing here bumps `generation`. Claiming does, so `generation = $5`
	// excludes a caller from an EARLIER claim and `assigned_to = $2` excludes a
	// DIFFERENT worker -- but two callers holding the SAME claim are
	// indistinguishable to both. What refuses the second is that the first set
	// assigned_to to NULL, so `assigned_to = $2` no longer matches.
	//
	// Measured, not asserted: delete the generation predicate and
	// TestASecondContinueAsNewOnTheSameClaimIsRefused_MultiBackend still
	// passes; keep assigned_to instead of NULLing it and the second call
	// SUCCEEDS, leaving the predecessor with two successors -- a forked chain.
	//
	// That second mutation is the cheapest implementation of decision 4, a
	// durable record of which worker ran a workflow. CLAUDE.md 3.112 records
	// the same clause doing the same unnamed job in the finalize path, where a
	// test named for the marker predicate stayed green with that predicate
	// deleted. cleat#1175.
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'done', result = $3, completed_at = now(), completed_by = assigned_to, assigned_to = NULL, query_state = $4
		WHERE id = $1 AND assigned_to = $2 AND generation = $5
	`, workflowID, workerID, sealedResult, sealedQS, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete workflow: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the post-commit cleanup below is not safe to run on the
		// new owner's behalf.
		return ErrFenceLost
	}

	// cleat#3245 Phase 3 step 2 piece 4a: mirror the same terminal
	// transition onto workflow_leases/workflow_payloads, same tx.
	if err := s.completeLeaseRow(ctx, tx, workflowID, workerID, "done", generation); err != nil {
		return fmt.Errorf("complete workflow: %w", err)
	}
	if err := s.writeResultPayload(ctx, tx, workflowID, sealedResult, sealedQS); err != nil {
		return fmt.Errorf("complete workflow: %w", err)
	}

	// No idempotency write on the success path. idempotency_keys.result was
	// written here and read nowhere, so cleat#1049 dropped the column; a
	// completed run now records nothing on that table. The failure path is
	// unchanged -- FailWorkflow and MoveToDeadLetterQueue still write
	// error_msg, still filtered `AND tenant_id`, which is where the Finding
	// S1 tenant-scope guard now lives.

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID, parentOutcomeMessage(statusDone))

	return nil
}

// FailWorkflow marks a workflow as failed.

func (s *PostgresStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("fail workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsParam := queryStateUpdateParam(queryState)

	// cleat#2312: error_msg, error_code, error_op and query_state are all
	// sensitive-at-rest; one tenant cipher derivation covers all four (and
	// the idempotency_keys.error_msg write below, which reuses the same
	// ciphertext rather than sealing the message a second time).
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return fmt.Errorf("fail workflow: derive tenant key: %w", err)
	}
	sealedErrorMsg, err := encryptTextColumnForStorage(errorMsg, tc)
	if err != nil {
		return fmt.Errorf("fail workflow: encrypt error_msg: %w", err)
	}
	sealedErrorCode, err := encryptTextColumnForStorage(errorCode, tc)
	if err != nil {
		return fmt.Errorf("fail workflow: encrypt error_code: %w", err)
	}
	sealedErrorOp, err := encryptTextColumnForStorage(errorOp, tc)
	if err != nil {
		return fmt.Errorf("fail workflow: encrypt error_op: %w", err)
	}
	if qs, ok := qsParam.(string); ok {
		sealedQS, err := encryptJSONColumnForStorage(qs, tc)
		if err != nil {
			return fmt.Errorf("fail workflow: encrypt query_state: %w", err)
		}
		qsParam = sealedQS
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = $3,
		    error_code = $4,
		    error_op = $5,
		    completed_at = now(),
		    completed_by = assigned_to, assigned_to = NULL,
		    query_state = COALESCE($6::jsonb, query_state)
		WHERE id = $1 AND assigned_to = $2 AND generation = $7
	`, workflowID, workerID, sealedErrorMsg, sealedErrorCode, sealedErrorOp, qsParam, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("fail workflow: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// cleat#3245 Phase 3 step 2 piece 4a: mirror the same terminal
	// transition onto workflow_leases/workflow_payloads, same tx.
	if err := s.completeLeaseRow(ctx, tx, workflowID, workerID, "failed", generation); err != nil {
		return fmt.Errorf("fail workflow: %w", err)
	}
	if err := s.writeErrorPayload(ctx, tx, workflowID, sealedErrorMsg, sealedErrorCode, sealedErrorOp, qsParam); err != nil {
		return fmt.Errorf("fail workflow: %w", err)
	}

	// Record idempotency error within the transaction (best-effort). Reuses
	// sealedErrorMsg rather than sealing errorMsg a second time -- both
	// columns hold the same plaintext, and AES-GCM's random nonce means a
	// second seal would produce different-looking ciphertext for no benefit.
	//
	// AND tenant_id = $3: see the identical comment on the sibling UPDATE in
	// CompleteWorkflow. s.tenantID is already set on this tx by
	// beginTxWithRLS.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1 AND tenant_id = $3`,
		workflowID, sealedErrorMsg, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID, parentOutcomeMessage(statusFailed))

	return nil
}

// enforceParentClosePolicy applies ParentClosePolicy to all child workflows
// of the given parent workflow. Best-effort post-commit cleanup.

// enforceParentClosePolicy applies a closing parent's policy to its children.
//
// It used to discard every error it produced: neither ExecContext's nor
// Commit's return value was assigned, in either transaction. When it failed,
// the children of a closed parent were simply unaffected by its policy --
// TERMINATE children kept running, REQUEST_CANCEL children were never
// flagged -- with no log line, no metric and no error anywhere. The function
// is void and its callers treat it as best-effort post-commit cleanup, so
// nothing downstream noticed either. See IMPROVEMENT-PLAN.md 2.50.
//
// It stays void: the contract with callers has not changed, only whether a
// failure is observable.
//
// outcomeMsg names what happened to the closing parent -- cleat#1978. It
// becomes the TERMINATE child's error_msg, replacing the hardcoded "parent
// workflow terminated" that used to run regardless of why the parent
// actually closed (completion, failure, dead-lettering, an operator's
// TerminateWorkflow call, or continue-as-new all reach here). Build it with
// parentOutcomeMessage.
func (s *PostgresStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID, outcomeMsg string) {
	s.enforceParentClosePolicyAt(ctx, parentWorkflowID, 0, outcomeMsg)
}

// parentOutcomeMessage describes, for a TERMINATE child's error_msg, what
// happened to the parent that closed it -- cleat#1978. Must stay in sync with
// the five terminal values workflow_instances.status actually takes; see
// enforceParentClosePolicyAt's own census of them, a few lines below.
func parentOutcomeMessage(parentStatus string) string {
	switch parentStatus {
	case statusDone:
		return "parent workflow completed"
	case statusFailed:
		return "parent workflow failed"
	case statusDeadLettered:
		return "parent workflow was dead-lettered"
	case statusTerminated:
		return "parent workflow was terminated"
	case statusCancelled:
		return "parent workflow was cancelled"
	default:
		return "parent workflow closed (status " + parentStatus + ")"
	}
}

// enforceParentClosePolicyAt is enforceParentClosePolicy with the recursion
// depth carried explicitly. See cascadeIntoClosedChildren.
func (s *PostgresStore) enforceParentClosePolicyAt(ctx context.Context, parentWorkflowID string, depth int, outcomeMsg string) {
	steps := []struct {
		policy string
		query  string
		args   []any
	}{
		// Both arms fence the child out. `generation = generation + 1` and
		// `assigned_to = NULL` are not bookkeeping: without them a child that a
		// worker is currently holding overwrites its own termination. The
		// worker's fence is (assigned_to, generation), finalize_workflow_status
		// checks it, and an UPDATE that changes only the status leaves that
		// fence valid -- so the next FinalizeWorkflowSegment matches, sets the
		// child back to 'ready' or on to 'done', and the termination is gone.
		// The error_msg survives, because the 'done' branch does not clear it,
		// leaving a row that says status='done' AND 'parent workflow
		// terminated'. Measured 2026-09-06 before this line existed: 4 runs of
		// 4, every TERMINATE child completed anyway carrying that message.
		//
		// The defer-phase arm below always had the bump, for the same reason
		// ExpireDeferPhases has it. Only this arm lacked it -- and this is the
		// arm most children take, since it is the one for children that owe no
		// cleanup.
		//
		// Two TERMINATE arms, and the predicate is what splits them:
		// a child that owes cleanup goes to 'terminating' with the outcome
		// recorded, and is failed later by FinalizeDeferPhase once its
		// defers have run. IMPROVEMENT-PLAN 3.114.
		//
		// `AND NOT` here rather than a status filter, so the two arms
		// partition the children exactly. deferPhaseOwedSQL is never NULL --
		// it is an IN over a NOT NULL column ANDed with an IS NOT NULL and an
		// EXISTS -- so NOT is total and no child falls between them.
		// WHAT COUNTS AS ALREADY-TERMINAL HERE, and why the list is five values
		// rather than two.
		//
		// It was `NOT IN ('done', 'failed')`, and the engine writes three more
		// terminal statuses than that: 'dead_lettered', 'terminated' and
		// 'cancelled'. So a dead-lettered child MATCHED, and a parent closing with
		// TERMINATE overwrote it (cleat#1227, measured on all three dialects):
		//
		//	BEFORE  status=dead_lettered  error_msg="retries exhausted"           error_code=E_RETRY
		//	AFTER   status=failed         error_msg="parent workflow terminated"  error_code=E_RETRY
		//
		// (That AFTER line is what this arm wrote before cleat#1978; TERMINATE
		// writes 'terminated' now, not 'failed' -- see the SET clause below. The
		// lesson the example carries is unchanged: an incomplete exclusion list
		// lets a settled child's status get overwritten at all.)
		//
		// Three losses in one UPDATE, as it stood then. The run left the
		// dead-letter queue; its original failure reason was replaced; and
		// error_code was not in the SET list, so the surviving row reported TWO
		// DIFFERENT CAUSES at once. That last part is what made it worse than a
		// plain overwrite -- nothing about the result looked wrong.
		//
		// 'terminating' is deliberately NOT here. A child mid-shutdown is not
		// terminal, and the two arms below split on deferPhaseOwedSQL precisely to
		// give it a defer phase rather than a terminal write.
		//
		// 'cancelled' IS one of the five values in the NOT IN list two lines below
		// -- CancelWorkflow (preemptivelySettle) writes it to
		// workflow_instances.status, exactly as TerminateWorkflow writes
		// 'terminated'. This comment claimed the opposite ("nothing writes it") for
		// months, directly contradicted by the SQL it sat above and by
		// CancelWorkflow's own doc comment in db.go -- found stale while building
		// cleat#1997's model of every status writer, and by cleat-review's
		// independent read of the same code the same day. There is no CHECK
		// constraint to consult, so the vocabulary has to come from what
		// production actually WRITES, and the whole of it is eight values:
		//
		//	cancelled  dead_lettered  done  failed  ready  running  terminated  terminating
		//
		// 'suspended' is the sharpest illustration of a name that is NOT one of them.
		// Thirty-five predicates read `status IN ('ready', 'suspended')` and no
		// statement anywhere sets it -- a suspension is written as 'ready' with a
		// next_wake_at, so those predicates are correct and merely carry a dead
		// branch. A name can be part of this codebase's vocabulary without ever
		// being part of its data, and with no CHECK constraint nothing catches
		// that. 'completed', 'pending', 'rejected' and 'resolved' are the same
		// shape from the other side: real statuses, of promises and schedules,
		// not of this table.
		//
		// REQUEST_CANCEL gets the same predicate though it overwrites nothing --
		// it only sets cancellation_requested. Setting that flag on a run that has
		// already finished is not data loss today, but it leaves a latent
		// instruction on a row a reprocess could pick up, and one rule stated once
		// is what stops the four-way split cleat#1227 is really about.
		{"TERMINATE", `
		UPDATE workflow_instances
		SET status = 'terminated', error_msg = $2, error_op = 'parent_close', error_code = NULL,
		    pending_terminal_status = NULL, defer_phase_deadline = NULL,
		    completed_at = now(),
		    completed_by = assigned_to, assigned_to = NULL, generation = generation + 1
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND NOT ` + deferPhaseOwedSQL + `
	`, []any{parentWorkflowID, outcomeMsg}},
		{"TERMINATE (defer phase)", `
		UPDATE workflow_instances
		SET status = '` + statusTerminating + `',
		    pending_terminal_status = 'terminated',
		    defer_phase_deadline = ` + deferPhaseDeadlinePostgres + `,
		    error_msg = $2, error_op = 'parent_close', error_code = NULL,
		    next_wake_at = now(),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND ` + deferPhaseOwedSQL + `
	`, []any{parentWorkflowID, outcomeMsg}},
		{"REQUEST_CANCEL", `
		UPDATE workflow_instances
		SET cancellation_requested = true
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
	`, []any{parentWorkflowID}},
	}

	// Collected before the UPDATE: see releaseTerminatedChildren for why not
	// RETURNING. A failure here costs the release, not the close policy.
	terminated, err := s.childrenClosedByTerminate(ctx, parentWorkflowID)
	if err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: could not list TERMINATE children; their concurrency keys and sticky-worker assignments stay held until TTL",
			"parent_workflow_id", parentWorkflowID, "error", err)
	}

	for _, step := range steps {
		if err := s.runParentClosePolicyStep(ctx, step.query, step.args...); err != nil {
			s.log().WarnContext(ctx, "enforceParentClosePolicy failed; children of a closed parent are unaffected by its close policy",
				"policy", step.policy, "parent_workflow_id", parentWorkflowID, "error", err)
			if step.policy == "TERMINATE" {
				terminated = nil
			}
		}
	}

	releaseTerminatedChildren(s.log(), s, terminated)
	// A child this cascade just closed was, by construction, TERMINATEd --
	// the plain TERMINATE arm's children are the only ones passed here (see
	// this function's own doc comment on cascadeIntoClosedChildren's caller).
	// So its own outcome, for ITS children's error_msg, is always "parent
	// workflow was terminated" -- not outcomeMsg, which describes the ROOT
	// parent's outcome and would be wrong for every level below it.
	childOutcomeMsg := parentOutcomeMessage(statusTerminated)
	cascadeIntoClosedChildren(s.log(), depth, terminated, func(id string, d int) {
		s.enforceParentClosePolicyAt(ctx, id, d, childOutcomeMsg)
	})
}

// terminateChildrenQuery selects the children the TERMINATE arm is about to
// fail, so their resources can be released after it commits. Its WHERE must
// stay identical to that UPDATE's, or the two disagree about which children
// were closed.
//
// Which is why it carries `AND NOT deferPhaseOwedSQL` too, and why that matters
// more than the symmetry: a child entering a defer phase is NOT terminal yet,
// and releasing its concurrency keys here would be the exact pre-emption
// IMPROVEMENT-PLAN 3.112 removed from TerminateWorkflow -- the host dropping
// the resource before the defer that releases it has run. Those children are
// released by FinalizeDeferPhase instead.
func (s *PostgresStore) childrenClosedByTerminate(ctx context.Context, parentWorkflowID string) ([]string, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM workflow_instances
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND NOT `+deferPhaseOwedSQL+`
	`, parentWorkflowID)
	if err != nil {
		return nil, err
	}
	return scanWorkflowIDs(rows)
}

func (s *PostgresStore) runParentClosePolicyStep(ctx context.Context, query string, args ...any) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts.

func (s *PostgresStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	qsParam := queryStateUpdateParam(queryState)

	// cleat#2312: see the identical derivation in FailWorkflow.
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return fmt.Errorf("move to dead letter queue: derive tenant key: %w", err)
	}
	sealedErrMsg, err := encryptTextColumnForStorage(errMsg, tc)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: encrypt error_msg: %w", err)
	}
	sealedErrorCode, err := encryptTextColumnForStorage(errorCode, tc)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: encrypt error_code: %w", err)
	}
	sealedErrorOp, err := encryptTextColumnForStorage(errorOp, tc)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: encrypt error_op: %w", err)
	}
	if qs, ok := qsParam.(string); ok {
		sealedQS, err := encryptJSONColumnForStorage(qs, tc)
		if err != nil {
			return fmt.Errorf("move to dead letter queue: encrypt query_state: %w", err)
		}
		qsParam = sealedQS
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = $3, error_code = $4, error_op = $5,
		    completed_at = now(), completed_by = assigned_to, assigned_to = NULL,
		    query_state = COALESCE($6::jsonb, query_state)
		WHERE id = $1 AND assigned_to = $2 AND generation = $7
	`, workflowID, workerID, sealedErrMsg, sealedErrorCode, sealedErrorOp, qsParam, generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("move to dead letter queue: rows affected: %w", err)
	}
	if n == 0 {
		// Another worker now owns this workflow. Roll back rather than
		// commit: the idempotency-key write and post-commit cleanup below
		// are not safe to run on the new owner's behalf.
		return ErrFenceLost
	}

	// cleat#3245 Phase 3 step 2 piece 4a: mirror the same terminal
	// transition onto workflow_leases/workflow_payloads, same tx.
	if err := s.completeLeaseRow(ctx, tx, workflowID, workerID, "dead_lettered", generation); err != nil {
		return fmt.Errorf("move to dead letter queue: %w", err)
	}
	if err := s.writeErrorPayload(ctx, tx, workflowID, sealedErrMsg, sealedErrorCode, sealedErrorOp, qsParam); err != nil {
		return fmt.Errorf("move to dead letter queue: %w", err)
	}

	// Record idempotency error within the transaction (best-effort). Reuses
	// sealedErrMsg -- see the identical comment on FailWorkflow's sibling
	// write.
	//
	// AND tenant_id = $3: see the identical comment on the sibling UPDATE in
	// CompleteWorkflow. s.tenantID is already set on this tx by
	// beginTxWithRLS.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1 AND tenant_id = $3`,
		workflowID, sealedErrMsg, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID, parentOutcomeMessage(statusDeadLettered))

	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to a runnable state.

func (s *PostgresStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("retry workflow: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', completed_by = assigned_to, assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = now()
		WHERE id = $1 AND status = 'dead_lettered'
	`, workflowID)
	if err != nil {
		return err
	}

	// cleat#3245 Phase 3 step 2 piece 4c: mirror the same transition onto
	// workflow_leases/workflow_payloads, same tx, gated on the instances
	// UPDATE above having actually matched -- RetryWorkflow's original
	// behaviour (a no-op, nil-returning call on a non-dead_lettered or
	// absent id) is unchanged either way, so this gate only decides
	// whether the mirror runs, never the function's own return value.
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workflow_leases
			SET status = 'ready', completed_by = assigned_to, assigned_to = NULL, heartbeat_at = NULL, next_wake_at = now()
			WHERE id = $1 AND status = 'dead_lettered'
		`, workflowID); err != nil {
			return fmt.Errorf("retry workflow: mirror lease: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE workflow_payloads SET error_msg = NULL, error_code = NULL, error_op = NULL WHERE id = $1
		`, workflowID); err != nil {
			return fmt.Errorf("retry workflow: mirror payload: %w", err)
		}
	}

	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// ReleaseWorkflow returns a workflow to the queue with a next wake time.
//
// This is the other end of the pair described on ReapStaleInstances, and the
// pairing is invisible from either side alone. The row this writes -- 'ready'
// or 'terminating', assigned_to NULL, next_wake_at set -- is UNREACHABLE BY THE
// REAPER, which matches status='running'. That is correct: a parked workflow
// has no owner, so losing the worker that parked it costs nothing and there is
// nothing to reclaim. It resumes through the ordinary claim predicate at its
// wake time, whichever worker gets there.
//
// Worth stating because it is routinely mistaken for crash recovery. A test
// that kills a worker while its workflow is mid-DurableSleep is not exercising
// the reaper at all -- the workflow was already parked and unowned before the
// kill, and it comes back when the sleep expires (cleat#1429).

func (s *PostgresStore) ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("release workflow: begin: %w", err)
	}
	defer tx.Rollback()

	// Same CASE as ReapStaleInstances, for the same reason: a workflow whose
	// terminal outcome is already recorded is not runnable work, and a release
	// that called it 'ready' would undo the distinction D6 created the
	// 'terminating' status to make. Either status is claimable, so the phase
	// runs again either way -- this is about the status telling the truth
	// while it waits.
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, next_wake_at = $3
		WHERE id = $1 AND assigned_to = $2 AND generation = $4
	`, workflowID, workerID, nextWakeAt, generation)
	if err != nil {
		return err
	}

	// A zero-row update is a lost fence, not a failure and not a success.
	// Reported rather than discarded because the caller branches on it:
	// cmd/cleat-worker's releaseWorkflow treats ErrFenceLost as "the no-op it
	// is" and logs at Debug, while any OTHER error is logged as "release
	// failed, workflow stays claimed until its lease expires" -- untrue of a
	// stale release on both counts. Until cleat#1223 that branch was dead on
	// PostgreSQL and MySQL, and the three sibling fenced writes (Complete,
	// Fail, Finalize) already reported a lost fence this way on all three
	// dialects.
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release workflow: rows affected: %w", err)
	}
	if rows == 0 {
		return ErrFenceLost
	}

	// cleat#3245 Phase 3 step 2, piece 2 (dual-write). Same fence
	// (assigned_to, generation) as the workflow_instances UPDATE above, on
	// the same tx -- this only runs once that UPDATE has already confirmed
	// the fence held, so the two tables cannot disagree about WHETHER the
	// release happened.
	//
	// Upgraded from an unconditional 'ready' to the real CASE WHEN
	// pending_terminal_status IS NOT NULL ... expression by piece 6c, which
	// is the piece that starts dual-writing pending_terminal_status
	// (TerminateWorkflow/CancelWorkflow's two-phase arm, adminForceMark's
	// defer-owed arm). Before piece 6c, workflow_leases.pending_terminal_status
	// was NULL on every row, so this CASE would always have taken the ELSE
	// branch -- unconditional 'ready' with extra text that made it look
	// conditional, which is why it was written as unconditional 'ready'
	// until now rather than as a CASE that could not yet be correct.
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, next_wake_at = $3
		WHERE id = $1 AND assigned_to = $2 AND generation = $4
	`, workflowID, workerID, nextWakeAt, generation); err != nil {
		return fmt.Errorf("release workflow: update lease: %w", err)
	}

	pgNotify(ctx, tx, s.notifyChannel)
	return tx.Commit()
}

// RequestCancellation sets the cancellation flag.

// StartNewRun is the ConcurrencyKeyStore-free entry point: no concurrency key.
func (s *PostgresStore) StartNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, StartOptions{})
}

// StartNewRunWithConcurrencyKey records the key the run wants ON THE ROW, in
// the same INSERT that creates it.
//
// cleat#1186. The key has to be written by the insert rather than by a second
// statement afterwards: the row is created 'ready' with next_wake_at already
// in the past, so a poller can claim it between the two -- and a run claimed
// before its key is recorded is exactly the case the claim predicate exists to
// prevent. There is no window here because there is no second statement.
//
// NOT on the WorkflowStore interface, deliberately. StartNewRun has four real
// implementations and NINE test doubles; adding a parameter would edit all
// thirteen for a field twelve of them do not care about. The HTTP layer asks
// for this through an optional interface assertion, as it already does for
// CountRunnableWorkflows and GetConcurrencyKeyHolder.
func (s *PostgresStore) StartNewRunWithConcurrencyKey(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, concurrencyKey string) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, StartOptions{ConcurrencyKey: concurrencyKey})
}

// StartNewRunWithOptions records every per-run value a start can set, in the
// INSERT that creates the run. See StartOptions for why the two suffixed
// methods collapsed into one.
func (s *PostgresStore) StartNewRunWithOptions(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, opts StartOptions) (string, bool, error) {
	return s.startNewRun(ctx, runID, defName, defVersion, input, idempotencyKey, tenantID, priority, opts)
}

func (s *PostgresStore) startNewRun(ctx context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey string, tenantID string, priority int, opts StartOptions) (string, bool, error) {
	concurrencyKey := opts.ConcurrencyKey
	runInstanceMs := msOrNil(opts.RunLimits.WasmInstanceTimeout)
	runWallClockMs := msOrNil(opts.RunLimits.WasmWallClockCeiling)
	runRetryMs := msOrNil(opts.RunLimits.HostRetryBudget)
	runMaxWorkflowMs := msOrNil(opts.RunLimits.MaxWorkflowDuration)
	if runID == "" {
		runID = uuid.New().String()
	}

	// cleat#2312: input is sensitive-at-rest. Sealed into a separate variable
	// -- never input itself -- so IdempotencyInputDigest above and the
	// ON CONFLICT re-read path below keep working against the plaintext;
	// only the two INSERTs further down bind the sealed copy.
	tc, err := s.tenantCipherForWrite()
	if err != nil {
		return "", false, fmt.Errorf("start new run: derive tenant key: %w", err)
	}
	sealedInput, err := encryptJSONColumnForStorage(string(input), tc)
	if err != nil {
		return "", false, fmt.Errorf("start new run: encrypt input: %w", err)
	}

	if idempotencyKey != "" {
		keyHash := sha256.Sum256([]byte(idempotencyKey))
		inputDigest := IdempotencyInputDigest(input)

		// EVERY statement against idempotency_keys below runs on a transaction
		// with the tenant established, the two lookups included. They ran on
		// the pool until cleat#1534, which is correct for a table with no
		// policy and unable to run at all once it has one: a policy's USING is
		// evaluated per candidate row and cleat.assert_tenant_set() raises
		// there, so the `AND tenant_id = $2` these statements already carry is
		// not what scopes them. Nothing about the statements changes; where
		// they run does.
		//
		// This adds no precondition. The no-key path at the bottom of this
		// function has always opened with beginTxWithRLS, so a start already
		// required a tenant on the majority of its calls; presenting an
		// Idempotency-Key was the way to reach the database without one.
		tx, err := s.beginTxWithRLS(ctx)
		if err != nil {
			return "", false, fmt.Errorf("start new run: begin: %w", err)
		}
		// The rollback covers the early returns below as well as the failure
		// paths. An explicit tx.Rollback() still precedes the concurrent-insert
		// re-read, where releasing the lock immediately is the point rather
		// than a tidy-up.
		defer tx.Rollback()

		// Check for existing idempotency key, within this tenant.
		//
		// The tenant filter is not defence in depth: idempotency_keys was
		// keyed by key_hash alone, so an Idempotency-Key was global across
		// every tenant in the deployment. Two customers both choosing
		// "order-123" collided, and the second was handed the first's
		// workflow ID with alreadyExisted = true while its own workflow was
		// never started. The key is a client-supplied request header, so that
		// is the expected outcome of ordinary naming rather than an attack.
		// migrations/*/010_idempotency_keys_tenant_id.sql, IMPROVEMENT-PLAN
		// 3.10.
		var existingWfID string
		var existingDef sql.NullString
		var existingDigest sql.NullString
		err = tx.QueryRowContext(ctx,
			`SELECT workflow_id, def_name, input_digest FROM idempotency_keys
			 WHERE key_hash = $1 AND tenant_id = $2 AND expires_at > now()`,
			keyHash[:], tenantID).Scan(&existingWfID, &existingDef, &existingDigest)
		if err == nil {
			// A hit must be for the SAME definition. NULL means the row predates
			// cleat#1047's backfill or its workflow has been purged -- unknown
			// rather than mismatched, so it is allowed through, which is exactly
			// today's behaviour for those rows.
			if existingDef.Valid && existingDef.String != defName {
				return "", false, fmt.Errorf("%w: key already started %q, this request names %q",
					ErrIdempotencyKeyDefMismatch, existingDef.String, defName)
			}
			if err := checkIdempotencyInput(existingDigest, inputDigest); err != nil {
				return "", false, err
			}
			return existingWfID, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}

		// Use the provided runID (already generated above).

		// Clear this key's row if its TTL has passed, so the INSERT below can
		// take the key over.
		//
		// WITHOUT THIS, `n == 0` BELOW HAS TWO CAUSES AND THE CODE ASSUMES
		// ONE. `ON CONFLICT (key_hash, tenant_id)` says nothing about expiry,
		// so an expired row still conflicts and still reports no rows
		// affected -- identical to the concurrent-insert case it is read as.
		// Only one of the two has a winner to re-read, and the re-read filters
		// on `expires_at > now()`, so for the other it looks for a row it
		// cannot see and the caller gets sql.ErrNoRows instead of a new run.
		// cleat#1671.
		//
		// Not a visibility fix: making the re-read see the expired row would
		// hand the caller a workflow id whose key the TTL already retired,
		// which is worse than the error. The dead row has to go.
		//
		// `expires_at <= now()` and not the key alone -- a row refreshed by a
		// concurrent starter between the lookup above and here is LIVE, and
		// deleting it would let two runs hold one key. In that case this
		// matches nothing and the INSERT below reports the conflict, which is
		// the outcome that path already handles correctly.
		//
		// Inside this transaction, so the delete and the insert commit or roll
		// back together. Scoped by the primary key, so it is a no-op lookup on
		// every start whose key is live or absent -- which is nearly all of
		// them.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM idempotency_keys
			 WHERE key_hash = $1 AND tenant_id = $2 AND expires_at <= now()`,
			keyHash[:], tenantID); err != nil {
			return "", false, fmt.Errorf("start new run: clear expired idempotency key: %w", err)
		}

		// Insert idempotency key record. ON CONFLICT DO NOTHING handles the
		// race where two requests arrive with the same key simultaneously --
		// and, since the delete above, ONLY that: a row that conflicts here is
		// necessarily live.
		ttlSeconds := int(s.idempotencyKeyTTL.Seconds())
		res, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id, def_name, input_digest)
			 VALUES ($1, $2, now() + ($3 * INTERVAL '1 second'), $4, $5, $6)
			 ON CONFLICT (key_hash, tenant_id) DO NOTHING`,
			keyHash[:], runID, ttlSeconds, tenantID, defName, inputDigest)
		if err != nil {
			return "", false, err
		}

		n, _ := res.RowsAffected()
		if n == 0 {
			// A LIVE row exists, so someone else won the race. The expired
			// case cannot reach here any more (cleat#1671): the delete above
			// removed it, so a conflict means a row whose TTL has not passed,
			// and the re-read's own `expires_at > now()` will find it.
			//
			// Roll back to release the lock at once, then re-read the winner.
			//
			// The re-read needs a transaction of its own: this one is being
			// abandoned, and the row it is looking for belongs to whoever won
			// the race. It ran on the pool until cleat#1534, which is the same
			// statement as the lookup above and the same reason it had to move.
			_ = tx.Rollback()
			tx2, err := s.beginTxWithRLS(ctx)
			if err != nil {
				return "", false, fmt.Errorf("start new run: begin re-read: %w", err)
			}
			defer tx2.Rollback()
			err = tx2.QueryRowContext(ctx,
				`SELECT workflow_id, def_name, input_digest FROM idempotency_keys
				 WHERE key_hash = $1 AND tenant_id = $2 AND expires_at > now()`,
				keyHash[:], tenantID).Scan(&existingWfID, &existingDef, &existingDigest)
			if err != nil {
				return "", false, err
			}
			// The concurrent winner must also be for THIS definition. Without
			// this the race path returns the other workflow's id even though
			// the lookup above refuses it -- the same defect, reachable only
			// under contention, which is where it would be hardest to see.
			if existingDef.Valid && existingDef.String != defName {
				return "", false, fmt.Errorf("%w: key already started %q, this request names %q",
					ErrIdempotencyKeyDefMismatch, existingDef.String, defName)
			}
			if err := checkIdempotencyInput(existingDigest, inputDigest); err != nil {
				return "", false, err
			}
			return existingWfID, true, nil
		}

		// The tenant was established when this transaction was opened, above.
		// It used to be set HERE, after the idempotency_keys work, and that
		// ordering is the whole of cleat#1534.

		// Insert the workflow instance.
		var resolvedTaskQueue string
		err = tx.QueryRowContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
			VALUES ($1, $2, $3, 'ready', $4,
			        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3 AND tenant_id = $5), 'default'),
			$5, $6, now() - INTERVAL '1 millisecond',
			NULLIF($7, ''), CASE WHEN $7 = '' THEN NULL ELSE digest($7, 'sha256') END, $8, $9, $10, $11)
			RETURNING task_queue
		`, runID, defName, defVersion, sealedInput, tenantID, priority, concurrencyKey, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs).Scan(&resolvedTaskQueue)
		if err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		if err := s.insertLeaseAndPayloadRows(ctx, tx, runID, tenantID, resolvedTaskQueue, priority, sealedInput); err != nil {
			return "", false, fmt.Errorf("start new run: %w", err)
		}

		pgNotify(ctx, tx, s.notifyChannel)
		return runID, false, tx.Commit()
	}

	// No idempotency key — normal flow.
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return "", false, fmt.Errorf("start new run: begin: %w", err)
	}
	defer tx.Rollback()

	var resolvedTaskQueue string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
		VALUES ($1, $2, $3, 'ready', $4,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3 AND tenant_id = $5), 'default'),
			$5, $6, now() - INTERVAL '1 millisecond',
			NULLIF($7, ''), CASE WHEN $7 = '' THEN NULL ELSE digest($7, 'sha256') END, $8, $9, $10, $11)
		RETURNING task_queue
	`, runID, defName, defVersion, sealedInput, tenantID, priority, concurrencyKey, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs).Scan(&resolvedTaskQueue)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	if err := s.insertLeaseAndPayloadRows(ctx, tx, runID, tenantID, resolvedTaskQueue, priority, sealedInput); err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return runID, false, tx.Commit()
}

// insertLeaseAndPayloadRows creates the workflow_leases and workflow_payloads
// rows that go with a freshly inserted workflow_instances row, on the SAME
// tx -- cleat#3245 Phase 3 step 2 (dual-write), piece 1. There is no backfill
// (owner decision: a fresh database is required for 0.5.0, so no supported
// upgrade path has a pre-existing workflow_instances row to catch up), which
// is what makes "create both rows together, from here on, every time" the
// whole of this step's job for new runs.
//
// next_wake_at is set explicitly to the same `now() - 1ms` the
// workflow_instances INSERT uses, rather than relying on this table's own
// `DEFAULT now()` -- the two would otherwise disagree by whatever the gap
// between the two statements happens to be, and this column feeds claim
// ordering. created_at is NOT passed explicitly: both tables default it to
// `now()`, and `now()` is the TRANSACTION timestamp in PostgreSQL (constant
// across every statement in one tx), so the two columns agree without having
// to say so.
//
// taskQueue is the value workflow_instances' own INSERT just resolved via
// its COALESCE-against-workflow_defs subquery, captured with RETURNING
// rather than re-evaluated here -- a second subquery risks reading a
// workflow_defs row that changed between the two statements, however
// unlikely, and silently duplicating a drifted value is worse than reusing
// the one already decided.
//
// sealedPayloadInput is whatever startNewRun already bound to
// workflow_instances.input (ciphertext when encryption is on, per
// cleat#2312) -- reused rather than re-sealed, so the two tables carry
// identical bytes rather than two independent encryptions of the same
// plaintext.
func (s *PostgresStore) insertLeaseAndPayloadRows(ctx context.Context, tx *sql.Tx, id, tenantID, taskQueue string, priority int, sealedPayloadInput string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_leases (id, tenant_id, task_queue, priority, next_wake_at)
		VALUES ($1, $2, $3, $4, now() - INTERVAL '1 millisecond')
	`, id, tenantID, taskQueue, priority); err != nil {
		return fmt.Errorf("insert lease row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_payloads (id, tenant_id, input)
		VALUES ($1, $2, $3)
	`, id, tenantID, sealedPayloadInput); err != nil {
		return fmt.Errorf("insert payload row: %w", err)
	}
	return nil
}

// completeLeaseRow mirrors a terminal status transition (CompleteWorkflow,
// FailWorkflow, MoveToDeadLetterQueue) onto workflow_leases, on the same tx
// and the same (assigned_to, generation) fence predicate as the
// workflow_instances UPDATE it follows -- cleat#3245 Phase 3 step 2, piece
// 4a. `completed_by = assigned_to, assigned_to = NULL` is evaluated against
// workflow_leases' OWN pre-update assigned_to (SQL SET clauses read the old
// row, never a value from another statement), so this needs no value passed
// in beyond the new status -- same as pieces 1/2's mirrors of this exact
// expression.
func (s *PostgresStore) completeLeaseRow(ctx context.Context, tx *sql.Tx, workflowID, workerID, status string, generation int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET status = $3, completed_by = assigned_to, assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2 AND generation = $4
	`, workflowID, workerID, status, generation); err != nil {
		return fmt.Errorf("mirror lease row: %w", err)
	}
	return nil
}

// writeResultPayload mirrors CompleteWorkflow's result/query_state write
// onto workflow_payloads, on the same tx. Takes already-sealed values (per
// cleat#2312) so the two tables carry identical ciphertext rather than two
// independent encryptions of the same plaintext.
func (s *PostgresStore) writeResultPayload(ctx context.Context, tx *sql.Tx, workflowID string, sealedResult, sealedQS any) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_payloads SET result = $2, query_state = $3 WHERE id = $1
	`, workflowID, sealedResult, sealedQS); err != nil {
		return fmt.Errorf("mirror payload row: %w", err)
	}
	return nil
}

// writeErrorPayload mirrors FailWorkflow/MoveToDeadLetterQueue's
// error_msg/error_code/error_op/query_state write onto workflow_payloads, on
// the same tx. qsParam is COALESCEd exactly as the workflow_instances UPDATE
// does -- a nil qsParam (query state not supplied) leaves the column
// unchanged rather than nulling it.
func (s *PostgresStore) writeErrorPayload(ctx context.Context, tx *sql.Tx, workflowID string, sealedErrMsg, sealedErrorCode, sealedErrorOp, qsParam any) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_payloads
		SET error_msg = $2, error_code = $3, error_op = $4, query_state = COALESCE($5::jsonb, query_state)
		WHERE id = $1
	`, workflowID, sealedErrMsg, sealedErrorCode, sealedErrorOp, qsParam); err != nil {
		return fmt.Errorf("mirror payload row: %w", err)
	}
	return nil
}

// StartChildWorkflow creates a child workflow instance linked to a parent.
// The child is created with its own independent workflow instance.
// If defVersion > 0, that version is used explicitly; otherwise the latest
// non-disabled version is used (SELECT MAX(version)).

// reapLimitArg turns the interface's "limit <= 0 is unbounded" into a value a
// LIMIT clause accepts on every dialect. math.MaxInt32 rather than NULL: NULL
// means unbounded to PostgreSQL's LIMIT and means "no rows" to MySQL's, and a
// sweep that silently reclaimed nothing is the failure this whole bound exists
// to make visible.
func reapLimitArg(limit int) int {
	if limit <= 0 {
		return math.MaxInt32
	}
	return limit
}

func (s *PostgresStore) ReapStaleInstances(ctx context.Context, timeout time.Duration, limit int) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: begin: %w", err)
	}
	defer tx.Rollback()

	// A workflow reaped mid-defer-phase goes back to 'terminating', not to
	// 'ready'. The marker on the row is what makes the next claim a defer
	// segment either way -- the executor reads pending_terminal_status, not
	// the status -- so this is not what keeps the phase correct. It is what
	// keeps the status honest: a workflow whose terminal outcome is already
	// decided is not runnable work, and reporting it as 'ready' would undo
	// exactly the distinction D6 created the status to make.
	//
	// The inner SELECT is what bounds the sweep -- see the interface doc for
	// why it is bounded at all. ORDER BY heartbeat_at reclaims the
	// longest-stale first, so a bound that binds delays the freshest rather
	// than picking arbitrarily.
	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
		    reclaim_count = reclaim_count + 1
		WHERE id IN (
		    SELECT id FROM workflow_instances
		    WHERE status = 'running'
		      AND heartbeat_at < now() - $1::interval
		    ORDER BY heartbeat_at
		    LIMIT $2
		)
		RETURNING id
	`, fmt.Sprintf("%d milliseconds", timeout.Milliseconds()), reapLimitArg(limit))
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	reapedIDs, err := scanWorkflowIDs(rows)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: scan: %w", err)
	}
	// cleat#3245 Phase 3 step 2 piece 6b: mirror onto workflow_leases, on the
	// same tx -- the exact set of ids the UPDATE above just reclaimed,
	// captured via RETURNING rather than re-run against workflow_leases'
	// own heartbeat_at/status columns. Piece 3 keeps those in sync, so the
	// two subqueries SHOULD already agree, but capturing avoids depending on
	// that agreement holding -- the same reasoning pieces 1/4b/4d use for
	// RETURNING over re-deriving.
	if err := reapLeaseRows(ctx, tx, reapedIDs); err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	return len(reapedIDs), tx.Commit()
}

// reapLeaseRows mirrors ReapStaleInstances/ReapStaleInstancesExcept's bulk
// reclaim onto workflow_leases, on the caller's tx, for exactly the ids named
// -- cleat#3245 Phase 3 step 2, piece 6b. A no-op on an empty slice: pq.Array
// of an empty slice still issues a statement matching nothing, but skipping
// it entirely avoids a wasted round trip on the common case (nothing stale).
func reapLeaseRows(ctx context.Context, tx *sql.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_leases
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
		    reclaim_count = reclaim_count + 1
		WHERE id = ANY($1)
	`, pq.Array(ids)); err != nil {
		return fmt.Errorf("mirror lease rows: %w", err)
	}
	return nil
}

// ListStaleHolders satisfies StaleHolderReaper. Same RLS scoping and same
// status='running'/heartbeat_at predicate as ReapStaleInstances, because it
// has to report the same population -- see that method's doc and
// StaleHolderReaper's doc for why the two are allowed to race.
func (s *PostgresStore) ListStaleHolders(ctx context.Context, timeout time.Duration, limit int) ([]StaleHold, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("list stale holders: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, generation, assigned_to FROM workflow_instances
		WHERE status = 'running'
		  AND heartbeat_at < now() - $1::interval
		ORDER BY heartbeat_at
		LIMIT $2
	`, fmt.Sprintf("%d milliseconds", timeout.Milliseconds()), reapLimitArg(limit))
	if err != nil {
		return nil, fmt.Errorf("list stale holders: %w", err)
	}
	defer rows.Close()

	var holders []StaleHold
	for rows.Next() {
		var h StaleHold
		var assignedTo sql.NullString
		if err := rows.Scan(&h.Key.WorkflowID, &h.Key.Generation, &assignedTo); err != nil {
			return nil, fmt.Errorf("list stale holders: scan: %w", err)
		}
		h.AssignedTo = assignedTo.String
		holders = append(holders, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stale holders: rows: %w", err)
	}
	return holders, tx.Commit()
}

// ReapStaleInstancesExcept satisfies StaleHolderReaper. Identical to
// ReapStaleInstances except for the NOT EXISTS clause, which is a no-op
// against an empty exclude -- unnest of two empty arrays produces zero
// rows, so NOT EXISTS is unconditionally true and every row ReapStaleInstances
// would have reclaimed is still reclaimed.
func (s *PostgresStore) ReapStaleInstancesExcept(ctx context.Context, timeout time.Duration, limit int, exclude []GenerationKey) (int, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances except: begin: %w", err)
	}
	defer tx.Rollback()

	excludeIDs := make([]string, len(exclude))
	excludeGenerations := make([]int64, len(exclude))
	for i, k := range exclude {
		excludeIDs[i] = k.WorkflowID
		excludeGenerations[i] = k.Generation
	}

	rows, err := tx.QueryContext(ctx, `
		UPDATE workflow_instances
		SET status = CASE WHEN pending_terminal_status IS NOT NULL
		                  THEN 'terminating' ELSE 'ready' END,
		    assigned_to = NULL, heartbeat_at = NULL, generation = generation + 1,
		    reclaim_count = reclaim_count + 1
		WHERE id IN (
		    SELECT id FROM workflow_instances
		    WHERE status = 'running'
		      AND heartbeat_at < now() - $1::interval
		      AND NOT EXISTS (
		          SELECT 1 FROM unnest($3::text[], $4::bigint[]) AS ex(id, generation)
		          WHERE ex.id = workflow_instances.id AND ex.generation = workflow_instances.generation
		      )
		    ORDER BY heartbeat_at
		    LIMIT $2
		)
		RETURNING id
	`, fmt.Sprintf("%d milliseconds", timeout.Milliseconds()), reapLimitArg(limit), excludeIDs, excludeGenerations)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances except: %w", err)
	}
	reapedIDs, err := scanWorkflowIDs(rows)
	if err != nil {
		return 0, fmt.Errorf("reap stale instances except: scan: %w", err)
	}
	// cleat#3245 Phase 3 step 2 piece 6b: mirror onto workflow_leases, on the
	// same tx -- see ReapStaleInstances' identical comment above.
	if err := reapLeaseRows(ctx, tx, reapedIDs); err != nil {
		return 0, fmt.Errorf("reap stale instances except: %w", err)
	}
	return len(reapedIDs), tx.Commit()
}

// PingDB satisfies DBPinger: a bounded round trip with no workflow-specific
// query, so a worker with nothing in flight still has a way to prove it can
// reach the database. See DBPinger's doc comment for why this exists.
func (s *PostgresStore) PingDB(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// StaleSetShape satisfies DBStallDetector. Same RLS scoping and same
// status='running' population as ReapStaleInstances, so the shape this
// reports is the shape ReapStaleInstances would actually act on -- see
// that method's doc for why 'running' is the whole population and why
// nothing here should widen it.
func (s *PostgresStore) StaleSetShape(ctx context.Context, timeout, missedBeatTimeout time.Duration) (StaleSetShape, error) {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return StaleSetShape{}, fmt.Errorf("stale set shape: begin: %w", err)
	}
	defer tx.Rollback()

	var shape StaleSetShape
	var oldest, newest sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT
		    COUNT(*),
		    COUNT(*) FILTER (WHERE heartbeat_at < now() - $1::interval),
		    COUNT(DISTINCT assigned_to) FILTER (WHERE heartbeat_at < now() - $1::interval),
		    MIN(heartbeat_at) FILTER (WHERE heartbeat_at < now() - $1::interval),
		    MAX(heartbeat_at) FILTER (WHERE heartbeat_at < now() - $1::interval),
		    COUNT(*) FILTER (WHERE heartbeat_at < now() - $2::interval),
		    (CASE WHEN MAX(heartbeat_at) < now() - $1::interval THEN true ELSE false END),
		    COUNT(DISTINCT assigned_to)
		FROM workflow_instances
		WHERE status = 'running'
	`, fmt.Sprintf("%d milliseconds", missedBeatTimeout.Milliseconds()),
		fmt.Sprintf("%d milliseconds", timeout.Milliseconds()),
	).Scan(&shape.Running, &shape.MissedBeat, &shape.MissedBeatDistinctAssignedTo,
		&oldest, &newest, &shape.Stale, &shape.NoRecentHeartbeat, &shape.DistinctAssignedTo)
	if err != nil {
		return StaleSetShape{}, fmt.Errorf("stale set shape: %w", err)
	}
	if oldest.Valid {
		shape.MissedBeatOldest = oldest.Time
	}
	if newest.Valid {
		shape.MissedBeatNewest = newest.Time
	}
	return shape, tx.Commit()
}

// ---- SignalStore interface implementation ----

// DeliverSignal satisfies the SignalStore interface.

// finishClaim commits a claim transaction and enforces the claim-limit
// invariant, releasing any excess rather than truncating it away. See
// enforceClaimLimit in claim_limit.go for why.
func (s *PostgresStore) finishClaim(ctx context.Context, tx *sql.Tx, workerID string, limit int, wfs []*WorkflowInstance) ([]*WorkflowInstance, error) {
	keep, excess := enforceClaimLimit(ctx, s.log(), "postgres", workerID, limit, wfs)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, wf := range excess {
		if err := s.ReleaseWorkflow(context.Background(), wf.ID, workerID, wf.Generation, wf.NextWakeAt); err != nil {
			s.log().ErrorContext(ctx, "releasing an over-claimed workflow failed; it stays claimed until its lease expires",
				"worker_id", workerID, "workflow_id", wf.ID, "error", err)
		}
	}

	// cleat#2312: input and error_code/error_op are sensitive-at-rest. This
	// is the one chokepoint both ClaimWorkflows and ClaimStickyWorkflows
	// funnel through, so decrypting here -- rather than in each scan loop --
	// covers both. A decrypt failure is logged and the ciphertext is left in
	// place rather than aborting the claim: the workflow has already been
	// claimed in the database by this point, and handing back a claim this
	// worker cannot read is better than silently losing the claim.
	for _, wf := range keep {
		decryptedInput, err := s.decryptPayloadJSON(string(wf.Input))
		if err != nil {
			s.log().WarnContext(ctx, "decrypt claimed workflow input failed",
				"workflow_id", wf.ID, "error", err)
		} else {
			wf.Input = json.RawMessage(decryptedInput)
		}
		if decryptedCode, err := s.decryptTextColumnFromStorage(wf.ErrorCode, "error_code"); err != nil {
			s.log().WarnContext(ctx, "decrypt claimed workflow error_code failed",
				"workflow_id", wf.ID, "error", err)
		} else {
			wf.ErrorCode = decryptedCode
		}
		if decryptedOp, err := s.decryptTextColumnFromStorage(wf.ErrorOp, "error_op"); err != nil {
			s.log().WarnContext(ctx, "decrypt claimed workflow error_op failed",
				"workflow_id", wf.ID, "error", err)
		} else {
			wf.ErrorOp = decryptedOp
		}
	}

	return keep, nil
}

// coerceResultJSON returns a result safe for the JSON-typed result column, and
// says so out loud when it had to replace one.
//
// The column is JSONB on PostgreSQL, JSON on MySQL and NVARCHAR(MAX) under an
// ISJSON check on SQL Server, so a result that is not valid JSON cannot be
// stored and something has to give. Replacing it with "{}" is the right call --
// failing the terminal write would lose the whole workflow over a formatting
// defect -- but doing it silently is not.
//
// It was silent, and that is how IMPROVEMENT-PLAN 3.22 erased an ambiguous call
// rather than merely mislabelling it: a workflow whose durable call came back
// [AMBIGUOUS] returned `{"error":""durable call ...""}` -- doubled quotes, from
// a generator that wraps an already-quoted string a second time -- which is
// invalid, so the workflow was stored `done` with result `{}` and no error
// anywhere. The engine had detected the ambiguity correctly and every trace of
// it was dropped here, in a two-line conditional with no log statement.
//
// The empty case is not logged: an entry point with no return value produces it
// on every successful run, and it is what the column default means anyway.
func coerceResultJSON(ctx context.Context, log *slog.Logger, workflowID, result string) string {
	if result == "" {
		return "{}"
	}
	if json.Valid([]byte(result)) {
		// Valid JSON is not the same as conforming to the contract. An entry
		// point returns a string containing a JSON-encoded OBJECT, and
		// json.Valid happily accepts a bare string, number or array -- which is
		// precisely why a double-encoded result ("{\"ok\":true}" as a JSON
		// string) sailed through here undetected across three SDKs.
		//
		// Reported, not rejected. Replacing a valid-but-wrong-shaped result
		// with {} would destroy data that is at least storable, and workflows
		// predating the contract still return scalars. What this removes is the
		// silence: a violation now names itself, with the workflow that
		// produced it.
		if log != nil && !looksLikeJSONObject(result) {
			log.ErrorContext(ctx, "workflow result is valid JSON but not an object -- the "+
				"contract is a string containing a JSON-encoded object, so this is stored as-is "+
				"but no consumer can rely on its shape. A result that starts with a quote is "+
				"usually an SDK encoding a value that was already JSON",
				"workflow_id", workflowID, "result_len", len(result),
				"result", truncateForLog(result))
		}
		return result
	}
	if log != nil {
		log.ErrorContext(ctx, "workflow result is not valid JSON and was replaced with {} -- "+
			"whatever it carried, including any error the workflow returned, is not stored anywhere",
			"workflow_id", workflowID, "result_len", len(result), "result", truncateForLog(result))
	}
	return "{}"
}

// truncateForLog bounds a value that goes into a log line. A workflow result is
// caller-controlled and can be large.
func truncateForLog(s string) string {
	const max = 512
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}

// scanClaimedWorkflows reads the rows a claim returns.
//
// ClaimWorkflowsAcrossTenants was retired in #1926 (the widened
// admin.claim_workflows path replaced by unconditional per-tenant rotation);
// this scan now has ClaimWorkflows as its only caller.
func scanClaimedWorkflows(rows *sql.Rows) ([]*WorkflowInstance, error) {
	var wfs []*WorkflowInstance
	for rows.Next() {
		var wf WorkflowInstance
		var nextWakeAt, createdAt sql.NullTime
		var tenantID, errorCode, errorOp sql.NullString

		if err := rows.Scan(&wf.ID, &wf.DefName, &wf.DefVersion, &wf.Status, &wf.Input,
			&wf.AssignedTo, &nextWakeAt, &tenantID, &createdAt, &errorCode, &errorOp,
			&wf.Generation, &wf.Priority, &wf.TraceID, &wf.PendingTerminalStatus); err != nil {
			return nil, fmt.Errorf("claim workflows scan: %w", err)
		}

		if nextWakeAt.Valid {
			wf.NextWakeAt = nextWakeAt.Time
		}
		if tenantID.Valid {
			wf.TenantID = tenantID.String
		}
		if createdAt.Valid {
			wf.CreatedAt = createdAt.Time
		}
		wf.ErrorCode = errorCode.String
		wf.ErrorOp = errorOp.String
		wfs = append(wfs, &wf)
	}
	return wfs, rows.Err()
}

// scanDueSchedules reads the rows a due-schedule query returns.
//
// Shared by the cross-tenant read and, on PostgreSQL, by the tenant-scoped one
// for the same reason scanClaimedWorkflows is shared: the cross-tenant column
// list lives in a migration, and the only thing keeping it in step with this
// scan is that there is exactly one scan.
func scanDueSchedules(rows *sql.Rows) ([]Schedule, error) {
	var schedules []Schedule
	for rows.Next() {
		var sch Schedule
		var lastRunAt sql.NullTime
		if err := rows.Scan(&sch.Name, &sch.DefName, &sch.EntryPoint, &sch.CronExpression,
			&sch.Input, &sch.DisabledAt, &sch.NextRunAt, &lastRunAt, &sch.Timezone, &sch.TenantID,
			&sch.MisfirePolicy, &sch.CatchUpLimit, &sch.OverlapPolicy, &sch.LastRunID); err != nil {
			return nil, fmt.Errorf("get due schedules scan: %w", err)
		}
		if lastRunAt.Valid {
			sch.LastRunAt = &lastRunAt.Time
		}
		schedules = append(schedules, sch)
	}
	return schedules, rows.Err()
}

// looksLikeJSONObject reports whether a result is a JSON object.
//
// Structural, not a full parse: coerceResultJSON has already established the
// value is valid JSON, so the first non-space byte decides it. A parse here
// would cost a second decode of every workflow result on the terminal path to
// learn one byte.
func looksLikeJSONObject(result string) bool {
	for i := 0; i < len(result); i++ {
		switch result[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// FinalizeWorkflowSegment wraps finalizeWorkflowSegmentInner so that a backend
// refusing a JSON value it was handed becomes a classified error rather than
// driver text. cleat#1460.
//
// WRAPPED AT THE BOUNDARY, not at each return, and that is the point: this
// function has a dozen error paths and will grow more, and a classification
// applied at one of them is a classification the next one silently lacks.
// wrapRejectedResult returns anything it does not recognise unchanged, so the
// blanket wrap costs nothing and cannot mislabel an unrelated failure.
func (s *PostgresStore) FinalizeWorkflowSegment(ctx context.Context, runID, workerID string, generation int64, newEvents []EventRecord, finalStatus string, result string, errorCode string, errorOp string, queryState map[string]string, nextWakeAt time.Time) error {
	err := wrapRejectedResult(
		s.finalizeWorkflowSegmentInner(ctx, runID, workerID, generation, newEvents,
			finalStatus, result, errorCode, errorOp, queryState, nextWakeAt),
		runID, result)
	return wrapPostgresFinalizeDBError(err, runID)
}

// wrapPostgresFinalizeDBError classifies a database-originated finalize
// failure into a *CleatError so cmd/cleat-worker's errors.As(err, &ce) can
// derive error_code instead of leaving it ErrUnknown for every DB-originated
// terminal failure on this dialect (cleat#2805).
//
// Narrow on purpose, and the narrowing is load-bearing, not laziness:
//   - nil, ErrFenceLost, and an error wrapRejectedResult already classified
//     (ErrResultRejected) pass through unchanged -- this classifies only
//     what reaches it UNclassified.
//   - a connection-level failure never reaches here at all:
//     cmd/cleat-worker's isConnectionError (text-matched, dialect-agnostic)
//     intercepts it first and releases the workflow for another worker,
//     never calling recordTerminalFailure in the first place. So this
//     function does not need its own connection check, and one here would
//     be dead code by construction.
//   - 40P01 (deadlock_detected) and 40001 (serialization_failure) are the
//     two PostgreSQL SQLSTATEs where the server GUARANTEES the transaction
//     was rolled back, so the failure describes contention, not a data
//     problem -> ErrTransient. A context cancellation or deadline (worker
//     shutdown mid-transaction, a caller's own timeout) is the same shape
//     for the same reason: it says nothing about the data and a retry is
//     sound -> ErrTransient too.
//   - Everything else defaults to ErrPermanent, matching the deleted
//     mapMSSQLError's own default arm (cleat#2792) and CleatError's own
//     "non-retryable" semantics -- an unrecognized DB failure is safer
//     reported as needing a human than silently retried.
func wrapPostgresFinalizeDBError(err error, workflowID string) error {
	if err == nil || errors.Is(err, ErrFenceLost) {
		return err
	}
	var ce *CleatError
	if errors.As(err, &ce) {
		return err
	}
	code := ErrPermanent
	var pqErr *pq.Error
	switch {
	case errors.As(err, &pqErr) && (pqErr.Code == "40P01" || pqErr.Code == "40001"):
		code = ErrTransient
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		code = ErrTransient
	}
	return &CleatError{Code: code, Op: "finalize workflow", WorkflowID: workflowID, Err: err}
}
