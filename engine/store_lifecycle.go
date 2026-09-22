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
		                      WHERE ck.key_hash = w.concurrency_key_hash
		                        AND ck.tenant_id = w.tenant_id
		                        AND ck.expires_at > now()
		                        AND ck.workflow_id <> w.id)
		    ))
		    OR
		    (q.name IS NOT NULL AND (
		      (
		        (SELECT count(*) FROM queue_holders qh
		          WHERE qh.tenant_id = w.tenant_id
		            AND qh.queue_name = q.name
		            AND qh.expires_at > now()
		            AND qh.workflow_id <> w.id) < q.concurrency_limit
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
// Queue itself uses (queue_store.go).
type registeredQueueLimits struct {
	concurrencyLimit  int
	rateLimit         *int
	ratePeriodSeconds *int
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
			                      WHERE ck.key_hash = w.concurrency_key_hash
			                        AND ck.tenant_id = w.tenant_id
			                        AND ck.expires_at > now()
			                        AND ck.workflow_id <> w.id)
			    ))
			    OR
			    (q.name IS NOT NULL AND (
			      (
			        (SELECT count(*) FROM queue_holders qh
			          WHERE qh.tenant_id = w.tenant_id
			            AND qh.queue_name = q.name
			            AND qh.expires_at > now()
			            AND qh.workflow_id <> w.id) < q.concurrency_limit
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
		ok, err := s.acquireCandidateConcurrencyKey(ctx, tx, c, limits)
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
	return s.finishClaim(ctx, tx, workerID, limit, wfs)
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
		SELECT name, concurrency_limit, rate_limit, rate_period_seconds FROM queues
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
		var rateLimit, ratePeriodSeconds sql.NullInt64
		if err := rows.Scan(&name, &ql.concurrencyLimit, &rateLimit, &ratePeriodSeconds); err != nil {
			return nil, fmt.Errorf("claim workflows: scan queue limit: %w", err)
		}
		ql.rateLimit, ql.ratePeriodSeconds = nullInt64Pair(rateLimit, ratePeriodSeconds)
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
// same lock -- both must admit for the candidate to be claimed.
func (s *PostgresStore) acquireCandidateConcurrencyKey(ctx context.Context, tx *sql.Tx, c claimCandidate, limits map[string]registeredQueueLimits) (bool, error) {
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
	// Re-claim: a run that already holds its own slot claims again after a lost
	// fence, without counting against the limit OR taking a new rate token --
	// it is continuing an admission already granted, not a new one.
	var selfHolds bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM queue_holders qh
			WHERE qh.tenant_id = $1 AND qh.queue_name = $2 AND qh.workflow_id = $3
			  AND qh.expires_at > now()
		)
	`, c.tenantID, *c.key, c.id).Scan(&selfHolds)
	if err != nil {
		return false, fmt.Errorf("claim workflows: queue self-hold check: %w", err)
	}
	if selfHolds {
		return true, nil
	}
	// Not already holding: count other holders and insert if a slot is free.
	var held int
	err = tx.QueryRowContext(ctx, `
		SELECT count(*) FROM queue_holders qh
		WHERE qh.tenant_id = $1 AND qh.queue_name = $2 AND qh.expires_at > now()
	`, c.tenantID, *c.key).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("claim workflows: count queue holders: %w", err)
	}
	if held >= ql.concurrencyLimit {
		return false, nil // at capacity
	}
	// cleat#1918: the rate limit, if declared, is checked under this same
	// queues-row lock -- independent of the concurrency check above, so a free
	// concurrency slot does not admit past a full rate window.
	if ql.rateLimit != nil {
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
	res, err := tx.ExecContext(ctx, `
		INSERT INTO queue_holders (tenant_id, queue_name, workflow_id, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		ON CONFLICT (tenant_id, queue_name, workflow_id) DO NOTHING
	`, c.tenantID, *c.key, c.id, claimedKeyTTL.Seconds())
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
                       WHERE ck.key_hash = workflow_instances.concurrency_key_hash
                         AND ck.tenant_id = workflow_instances.tenant_id
                         AND ck.expires_at > now()
                         AND ck.workflow_id <> workflow_instances.id))
			ORDER BY priority ASC, created_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE workflow_instances w
		SET status = 'running',
		    signal_seq_at_claim = signal_seq,
		    signal_consumed_at_claim = signal_consumed_seq,
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

	// Create the new workflow run.
	// Use the store's tenant scope to preserve tenant isolation.
	var newRunID string
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
		RETURNING id
		`, defName, defVersion, newInput, s.tenantID, priority, currentRunID).Scan(&newRunID)
	if err != nil {
		return "", fmt.Errorf("continue as new: start new run: %w", err)
	}

	// Complete the current run.
	qsJSON := marshalQueryState(queryState)
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
	`, currentRunID, workerID, resultJSON, qsJSON, generation)
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

	if err := tx.Commit(); err != nil {
		return "", err
	}

	releaseWorkflowResources(s.log(), s, currentRunID)
	s.enforceParentClosePolicy(context.Background(), currentRunID)

	return newRunID, nil
}

// FinalizeWorkflowSegment atomically appends new events and updates the
// workflow status in a single database transaction.  This eliminates the
// race between AppendEventHistoryBatch and the subsequent CompleteWorkflow /
// FailWorkflow / ReleaseWorkflow call.
//
// finalStatus must be one of:
//   - "done"   — marks the workflow as completed with the given result
//   - "failed" — marks the workflow as failed with the given error info
//   - "ready"  — returns the workflow to the ready queue (suspend)
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

	var fenceHeld bool
	if err := tx.QueryRowContext(ctx, `
		SELECT finalize_workflow_status($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, runID, workerID, generation, finalStatus, resultJSON, errorCode, errorOp, string(qsJSON), nextWakeAt, s.notifyChannel).Scan(&fenceHeld); err != nil {
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
		s.enforceParentClosePolicy(context.Background(), runID)
	}

	return nil
}

// validFinalStatus returns true for status values accepted by finalize_workflow_status.
func validFinalStatus(status string) bool {
	switch status {
	case "done", "failed", "ready", "suspended":
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
	`, workflowID, workerID, resultJSON, qsJSON, generation)
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
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// FailWorkflow marks a workflow as failed.

func (s *PostgresStore) FailWorkflow(ctx context.Context, workflowID, workerID string, generation int64, errorMsg, errorCode, errorOp string, queryState map[string]string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("fail workflow: begin: %w", err)
	}
	defer tx.Rollback()

	qsJSON := marshalQueryState(queryState)
	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'failed',
		    error_msg = $3,
		    error_code = $4,
		    error_op = $5,
		    completed_at = now(),
		    completed_by = assigned_to, assigned_to = NULL,
		    query_state = $6
		WHERE id = $1 AND assigned_to = $2 AND generation = $7
	`, workflowID, workerID, errorMsg, errorCode, errorOp, string(qsJSON), generation)
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

	// Record idempotency error within the transaction (best-effort).
	//
	// AND tenant_id = $3: see the identical comment on the sibling UPDATE in
	// CompleteWorkflow. s.tenantID is already set on this tx by
	// beginTxWithRLS.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1 AND tenant_id = $3`,
		workflowID, errorMsg, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

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
func (s *PostgresStore) enforceParentClosePolicy(ctx context.Context, parentWorkflowID string) {
	s.enforceParentClosePolicyAt(ctx, parentWorkflowID, 0)
}

// enforceParentClosePolicyAt is enforceParentClosePolicy with the recursion
// depth carried explicitly. See cascadeIntoClosedChildren.
func (s *PostgresStore) enforceParentClosePolicyAt(ctx context.Context, parentWorkflowID string, depth int) {
	steps := []struct {
		policy string
		query  string
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
		// WHAT COUNTS AS ALREADY-TERMINAL HERE, and why the list is four values
		// rather than two.
		//
		// It was `NOT IN ('done', 'failed')`, and the engine writes two more
		// terminal statuses than that: 'dead_lettered' and 'terminated'. So a
		// dead-lettered child MATCHED, and a parent closing with TERMINATE
		// overwrote it (cleat#1227, measured on all three dialects):
		//
		//	BEFORE  status=dead_lettered  error_msg="retries exhausted"           error_code=E_RETRY
		//	AFTER   status=failed         error_msg="parent workflow terminated"  error_code=E_RETRY
		//
		// Three losses in one UPDATE. The run leaves the dead-letter queue; its
		// original failure reason is replaced; and error_code is NOT in the SET
		// list, so the surviving row reports TWO DIFFERENT CAUSES at once. That
		// last part is what makes it worse than a plain overwrite -- nothing about
		// the result looks wrong.
		//
		// 'terminating' is deliberately NOT here. A child mid-shutdown is not
		// terminal, and the two arms below split on deferPhaseOwedSQL precisely to
		// give it a defer phase rather than a terminal write.
		//
		// 'cancelled' is deliberately NOT here either: nothing writes it to
		// workflow_instances.status. It exists elsewhere in the codebase, which is
		// exactly the trap -- a status list assembled by grepping the tree for
		// status-shaped strings picks it up. There is no CHECK constraint to
		// consult, so the vocabulary has to come from what production actually
		// WRITES, and the whole of it is seven values:
		//
		//	dead_lettered  done  failed  ready  running  terminated  terminating
		//
		// 'suspended' is the sharpest illustration and is NOT one of them.
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
		SET status = 'failed', error_msg = 'parent workflow terminated',
		    pending_terminal_status = NULL, defer_phase_deadline = NULL,
		    completed_at = now(),
		    completed_by = assigned_to, assigned_to = NULL, generation = generation + 1
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND NOT ` + deferPhaseOwedSQL + `
	`},
		{"TERMINATE (defer phase)", `
		UPDATE workflow_instances
		SET status = '` + statusTerminating + `',
		    pending_terminal_status = 'failed',
		    defer_phase_deadline = ` + deferPhaseDeadlinePostgres + `,
		    error_msg = 'parent workflow terminated',
		    next_wake_at = now(),
		    assigned_to = NULL,
		    generation = generation + 1
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'TERMINATE'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
		  AND ` + deferPhaseOwedSQL + `
	`},
		{"REQUEST_CANCEL", `
		UPDATE workflow_instances
		SET cancellation_requested = true
		WHERE parent_workflow_id = $1
		  AND parent_close_policy = 'REQUEST_CANCEL'
		  AND status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
	`},
	}

	// Collected before the UPDATE: see releaseTerminatedChildren for why not
	// RETURNING. A failure here costs the release, not the close policy.
	terminated, err := s.childrenClosedByTerminate(ctx, parentWorkflowID)
	if err != nil {
		s.log().WarnContext(ctx, "enforceParentClosePolicy: could not list TERMINATE children; their concurrency keys and sticky-worker assignments stay held until TTL",
			"parent_workflow_id", parentWorkflowID, "error", err)
	}

	for _, step := range steps {
		if err := s.runParentClosePolicyStep(ctx, step.query, parentWorkflowID); err != nil {
			s.log().WarnContext(ctx, "enforceParentClosePolicy failed; children of a closed parent are unaffected by its close policy",
				"policy", step.policy, "parent_workflow_id", parentWorkflowID, "error", err)
			if step.policy == "TERMINATE" {
				terminated = nil
			}
		}
	}

	releaseTerminatedChildren(s.log(), s, terminated)
	cascadeIntoClosedChildren(s.log(), depth, terminated, func(id string, d int) {
		s.enforceParentClosePolicyAt(ctx, id, d)
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

func (s *PostgresStore) runParentClosePolicyStep(ctx context.Context, query, parentWorkflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, query, parentWorkflowID); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveToDeadLetterQueue marks a workflow as dead_lettered because it failed
// after exhausting all retry attempts.

func (s *PostgresStore) MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64, errMsg, errorCode, errorOp string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("move to dead letter queue: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'dead_lettered', error_msg = $3, error_code = $4, error_op = $5,
		    completed_at = now(), completed_by = assigned_to, assigned_to = NULL
		WHERE id = $1 AND assigned_to = $2 AND generation = $6
	`, workflowID, workerID, errMsg, errorCode, errorOp, generation)
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
	// Record idempotency error within the transaction (best-effort).
	//
	// AND tenant_id = $3: see the identical comment on the sibling UPDATE in
	// CompleteWorkflow. s.tenantID is already set on this tx by
	// beginTxWithRLS.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET error_msg = $2 WHERE workflow_id = $1 AND tenant_id = $3`,
		workflowID, errMsg, s.tenantID); err != nil {
		s.log().WarnContext(ctx, "idempotency update failed", "error", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	releaseWorkflowResources(s.log(), s, workflowID)

	// Enforce ParentClosePolicy on children.
	s.enforceParentClosePolicy(context.Background(), workflowID)

	return nil
}

// RetryWorkflow moves a dead_lettered workflow back to a runnable state.

func (s *PostgresStore) RetryWorkflow(ctx context.Context, workflowID string) error {
	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return fmt.Errorf("retry workflow: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET status = 'ready', completed_by = assigned_to, assigned_to = NULL, heartbeat_at = NULL,
		    error_msg = NULL, error_code = NULL, error_op = NULL,
		    next_wake_at = now()
		WHERE id = $1 AND status = 'dead_lettered'
	`, workflowID)
	if err != nil {
		return err
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
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
			VALUES ($1, $2, $3, 'ready', $4,
			        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3 AND tenant_id = $5), 'default'),
			$5, $6, now() - INTERVAL '1 millisecond',
			NULLIF($7, ''), CASE WHEN $7 = '' THEN NULL ELSE digest($7, 'sha256') END, $8, $9, $10, $11)
		`, runID, defName, defVersion, input, tenantID, priority, concurrencyKey, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
		if err != nil {
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

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue, tenant_id, priority, next_wake_at, concurrency_key, concurrency_key_hash, run_wasm_instance_timeout_ms, run_wasm_wall_clock_ceiling_ms, run_host_retry_budget_ms, run_max_workflow_duration_ms)
		VALUES ($1, $2, $3, 'ready', $4,
		        COALESCE((SELECT task_queue FROM workflow_defs WHERE name = $2 AND version = $3 AND tenant_id = $5), 'default'),
			$5, $6, now() - INTERVAL '1 millisecond',
			NULLIF($7, ''), CASE WHEN $7 = '' THEN NULL ELSE digest($7, 'sha256') END, $8, $9, $10, $11)
	`, runID, defName, defVersion, input, tenantID, priority, concurrencyKey, runInstanceMs, runWallClockMs, runRetryMs, runMaxWorkflowMs)
	if err != nil {
		return "", false, fmt.Errorf("start new run: %w", err)
	}
	pgNotify(ctx, tx, s.notifyChannel)
	return runID, false, tx.Commit()
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
	result, err := tx.ExecContext(ctx, `
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
	`, fmt.Sprintf("%d seconds", int(timeout.Seconds())), reapLimitArg(limit))
	if err != nil {
		return 0, fmt.Errorf("reap stale instances: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), tx.Commit()
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
// Shared by ClaimWorkflows and ClaimWorkflowsAcrossTenants deliberately: the
// second reads its columns from admin.claim_workflows, a function defined in a
// migration, and the only thing keeping that definition in step with this scan
// is that there is exactly one scan. Two copies would drift, and the symptom
// would be a scan error at claim time on whichever deployment ran the newer
// migration.
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
	return wrapRejectedResult(
		s.finalizeWorkflowSegmentInner(ctx, runID, workerID, generation, newEvents,
			finalStatus, result, errorCode, errorOp, queryState, nextWakeAt),
		runID, result)
}
