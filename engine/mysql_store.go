package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// MySQL error code helpers
// ---------------------------------------------------------------------------

func isDuplicateKeyError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}

func isLockWaitTimeout(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1205
	}
	return false
}

func isDeadlockError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1213
	}
	return false
}

// ---------------------------------------------------------------------------
// MySQLStore
// ---------------------------------------------------------------------------

// MySQLStore implements WorkflowStore using a MySQL 8.0+ or MariaDB 10.6+
// database. MySQL has no row-level security, so tenant isolation is
// enforced at the database level — each tenant gets its own database
// (cleat_<tenant_id>). The store's connection pool is scoped to the
// tenant's database, making cross-tenant data access impossible at the
// connection level. WHERE tenant_id = ? clauses are retained as
// defense-in-depth.
//
// Because there is no RLS, those WHERE clauses are the only thing scoping a
// query to a tenant once a connection is open — PostgreSQL's seven policies
// have no MySQL equivalent (IMPROVEMENT-PLAN.md 1.7). A tenantID of "" is
// therefore not a harmless empty filter: it is a query running with no
// identity and no database-level backstop. See requireTenant below.
type MySQLStore struct {
	db                *sql.DB
	taskQueues        []string
	tenantID          string
	dialect           Dialect
	idempotencyKeyTTL time.Duration
	notifyChannel     string // MySQL has no LISTEN/NOTIFY; empty = disabled (forward compat)

	// Encryption at rest for sensitive event payloads.
	// NOTE: MySQL does not yet support encryption at rest; these fields are
	// present for forward compatibility so that StreamEventHistory can
	// contain the same decryption guard as the Postgres variant.
	encryption               *PayloadEncryption
	encryptSensitivePayloads bool

	// disableReadRedaction when true bypasses RedactOnRead on the read path.
	// Set to true during replay to avoid the overhead of retroactive redaction
	// on every event load. Default false.
	disableReadRedaction bool

	// perTenantDatabase records that this store was built by
	// MySQLStoreFactory, whose topology gives each tenant its own physical
	// database (cleat_<tenant_id>). Only ClaimWorkflowsAcrossTenants reads it,
	// and only to refuse: dropping a tenant_id predicate cannot widen a
	// connection that is pointed at a single tenant's database, so the claim
	// would return one tenant's work and report success. See the doc comment
	// on ClaimWorkflowsAcrossTenants.
	perTenantDatabase bool

	logger *slog.Logger
}

// NewMySQLStore creates a MySQLStore scoped to the given task queues.
// The taskQueues slice specifies which task queues this worker pool should
// poll (e.g., "default", "gpu", "high-memory"). Defaults to ["default"].
// The tenantID defaults to the default tenant UUID.
func NewMySQLStore(db *sql.DB, taskQueues ...string) *MySQLStore {
	tqs := taskQueues
	if len(tqs) == 0 {
		tqs = []string{"default"}
	}
	return &MySQLStore{
		db:                db,
		taskQueues:        tqs,
		tenantID:          "00000000-0000-0000-0000-000000000000",
		dialect:           DialectMySQL,
		idempotencyKeyTTL: 720 * time.Hour,
	}
}

// WithIdempotencyKeyTTL returns a copy of the store with the given idempotency key TTL.
func (s *MySQLStore) WithIdempotencyKeyTTL(ttl time.Duration) *MySQLStore {
	cp := *s
	cp.idempotencyKeyTTL = ttl
	return &cp
}

// WithReadRedactionDisabled returns a copy of the store with redaction on
// the read path disabled. This is used during replay to avoid the overhead
// of retroactive redaction on every event load.
func (s *MySQLStore) WithReadRedactionDisabled(disabled bool) *MySQLStore {
	cp := *s
	cp.disableReadRedaction = disabled
	return &cp
}

// WithEncryption returns a copy of the store with encryption at rest enabled.
// NOTE: Encryption at rest is not yet supported on MySQL backends. This method
// is present for forward compatibility so that StreamEventHistory can contain
// the same decryption guard as the Postgres variant.
func (s *MySQLStore) WithEncryption(enc *PayloadEncryption, enabled bool) *MySQLStore {
	cp := *s
	cp.encryption = enc
	cp.encryptSensitivePayloads = enabled
	return &cp
}

// WithLogger returns a copy of the store with the given structured logger.
func (s *MySQLStore) WithLogger(l *slog.Logger) *MySQLStore {
	cp := *s
	cp.logger = l
	return &cp
}

// log returns the configured logger or the default logger.
func (s *MySQLStore) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// requireTenant rejects an operation attempted with no tenant set.
//
// MSSQLStore has had this check since it was written, in setSessionContext:
// "tenant ID must be set before setting session context for an RLS-scoped
// transaction". MySQL had no equivalent anywhere — 90 references to s.tenantID
// across mysql_ops.go, mysql_events.go, mysql_lifecycle.go and this file, and
// not one guard. An empty tenantID simply produced a comparison against the
// empty string, which matches nothing, returns no rows and no error, and so
// reads to the caller as "this tenant has no data" rather than "this query had
// no identity".
//
// This was invisible because TestUnauthenticatedQueryRejection's type switch
// had no case for *MySQLStore: the MySQL subtest fell through to default: and
// skipped itself, unconditionally, every time — including in multi-db-ci.yml's
// test-mysql job, which exists to test MySQL. The conditional-skip audit turned
// that skip into a failure, which is how this surfaced.
//
// Scope, stated plainly: this guard is currently applied to
// GetActiveInstanceCountsByVersion only, the one method the test covers. Do not
// read the presence of this helper as a claim that MySQL tenant scoping is
// enforced.
//
// This used to point at "auditing the other ~89 tenant-scoped MySQL call
// sites" as IMPROVEMENT-PLAN.md 1.7 work that "is not done". Corrected
// 2026-08-31: tiers.yaml records multi-tenancy-mysql as NOT SUPPORTED --
// single-tenant only (D1, 2026-08-06), "a documented product boundary, not an
// open engineering item", because emulating RLS costs 6.1x on scans. So there
// is no tenant-isolation audit outstanding here, and a comment sending the
// next reader at one is worse than no comment.
//
// What this guard is actually for survives that decision, which is why it
// stays: it is a wrong-answer guard, not a multi-tenancy guard. An empty
// tenantID makes every tenant_id predicate a comparison against the empty
// string -- no rows, no error -- so a query with no identity reads to the
// caller as "this tenant has no data". That is just as wrong in the
// single-tenant deployment MySQL is supported for.
func (s *MySQLStore) requireTenant(op string) error {
	if s.tenantID == "" {
		return fmt.Errorf("%s: tenant ID must be set; MySQL has no row-level "+
			"security, so an unscoped query has no database-level backstop", op)
	}
	return nil
}

// WithTenant returns a copy of the store scoped to the given tenant ID.
// This is used in the dispatch loop to set the correct tenant context
// before executing a workflow. The returned store's methods will add
// WHERE tenant_id = ? to every tenant-scoped query.
func (s *MySQLStore) WithTenant(tenantID string) *MySQLStore {
	cp := *s
	cp.tenantID = tenantID
	return &cp
}

// beginTx starts a new transaction. MySQL has no RLS equivalent, so no
// additional setup is needed -- tenant isolation is handled by explicit
// WHERE tenant_id = ? clauses on every query.
func (s *MySQLStore) beginTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	return tx, nil
}

// inClausePlaceholders returns a comma-separated list of n "?" placeholders
// for use in MySQL IN (...)-clauses.
func inClausePlaceholders(n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = "?"
	}
	return strings.Join(parts, ", ")
}

// taskQueueClause returns the IN-clause SQL fragment and argument slice
// for filtering by the store's configured task queues.
func (s *MySQLStore) taskQueueClause() (string, []any) {
	phs := make([]string, len(s.taskQueues))
	args := make([]any, len(s.taskQueues))
	for i, tq := range s.taskQueues {
		phs[i] = "?"
		args[i] = tq
	}
	return strings.Join(phs, ", "), args
}

// ---------------------------------------------------------------------------
// RequestCancellation / CheckCancellation
// ---------------------------------------------------------------------------

// RequestCancellation sets the cancellation flag on a workflow.
func (s *MySQLStore) RequestCancellation(ctx context.Context, workflowID, reason string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("request cancellation: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET cancellation_requested = true, cancellation_reason = ?
		WHERE id = ? AND tenant_id = ?
	`, reason, workflowID, s.tenantID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CheckCancellation checks if a workflow has been cancelled.
func (s *MySQLStore) CheckCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	var cancelled bool
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT cancellation_requested, cancellation_reason
		FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, workflowID, s.tenantID).Scan(&cancelled, &reason)
	if err != nil {
		return false, "", err
	}
	return cancelled, reason.String, nil
}

// ---------------------------------------------------------------------------
// StartChildWorkflow / GetChildResult
// ---------------------------------------------------------------------------

// StartChildWorkflow creates a child workflow instance linked to a parent.
// defVersion is the explicit workflow definition version to use, or 0 to use
// default resolution (SELECT MAX(version)).
func (s *MySQLStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	runID := uuid.New().String()

	var err error
	if defVersion > 0 {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
			VALUES (?, ?, ?, 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT t.task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS t), 'default'),
			        ?, ?)
		`, runID, defName, defVersion, inputJSON, parentID, parentClosePolicy, parentID, s.tenantID, priority)
	} else {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
			VALUES (?, ?, (SELECT COALESCE(MAX(version), 0) FROM workflow_defs WHERE name = ? AND NOT deprecated AND tenant_id = ?), 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT t.task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS t), 'default'),
			        ?, ?)
		`, runID, defName, defName, s.tenantID, inputJSON, parentID, parentClosePolicy, parentID, s.tenantID, priority)
	}
	if err != nil {
		return "", fmt.Errorf("start child workflow: %w", err)
	}
	return runID, nil
}

// StartChildWorkflowAtomic creates a child workflow and records the parent's
// child_workflow event in a single transaction, guaranteeing exactly-once creation.
func (s *MySQLStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	if childID == "" {
		childID = uuid.New().String()
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. INSERT child workflow instance.
	if defVersion > 0 {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
			VALUES (?, ?, ?, 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT t.task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS t), 'default'),
			        ?, ?)
		`, childID, defName, defVersion, inputJSON, parentID, parentClosePolicy, parentID, s.tenantID, priority)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, parent_close_policy, task_queue, tenant_id, priority)
			VALUES (?, ?, (SELECT COALESCE(MAX(version), 0) FROM workflow_defs WHERE name = ? AND NOT deprecated AND tenant_id = ?), 'ready', ?, ?,
			        COALESCE(NULLIF(?, ''), 'ABANDON'),
			        COALESCE((SELECT t.task_queue FROM (SELECT task_queue FROM workflow_instances WHERE id = ?) AS t), 'default'),
			        ?, ?)
		`, childID, defName, defName, s.tenantID, inputJSON, parentID, parentClosePolicy, parentID, s.tenantID, priority)
	}
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert child: %w", err)
	}

	// 2. INSERT IGNORE child_workflow event into parent's event_history.
	event.RunID = childID
	// previousStoredChecksum, not a hand-rolled read: it runs on tx (so it sees
	// this transaction and carries its RLS/tenant context), qualifies by
	// tenant_id, and distinguishes "no predecessor" from a failed read. The
	// copy that used to be here ran on s.db -- the raw pool, no RLS context --
	// and discarded the error, so under a non-superuser role it silently
	// checksummed against an empty predecessor and broke the chain.
	prevCS, err := s.previousStoredChecksum(ctx, tx, parentID, event.Step)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: previous checksum: %w", err)
	}
	checksum := computeEventChecksum(event, prevCS)
	// See PostgresStore.StartChildWorkflowAtomic for why the payload column
	// matters here: it carries the fields the checksum covers that have no
	// column of their own, and LoadEventHistory restores them from it. Omitted,
	// every child_workflow event failed VerifyWorkflowEvents.
	payloadJSON, _ := eventRecordToPayload(event)
	payloadArg := nullStr("")
	if len(payloadJSON) > 0 {
		payloadArg = sql.NullString{String: string(payloadJSON), Valid: true}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT IGNORE INTO event_history (workflow_id, step, event_type, child_name, child_input, run_id, created_at, checksum, tenant_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, parentID, event.Step, string(event.EventType),
		nullStr(event.ChildName), nullStr(event.ChildInput), nullStr(childID),
		time.UnixMilli(event.TimestampMs), checksum, s.tenantID, payloadArg)
	if err != nil {
		return "", fmt.Errorf("start child workflow atomic: insert event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("start child workflow atomic: commit: %w", err)
	}
	return childID, nil
}

// GetChildResult checks whether a child workflow has completed and returns its result.
func (s *MySQLStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	// Resolve the chain first -- see PostgresStore.GetChildResult for why: a
	// child that continued as new leaves its first run 'done' with an empty
	// result, and that is the run the parent holds the id of (cleat#955).
	runID, err := terminalRunID(ctx, runID, s.successorOfRun)
	if err != nil {
		return "", false, err
	}
	var result string
	var status string
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(result, '{}'), status FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, runID, s.tenantID).Scan(&result, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get child result: %w", err)
	}
	if status == "done" || status == "failed" {
		return compactJSONString(result), true, nil
	}
	return "", false, nil
}

// ---------------------------------------------------------------------------
// GetQueryState
// ---------------------------------------------------------------------------

// GetQueryState returns the query state for a workflow instance key.
func (s *MySQLStore) GetQueryState(ctx context.Context, workflowID, key string) (string, error) {
	var value sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT JSON_UNQUOTE(JSON_EXTRACT(query_state, ?)) FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, "$."+key, workflowID, s.tenantID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get query state: %w", err)
	}
	return value.String, nil
}

// ---------------------------------------------------------------------------
// DeliverSignal / PollSignal / PollCancellation / PollAndClaimSignal
// ---------------------------------------------------------------------------

// DeliverSignal stores a signal for a workflow, as its own row.
//
// It used ON DUPLICATE KEY UPDATE until IMPROVEMENT-PLAN 3.215, which made a
// second signal of the same name silently replace the first. The clause is
// gone because the duplicate key is gone.
func (s *MySQLStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("deliver signal: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
		VALUES (?, ?, ?, ?)
	`, workflowID, signalName, encodeJSONPayload(payload), s.tenantID)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances
		SET signal_seq = signal_seq + 1,
		    next_wake_at = CASE WHEN status = 'ready' THEN NOW(6) ELSE next_wake_at END
		WHERE id = ? AND tenant_id = ?
	`, workflowID, s.tenantID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// PollSignal returns the oldest unconsumed delivery with this name without
// consuming it. This is non-destructive — the delivery remains available until
// ConsumeSignal removes it by id.
func (s *MySQLStore) PollSignal(ctx context.Context, workflowID, signalName string) (SignalDelivery, bool, error) {
	var id int64
	var payload string
	var deliveredAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT id, payload, delivered_at FROM workflow_signals
		WHERE workflow_id = ? AND signal_name = ? AND tenant_id = ?
		ORDER BY id
		LIMIT 1
	`, workflowID, signalName, s.tenantID).Scan(&id, &payload, &deliveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SignalDelivery{}, false, nil
	}
	if err != nil {
		return SignalDelivery{}, false, fmt.Errorf("poll signal: %w", err)
	}
	return SignalDelivery{
		ID:            id,
		Payload:       decodeJSONPayload(payload),
		DeliveredAtMs: deliveredAt.UnixMilli(),
	}, true, nil
}

// PollCancellation checks whether the workflow has been cancelled.
func (s *MySQLStore) PollCancellation(ctx context.Context, workflowID string) (bool, string, error) {
	return s.CheckCancellation(ctx, workflowID)
}

// GetAllowedSignalCallers returns the allowed_signals list for a workflow.
func (s *MySQLStore) GetAllowedSignalCallers(ctx context.Context, workflowID string) ([]string, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT allowed_signals FROM workflow_instances WHERE id = ? AND tenant_id = ?`,
		workflowID, s.tenantID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get allowed signal callers: %w", err)
	}
	if !raw.Valid || raw.String == "" || raw.String == "null" {
		return nil, nil
	}
	var callers []string
	if err := json.Unmarshal([]byte(raw.String), &callers); err != nil {
		return nil, fmt.Errorf("get allowed signal callers: parse: %w", err)
	}
	return callers, nil
}

// SetAllowedSignalCallers replaces the allowed_signals list for a workflow.
// See PostgresStore.SetAllowedSignalCallers. IMPROVEMENT-PLAN 3.15.
//
// Scoped by tenant: MySQL has no row-level security, so this predicate is the
// whole of the isolation -- without it one tenant could grant callers on
// another tenant's workflow by id.
func (s *MySQLStore) SetAllowedSignalCallers(ctx context.Context, workflowID string, callers []string) error {
	encoded, err := encodeAllowedSignals(callers)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE workflow_instances SET allowed_signals = ? WHERE id = ? AND tenant_id = ?`,
		encoded, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("set allowed signal callers: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set allowed signal callers: rows affected: %w", err)
	}
	if n == 0 {
		// MySQL reports 0 for "matched but unchanged" as well as "no such row",
		// so confirm the row is genuinely absent before reporting not-found --
		// otherwise setting a list to the value it already holds looks like a
		// missing workflow.
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM workflow_instances WHERE id = ? AND tenant_id = ?`,
			workflowID, s.tenantID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWorkflowNotFound
		}
		if err != nil {
			return fmt.Errorf("set allowed signal callers: confirm: %w", err)
		}
	}
	return nil
}

// PollAndClaimSignal atomically checks for and claims a pending signal.
// ConsumeSignal removes one delivery by id.
//
// No SELECT ... FOR UPDATE, and no transaction: the previous method here read
// and deleted in one step, which needed a lock to be atomic. Deleting a known
// id is already atomic, and deleting one that is already gone is the
// documented no-op, so there is nothing left for the lock to protect.
func (s *MySQLStore) ConsumeSignal(ctx context.Context, workflowID string, id int64) error {
	// The consumed counter, bumped in the same statement batch as the delete.
	//
	// finalize wakes a segment that CONSUMED something and still has rows
	// waiting, because a segment that consumed once can consume again --
	// progress is what separates a burst worth draining from an unrelated
	// pending signal that would spin (cleat#953). Bumped here rather than
	// through a new store method, because ConsumeSignal already writes.
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM workflow_signals
		WHERE id = ? AND workflow_id = ? AND tenant_id = ?
	`, id, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("consume signal: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET signal_consumed_seq = signal_consumed_seq + 1
		WHERE id = ? AND tenant_id = ?
	`, workflowID, s.tenantID); err != nil {
		return fmt.Errorf("consume signal: bump consumed counter: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// UpdateStickyWorker / ClearStickyWorker
// ---------------------------------------------------------------------------

// UpdateStickyWorker sets the sticky worker for a workflow.
func (s *MySQLStore) UpdateStickyWorker(ctx context.Context, workflowID, workerID string) error {
	// The `AND tenant_id` is load-bearing HERE and absent on the other two
	// dialects on purpose: MySQL has no row-level security, so this statement
	// is the tenant boundary. Postgres enforces it with a FOR ALL RLS policy
	// and SQL Server with session-context policies -- see the note on
	// PostgresStore.UpdateStickyWorker before concluding the others have a gap.
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = ? WHERE id = ? AND tenant_id = ?
	`, workerID, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("update sticky worker: %w", err)
	}
	return nil
}

// ClearStickyWorker removes the sticky worker assignment.
func (s *MySQLStore) ClearStickyWorker(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET sticky_worker_id = NULL WHERE id = ? AND tenant_id = ?
	`, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("clear sticky worker: %w", err)
	}
	return nil
}

// ---- ReleaseWorkflowConcurrencyKeys ----

// ReleaseWorkflowConcurrencyKeys releases all concurrency keys held by a workflow.
// Runs as a best-effort operation.
func (s *MySQLStore) ReleaseWorkflowConcurrencyKeys(ctx context.Context, workflowID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM concurrency_keys WHERE workflow_id = ? AND tenant_id = ?`, workflowID, s.tenantID)
	if err != nil {
		return fmt.Errorf("release workflow concurrency keys: %w", err)
	}
	return nil
}

// ResolveTenantFromAPIKey looks up a tenant UUID by API key hash.
func (s *MySQLStore) ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM tenant_api_keys
		 WHERE key_hash = ? AND revoked_at IS NULL`, keyHash).Scan(&tenantID)
	if err != nil {
		return uuid.Nil, err
	}
	return tenantID, nil
}

// ---------------------------------------------------------------------------
// MySQLStoreFactory
// ---------------------------------------------------------------------------

// MySQLStoreFactory implements StoreFactory for MySQL/MariaDB with
// per-tenant database isolation. Each tenant gets its own MySQL database
// (cleat_<tenant_id>), and the connection pool is scoped to that database.
type MySQLStoreFactory struct {
	mu sync.RWMutex

	// masterDB is connected without a default database — used for
	// CREATE DATABASE and other administrative operations.
	masterDB *sql.DB

	// baseDSN is a DSN template used to open per-tenant connections.
	// It should include the base connection parameters (user, password,
	// host, port, params) but NOT the database name. The database name
	// is appended per tenant.
	// Example: "user:pass@tcp(localhost:3306)/?parseTime=true&multiStatements=true"
	baseDSN string

	// tenantDBs maps tenantID -> per-tenant connection pool.
	tenantDBs map[string]*sql.DB

	idempotencyKeyTTL  time.Duration
	tenantPoolMaxConns int

	logger *slog.Logger
}

// NewMySQLStoreFactory creates a MySQLStoreFactory.
// masterDB is a *sql.DB connected without a default database (used for
// administrative operations like CREATE DATABASE). baseDSN is a DSN template
// with connection parameters but without a database name — the per-tenant
// database name is appended for each tenant's connection.
func NewMySQLStoreFactory(masterDB *sql.DB, baseDSN string, idempotencyKeyTTL ...time.Duration) *MySQLStoreFactory {
	ttl := 720 * time.Hour
	if len(idempotencyKeyTTL) > 0 {
		ttl = idempotencyKeyTTL[0]
	}
	return &MySQLStoreFactory{
		masterDB:           masterDB,
		baseDSN:            baseDSN,
		tenantDBs:          make(map[string]*sql.DB),
		idempotencyKeyTTL:  ttl,
		tenantPoolMaxConns: 25,
	}
}

// WithLogger sets the structured logger on the factory. Stores created by
// OpenStore will inherit it.
func (f *MySQLStoreFactory) WithLogger(l *slog.Logger) *MySQLStoreFactory {
	f.logger = l
	return f
}

// WithTenantPoolMaxConns sets the max open connections per tenant pool.
func (f *MySQLStoreFactory) WithTenantPoolMaxConns(n int) *MySQLStoreFactory {
	if n > 0 {
		f.tenantPoolMaxConns = n
	}
	return f
}

// buildTenantDSN inserts the database name into the base DSN.
// The baseDSN has connection parameters without a database name, e.g.
// "user:pass@tcp(host:port)/?parseTime=true". The database name is
// inserted after the last '/' and before any '?' query parameters.
func (f *MySQLStoreFactory) buildTenantDSN(dbName string) string {
	base := f.baseDSN
	slash := strings.LastIndex(base, "/")
	if slash < 0 {
		return base + dbName
	}
	return base[:slash+1] + dbName + base[slash+1:]
}

// CreateTenantDatabase creates a new database for the given tenant and
// returns a connection pool scoped to that database. It is idempotent —
// if the database already exists, it just opens a new pool to it.
func (f *MySQLStoreFactory) CreateTenantDatabase(ctx context.Context, tenantID string) (*sql.DB, error) {
	// tenantID must be a valid UUID to prevent SQL injection through
	// backtick-quoted identifiers.
	if _, err := uuid.Parse(tenantID); err != nil {
		return nil, fmt.Errorf("invalid tenant ID %q: %w", tenantID, err)
	}
	// Replace hyphens with underscores for use as a database name suffix.
	dbName := "cleat_" + strings.ReplaceAll(tenantID, "-", "_")

	f.mu.Lock()
	defer f.mu.Unlock()

	// Check if we already have a pool for this tenant.
	if existing, ok := f.tenantDBs[tenantID]; ok {
		return existing, nil
	}

	// Create the database via the master connection.
	_, err := f.masterDB.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+dbName+"`")
	if err != nil {
		return nil, fmt.Errorf("create tenant database %s: %w", dbName, err)
	}

	// Open a new connection pool scoped to this tenant's database.
	tenantDSN := f.buildTenantDSN(dbName)
	tenantDB, err := sql.Open("mysql", tenantDSN)
	if err != nil {
		return nil, fmt.Errorf("open tenant pool %s: %w", dbName, err)
	}

	// Configure the pool.
	tenantDB.SetMaxOpenConns(f.tenantPoolMaxConns)
	tenantDB.SetMaxIdleConns(max(2, f.tenantPoolMaxConns/5))
	tenantDB.SetConnMaxLifetime(5 * time.Minute)

	// Verify connectivity.
	if err := tenantDB.PingContext(ctx); err != nil {
		tenantDB.Close()
		return nil, fmt.Errorf("ping tenant db %s: %w", dbName, err)
	}

	f.tenantDBs[tenantID] = tenantDB
	return tenantDB, nil
}

// DropTenantDatabase removes a tenant database and closes its connection pool.
func (f *MySQLStoreFactory) DropTenantDatabase(tenantID string) error {
	dbName := "cleat_" + strings.ReplaceAll(tenantID, "-", "_")

	f.mu.Lock()
	defer f.mu.Unlock()

	if db, ok := f.tenantDBs[tenantID]; ok {
		db.Close()
		delete(f.tenantDBs, tenantID)
	}

	_, err := f.masterDB.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
	return err
}

// getOrCreateTenantDB returns the connection pool for a tenant,
// creating the tenant database if needed.
func (f *MySQLStoreFactory) getOrCreateTenantDB(ctx context.Context, tenantID string) (*sql.DB, error) {
	f.mu.RLock()
	db, ok := f.tenantDBs[tenantID]
	f.mu.RUnlock()
	if ok {
		return db, nil
	}
	return f.CreateTenantDatabase(ctx, tenantID)
}

// TenantDB returns a *sql.DB connection pool for the given tenant, creating
// a new per-tenant database and connection pool if one does not already exist.
func (f *MySQLStoreFactory) TenantDB(ctx context.Context, tenantID string) (*sql.DB, error) {
	return f.getOrCreateTenantDB(ctx, tenantID)
}

// OpenStore creates a MySQLStore scoped to the given tenant and task queues.
// The store's connection pool is scoped to the tenant's database.
//
// NOTE: Encryption at rest (--encrypt-sensitive-payloads) is not yet supported
// on MySQL backends. See PostgresStore.WithEncryption for the reference implementation.
func (f *MySQLStoreFactory) OpenStore(ctx context.Context, tenantID string, taskQueues ...string) (WorkflowStore, io.Closer, error) {
	tenantDB, err := f.getOrCreateTenantDB(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("open store for tenant %s: %w", tenantID, err)
	}

	store := NewMySQLStore(tenantDB, taskQueues...)
	store.tenantID = tenantID
	store = store.WithLogger(f.logger)
	// Set last: WithLogger returns a copy, so anything set before it survives
	// only by accident of struct copying. See ClaimWorkflowsAcrossTenants.
	store.perTenantDatabase = true
	return store, nopCloser{}, nil
}

// Close closes all tenant connection pools.
func (f *MySQLStoreFactory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for tenantID, db := range f.tenantDBs {
		db.Close()
		delete(f.tenantDBs, tenantID)
	}
	return nil
}

// DriverName returns "mysql".
func (f *MySQLStoreFactory) DriverName() string { return "mysql" }

// Dialect returns DialectMySQL.
func (f *MySQLStoreFactory) Dialect() Dialect { return DialectMySQL }

// GetChildCompletedAtMs returns the child's completion instant in Unix
// milliseconds. See ChildWorkflowStore and engine/children.go's
// pollChildIsDeterministic. This is the DATABASE clock.
func (s *MySQLStore) GetChildCompletedAtMs(ctx context.Context, runID string) (int64, bool, error) {
	var completedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT completed_at FROM workflow_instances WHERE id = ? AND tenant_id = ?
	`, runID, s.tenantID).Scan(&completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get child completed_at: %w", err)
	}
	if !completedAt.Valid {
		return 0, false, nil
	}
	return completedAt.Time.UnixMilli(), true, nil
}
