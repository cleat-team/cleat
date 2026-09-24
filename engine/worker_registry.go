package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WorkerRegistry makes workers countable.
//
// cleat#1487 wants a cluster-global connection budget and records why one could
// not be built: a worker exists in the database only as a value on rows it
// currently holds -- `assigned_to`, refreshed by the heartbeat loop's
// HeartbeatBatchFenced call, which is a per-WORKFLOW heartbeat rather than a
// per-WORKER one. A worker holding no
// claims is invisible, so the workers hardest to see are the freshly started
// and idle ones, which have already opened their fixed pools and are consuming
// connections without doing work.
//
// This is membership and nothing else. It computes no share, sizes no pool and
// changes no behaviour; that is #1487's second half and is deliberately a
// separate change.
//
// NOT a WorkflowStore method, on purpose. WorkflowStore has three dialect
// implementations and test doubles, and every method added to it has to be
// written four or more times. Worker membership needs one table and five
// statements, so it is a small type over *sql.DB instead, built on the
// Dialect helpers in query_builder.go.
type WorkerRegistry struct {
	DB      *sql.DB
	Dialect Dialect
}

// WorkerRegistration is one row of admin.workers.
//
// concurrency and connection_budget are diagnostic rather than load-bearing:
// nothing here reads them. An operator looking at a cluster that will not fit
// its database wants to see how each worker is configured, and the alternative
// -- correlating process flags across hosts by hand -- is the thing this table
// exists to replace.
type WorkerRegistration struct {
	WorkerID         string
	Hostname         string
	PID              int
	Concurrency      int
	ConnectionBudget int
	StartedAt        time.Time
	LastHeartbeatAt  time.Time

	// SecretKeyVersions are the tenant-secret key versions this worker can
	// open, published so a writer can refuse a version some live worker cannot
	// read (cleat#1991, secret_key_gate.go). Written on every registration --
	// nil means "no master key", which is stored as the empty string and is NOT
	// the same as NULL, which means a row that predates this column.
	SecretKeyVersions []int
}

// table is admin.workers everywhere except MySQL, which has no schemas -- its
// administrative tables are unprefixed, as tenants and tenant_api_keys are.
func (r *WorkerRegistry) table() string {
	if r.Dialect == DialectMySQL {
		return "workers"
	}
	return "admin.workers"
}

// cutoffSeconds converts a duration to whole seconds, never below 1.
//
// Zero would make every worker stale including the caller, which reads as "the
// cluster is empty" rather than as "you passed a zero duration".
func cutoffSeconds(maxAge time.Duration) int {
	s := int(maxAge / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// stmt assembles one statement from fragments and is the ONLY place in this
// file where SQL text is built.
//
// Every fragment is a compile-time constant chosen by a closed switch: table()
// returns one of two literals, and Dialect.placeholder, Dialect.nowExpr and
// Dialect.intervalExpr each return one of three, panicking on an unknown
// dialect rather than falling through. No caller-controlled string can reach
// any of them -- the only runtime values in this file are bound parameters.
//
// It exists so that the gosec suppression below is made once, at a site whose
// inputs can be checked in one reading, instead of six times at call sites
// where each would have to be argued separately. Adding a statement here does
// not add a suppression; introducing a fragment that is NOT constant would be
// a change to this function, which is where a reviewer would look.
//
//nolint:gosec // G202: see above -- all fragments are constants from closed switches.
func (r *WorkerRegistry) stmt(parts ...string) string {
	q := ""
	for _, p := range parts {
		q += p
	}
	return q
}

func (r *WorkerRegistry) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return r.DB.ExecContext(ctx, query, args...)
}

// Register records this worker as present. Called once, at startup.
//
// A plain INSERT rather than an upsert, which is what lets this be one
// statement on all three dialects instead of ON CONFLICT / ON DUPLICATE KEY /
// MERGE. It is sound because a worker id is 16 random bytes generated per
// PROCESS (generateWorkerID): a restarted worker is a new row, and its old row
// is removed by Deregister on a clean exit or by SweepExpired on a crash.
func (r *WorkerRegistry) Register(ctx context.Context, reg WorkerRegistration) error {
	return r.registerOn(ctx, r.DB, reg)
}

// registerOn is Register on a caller-chosen connection or transaction, which is
// how RegisterUnderKeyGate makes the registration part of the span that holds
// the secret-key gate.
func (r *WorkerRegistry) registerOn(ctx context.Context, q querier, reg WorkerRegistration) error {
	_, err := q.ExecContext(ctx, r.stmt(
		"INSERT INTO ", r.table(),
		" (worker_id, hostname, pid, concurrency, connection_budget,",
		" started_at, last_heartbeat_at, secret_key_versions) VALUES (",
		r.Dialect.placeholder(1), ", ", r.Dialect.placeholder(2), ", ",
		r.Dialect.placeholder(3), ", ", r.Dialect.placeholder(4), ", ",
		r.Dialect.placeholder(5), ", ", r.Dialect.nowExpr(), ", ", r.Dialect.nowExpr(), ", ",
		r.Dialect.placeholder(6), ")"),
		reg.WorkerID, reg.Hostname, reg.PID, reg.Concurrency, reg.ConnectionBudget,
		encodeKeyVersions(reg.SecretKeyVersions))
	if err != nil {
		return fmt.Errorf("worker registry: register %s: %w", reg.WorkerID, err)
	}
	return nil
}

// Heartbeat renews this worker's lease.
//
// Returns ErrWorkerNotRegistered when it updates nothing, which happens when a
// sweep already removed this worker -- a worker paused longer than the expiry
// window, or one whose clock-blocked heartbeat loop fell behind. Saying so lets
// the caller re-register rather than heartbeat into a row that is not there,
// which no error would otherwise report: an UPDATE matching no rows succeeds.
func (r *WorkerRegistry) Heartbeat(ctx context.Context, workerID string) error {
	res, err := r.exec(ctx, r.stmt(
		"UPDATE ", r.table(), " SET last_heartbeat_at = ", r.Dialect.nowExpr(),
		" WHERE worker_id = ", r.Dialect.placeholder(1)), workerID)
	if err != nil {
		return fmt.Errorf("worker registry: heartbeat %s: %w", workerID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("worker registry: heartbeat %s: rows affected: %w", workerID, err)
	}
	if n > 0 {
		return nil
	}

	// Zero has TWO meanings and only one of them is "the row is gone".
	//
	// MySQL's affected-row count is rows CHANGED, not rows MATCHED -- writing a
	// column its existing value reports 0. Measured directly: an UPDATE setting
	// last_heartbeat_at to the value it already held returned ROW_COUNT() = 0.
	// So a heartbeat that lands inside the resolution of the clock reports
	// exactly what a swept worker reports, and treating the two alike made a
	// live worker de-register and re-register itself every few beats.
	//
	// Dialect.nowExpr() is NOW(6) on MySQL, which makes the collision unlikely
	// rather than impossible, so the ambiguity is resolved rather than
	// narrowed: ask whether the row is there. Only on this path, so an
	// ordinary heartbeat is still one statement.
	var present int
	err = r.DB.QueryRowContext(ctx, r.stmt(
		"SELECT 1 FROM ", r.table(), " WHERE worker_id = ", r.Dialect.placeholder(1)),
		workerID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkerNotRegistered
	}
	if err != nil {
		return fmt.Errorf("worker registry: heartbeat %s: confirm registration: %w", workerID, err)
	}
	return nil
}

// Deregister removes this worker on a clean shutdown, so its share is released
// immediately rather than after the expiry window.
func (r *WorkerRegistry) Deregister(ctx context.Context, workerID string) error {
	if _, err := r.exec(ctx, r.stmt(
		"DELETE FROM ", r.table(), " WHERE worker_id = ", r.Dialect.placeholder(1)),
		workerID); err != nil {
		return fmt.Errorf("worker registry: deregister %s: %w", workerID, err)
	}
	return nil
}

// CountLive returns the number of workers whose heartbeat is within maxAge AND
// that take part in the cluster connection budget.
//
// The second condition exists because registration is no longer conditional on
// --cluster-connection-budget: every worker registers, to publish the secret
// keys it can open. A worker with no budget divides nothing, so counting it
// would shrink every budgeted worker's share for a cluster it is not spending
// from. connection_budget > 0 is the same test the worker used to decide
// whether to register at all.
func (r *WorkerRegistry) CountLive(ctx context.Context, maxAge time.Duration) (int, error) {
	q := r.stmt("SELECT count(*) FROM ", r.table(),
		" WHERE connection_budget > 0 AND last_heartbeat_at > ", r.Dialect.intervalExpr(1))
	var n int
	if err := r.DB.QueryRowContext(ctx, q, cutoffSeconds(maxAge)).Scan(&n); err != nil {
		return 0, fmt.Errorf("worker registry: count live: %w", err)
	}
	return n, nil
}

// ListLive returns the live workers, newest heartbeat last. For diagnostics --
// the budget only needs the count.
func (r *WorkerRegistry) ListLive(ctx context.Context, maxAge time.Duration) ([]WorkerRegistration, error) {
	q := r.stmt(
		"SELECT worker_id, hostname, pid, concurrency, connection_budget,",
		" started_at, last_heartbeat_at FROM ", r.table(),
		" WHERE last_heartbeat_at > ", r.Dialect.intervalExpr(1),
		" ORDER BY last_heartbeat_at")
	rows, err := r.DB.QueryContext(ctx, q, cutoffSeconds(maxAge))
	if err != nil {
		return nil, fmt.Errorf("worker registry: list live: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkerRegistration
	for rows.Next() {
		var w WorkerRegistration
		if err := rows.Scan(&w.WorkerID, &w.Hostname, &w.PID, &w.Concurrency,
			&w.ConnectionBudget, &w.StartedAt, &w.LastHeartbeatAt); err != nil {
			return nil, fmt.Errorf("worker registry: list live: scan: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker registry: list live: %w", err)
	}
	return out, nil
}

// SweepExpired removes workers whose heartbeat predates maxAge, and returns how
// many it removed.
//
// Every worker sweeps; there is no elected sweeper. A DELETE that matches
// nothing is free, and two workers deleting the same expired row is not a
// conflict -- the second one removes zero rows. That is the same reasoning the
// existing reaper runs on, and it avoids needing an election to run a
// tidy-up.
func (r *WorkerRegistry) SweepExpired(ctx context.Context, maxAge time.Duration) (int64, error) {
	res, err := r.exec(ctx, r.stmt(
		"DELETE FROM ", r.table(),
		" WHERE last_heartbeat_at <= ", r.Dialect.intervalExpr(1)), cutoffSeconds(maxAge))
	if err != nil {
		return 0, fmt.Errorf("worker registry: sweep expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("worker registry: sweep expired: rows affected: %w", err)
	}
	return n, nil
}

// ErrWorkerNotRegistered is returned by Heartbeat when no row matched, meaning
// a sweep has already removed this worker.
var ErrWorkerNotRegistered = fmt.Errorf("worker registry: worker is not registered")
