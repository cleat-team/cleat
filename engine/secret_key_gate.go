package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/cleat-team/cleat/plugin"
)

// THE SECRET-KEY WRITE GATE (cleat#1991, option A).
//
// A rotation must never leave a serving worker holding a row it cannot open
// (specs/CleatKeyRotation.tla, invariant S1). Two mechanisms carry that, and
// the model shows neither can stand in for the other:
//
//	boot check  a NEW worker meeting OLD rows   -- worker.checkSecretsUsable
//	write gate  a NEW write meeting OLD workers -- this file
//
// The gate is: a writer refuses to write key version v unless every LIVE worker
// registered in admin.workers can open v.
//
// # Why a named lock, and what it refines
//
// The model's WriteGate = "registry" treats two spans as atomic with respect to
// each other:
//
//	WORKER span   {register (publish my key set); boot-check (read every row)}
//	WRITER span   {read the registry; write the row}
//
// If they interleave, a writer can read the registry before a worker registers
// and write the row after that worker's boot check has already read the table:
// the worker then serves a row it cannot open. That is the counterexample the
// model finds for WriteGate = "observed", and the reason "deploy, wait, then
// reseal" is not safe.
//
// Both spans take ONE named database lock first and hold it to their end: a
// worker takes it SHARED and a writer takes it EXCLUSIVE. Shared/exclusive
// exclusion is exactly "a worker span and a writer span never overlap, and
// worker spans do not exclude one another". So the interleaving above is
// unreachable, and the two orders that remain are the two the model checks:
//
//	writer span first  -> it commits before the worker's span starts, so the
//	                      worker's boot check reads the row it wrote;
//	worker span first  -> the worker's row is committed before the writer's
//	                      span starts, so the writer's registry read sees it.
//
// It is a lock rather than the SERIALIZABLE transaction the design comment on
// #1991 proposed because the boot check reads one tenant per transaction under
// row-level security, and because SERIALIZABLE means SSI predicate locks on
// PostgreSQL, shared locks on every plain read on MySQL and range locks on SQL
// Server -- three behaviours to get right where a lock is one obligation.
//
// # Scope of the lock, per dialect
//
//	PostgreSQL  pg_advisory_xact_lock[_shared], released by COMMIT or ROLLBACK.
//	SQL Server  sp_getapplock, owner Transaction, released the same way.
//	MySQL       GET_LOCK, which is SESSION-scoped: it survives COMMIT and stays
//	            with the connection, and it is EXCLUSIVE ONLY, so MySQL workers
//	            serialise their boot checks with one another. MySQL is
//	            single-tenant and small; that cost is accepted. Because it is
//	            session-scoped it is taken on a dedicated *sql.Conn and released
//	            in a defer on every path -- see withMySQLGate.
//
// # What is NOT covered
//
// "Live" is a heartbeat within SecretKeyLiveWindow. A worker stalled for longer
// than that while STILL SERVING is invisible to a writer. The model has no
// clock and does not check liveness. A worker whose membership loop notices its
// own gap re-registers and re-checks (cmd/cleat-worker/worker_membership.go),
// which bounds the exposure to the stall itself; it does not remove it.
// Workers older than this feature are invisible if they do not register.
const (
	keyGateName = "cleat.secret_key_gate"

	// keyGatePGKey is the advisory-lock key. Distinct from
	// migration.migrationsLockKey (...561) and plugin.pluginMigrationsLockKey (...562).
	keyGatePGKey int64 = 7215842093104563

	// SecretKeyLiveWindow is how recent a heartbeat must be for a writer to
	// count a worker. Deliberately generous: a crashed worker's row is removed
	// within the workers' own stale window by any live worker's sweep, so this
	// only matters when NO worker is left to sweep, and then it stops a dead
	// row blocking writes forever. Being too lenient is safe (a restarting
	// worker re-reads the table); being too strict is what would hide a
	// serving worker.
	SecretKeyLiveWindow = 5 * time.Minute
)

// keyGateWriterWait and keyGateWorkerWait bound the lock waits. A writer waits
// behind booting workers; a booting worker waits behind a writer. Variables
// only so a test can shorten them; nothing else assigns them.
var (
	keyGateWriterWait = 15 * time.Second
	keyGateWorkerWait = 45 * time.Second
)

type keyGateMode int

const (
	keyGateShared keyGateMode = iota
	keyGateExclusive
)

func (m keyGateMode) String() string {
	if m == keyGateShared {
		return "shared"
	}
	return "exclusive"
}

// KeyGateBusyError is a lock wait that timed out. It says who the caller was
// waiting for, because "timed out" on its own reads as a hang.
type KeyGateBusyError struct {
	Mode keyGateMode
	Wait time.Duration
}

func (e *KeyGateBusyError) Error() string {
	if e.Mode == keyGateExclusive {
		return fmt.Sprintf("the secret-key gate is busy: waiting %s for workers that are booting "+
			"(each holds it while it checks every stored secret) -- try again shortly", e.Wait)
	}
	return fmt.Sprintf("the secret-key gate is busy: waited %s for a secret write to finish "+
		"(a set-secret or reseal-secrets is running) -- start this worker again shortly", e.Wait)
}

// WorkerKeys is what one registry row says about the keys a worker holds.
type WorkerKeys struct {
	WorkerID string
	Hostname string
	PID      int
	// Known is false when the row carries no key set at all: a worker older
	// than this feature that registered for the connection budget.
	Known    bool
	Versions []int
}

// opens reports whether this worker can open rows sealed at version v.
//
// A row with no key set is a worker that predates rotation, and every worker
// that predates rotation holds exactly one key, which is version 1. Reading it
// as {1} rather than as "nothing" is what leaves a rolling upgrade of a
// deployment that never rotates unaffected: every write there is at version 1.
func (w WorkerKeys) opens(v int) bool {
	if !w.Known {
		return v == 1
	}
	for _, have := range w.Versions {
		if have == v {
			return true
		}
	}
	return false
}

func (w WorkerKeys) describe() string {
	who := w.Hostname
	if who == "" {
		who = "(unknown host)"
	}
	id := w.WorkerID
	if len(id) > 8 {
		id = id[:8]
	}
	label := fmt.Sprintf("%s (pid %d, worker %s)", who, w.PID, id)
	switch {
	case !w.Known:
		return label + " predates key rotation and is assumed to hold version 1 only"
	case len(w.Versions) == 0:
		return label + " has no master key configured"
	default:
		return fmt.Sprintf("%s opens versions %v", label, w.Versions)
	}
}

// SecretKeyGateError refuses a write. It names every worker that blocks it.
type SecretKeyGateError struct {
	Version  int
	Blocking []WorkerKeys
}

func (e *SecretKeyGateError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to write key version %d: %d live worker(s) cannot open it:", e.Version, len(e.Blocking))
	keyless := false
	for _, w := range e.Blocking {
		b.WriteString("\n  - " + w.describe())
		if w.Known && len(w.Versions) == 0 {
			keyless = true
		}
	}
	if keyless {
		b.WriteString("\nA worker with no master key would fail on the first workflow that resolves this " +
			"secret. Configure CLEAT_SECRET_MASTER_KEY on it, or wait for it to stop.")
	}
	b.WriteString("\nRoll the key ring out to every worker first, or wait for these workers to stop " +
		"(a worker leaves the registry within seconds of stopping). Workers list the versions " +
		"they can open in their startup log.")
	return b.String()
}

// mayWrite returns the workers that block a write at version v.
func mayWrite(v int, workers []WorkerKeys) []WorkerKeys {
	var blocking []WorkerKeys
	for _, w := range workers {
		if !w.opens(v) {
			blocking = append(blocking, w)
		}
	}
	sort.Slice(blocking, func(i, j int) bool { return blocking[i].WorkerID < blocking[j].WorkerID })
	return blocking
}

// encodeKeyVersions is the column's representation: comma-separated integers,
// ascending. The empty string is a worker with no key, which is not NULL --
// NULL means "did not say".
func encodeKeyVersions(vs []int) string {
	sorted := append([]int(nil), vs...)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, v := range sorted {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// decodeKeyVersions is the inverse. A value it cannot parse yields NO versions,
// so a corrupt row blocks writes instead of admitting them.
func decodeKeyVersions(s string) []int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// liveKeySets reads every live worker's key set on q, which is the connection
// or transaction that holds the gate.
//
//nolint:gosec // G202: fragments are constants from WorkerRegistry's closed switches.
func (r *WorkerRegistry) liveKeySets(ctx context.Context, q querier, window time.Duration) ([]WorkerKeys, error) {
	query := r.stmt(
		"SELECT worker_id, hostname, pid, secret_key_versions FROM ", r.table(),
		" WHERE last_heartbeat_at > ", r.Dialect.intervalExpr(1))
	rows, err := q.QueryContext(ctx, query, cutoffSeconds(window))
	if err != nil {
		return nil, fmt.Errorf("worker registry: read live key sets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkerKeys
	for rows.Next() {
		var w WorkerKeys
		var hostname sql.NullString
		var csv sql.NullString
		if err := rows.Scan(&w.WorkerID, &hostname, &w.PID, &csv); err != nil {
			return nil, fmt.Errorf("worker registry: scan live key sets: %w", err)
		}
		w.Hostname = hostname.String
		w.Known = csv.Valid
		if csv.Valid {
			w.Versions = decodeKeyVersions(csv.String)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// checkMayWrite is the writer's half: run it on the connection that holds the
// gate exclusively, immediately before the write on that same connection.
func (s *SecretStore) checkMayWrite(ctx context.Context, q querier, version int) error {
	reg := &WorkerRegistry{Dialect: Dialect(s.dialect)}
	workers, err := reg.liveKeySets(ctx, q, SecretKeyLiveWindow)
	if err != nil {
		return err
	}
	if blocking := mayWrite(version, workers); len(blocking) > 0 {
		return &SecretKeyGateError{Version: version, Blocking: blocking}
	}
	return nil
}

// acquireKeyGate takes the gate on q. On PostgreSQL and SQL Server q is a
// transaction and the lock is released when it ends; on MySQL q is a dedicated
// connection and the caller must release.
func acquireKeyGate(ctx context.Context, q querier, dialect string, mode keyGateMode, wait time.Duration) error {
	busy := &KeyGateBusyError{Mode: mode, Wait: wait}
	switch dialect {
	case "mysql":
		var got sql.NullInt64
		secs := int(wait / time.Second)
		if secs < 1 {
			secs = 1
		}
		if err := q.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, keyGateName, secs).Scan(&got); err != nil {
			return fmt.Errorf("secret-key gate: GET_LOCK: %w", err)
		}
		switch {
		case !got.Valid:
			return fmt.Errorf("secret-key gate: GET_LOCK failed (returned NULL)")
		case got.Int64 == 0:
			return busy
		}
		return nil
	case "mssql":
		var code int
		stmt := `DECLARE @r int; EXEC @r = sp_getapplock @Resource = N'` + keyGateName +
			`', @LockMode = N'Shared', @LockOwner = N'Transaction', @LockTimeout = @p1; SELECT @r`
		if mode == keyGateExclusive {
			stmt = `DECLARE @r int; EXEC @r = sp_getapplock @Resource = N'` + keyGateName +
				`', @LockMode = N'Exclusive', @LockOwner = N'Transaction', @LockTimeout = @p1; SELECT @r`
		}
		if err := q.QueryRowContext(ctx, stmt, int(wait/time.Millisecond)).Scan(&code); err != nil {
			return fmt.Errorf("secret-key gate: sp_getapplock: %w", err)
		}
		switch {
		case code >= 0:
			return nil
		case code == -1:
			return busy
		default:
			return fmt.Errorf("secret-key gate: sp_getapplock returned %d", code)
		}
	default:
		if _, err := q.ExecContext(ctx, `SELECT set_config('lock_timeout', $1, true)`,
			strconv.Itoa(int(wait/time.Millisecond))+"ms"); err != nil {
			return fmt.Errorf("secret-key gate: set lock_timeout: %w", err)
		}
		stmt := `SELECT pg_advisory_xact_lock_shared($1)`
		if mode == keyGateExclusive {
			stmt = `SELECT pg_advisory_xact_lock($1)`
		}
		if _, err := q.ExecContext(ctx, stmt, keyGatePGKey); err != nil {
			var pqErr *pq.Error
			if errors.As(err, &pqErr) && pqErr.Code == "55P03" {
				return busy
			}
			return fmt.Errorf("secret-key gate: advisory lock: %w", err)
		}
		return nil
	}
}

// withMySQLGate holds the session-scoped MySQL lock around fn on ONE connection.
//
// GET_LOCK is not transaction-scoped: COMMIT does not release it and a pooled
// connection carries it back into the pool. So the lock is taken on a dedicated
// *sql.Conn and released in a defer that runs on every exit -- an error from
// fn, a failed boot check, a panic. The release uses a fresh context, because
// the caller's is very often what has just been cancelled.
//
// If the release itself fails, the connection is discarded (driver.ErrBadConn
// from Raw), which ends the session and so the lock. A lock that outlived its
// holder would stall every later writer for the full wait.
func withMySQLGate(ctx context.Context, db *sql.DB, mode keyGateMode, wait time.Duration, fn func(q querier) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("secret-key gate: take a connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := acquireKeyGate(ctx, conn, "mysql", mode, wait); err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released sql.NullInt64
		rerr := conn.QueryRowContext(rctx, `SELECT RELEASE_LOCK(?)`, keyGateName).Scan(&released)
		if rerr != nil || !released.Valid || released.Int64 != 1 {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			if err == nil {
				err = fmt.Errorf("secret-key gate: could not release the lock (connection discarded): %v", rerr)
			}
		}
	}()
	return fn(conn)
}

// inGate runs fn holding the gate in the given mode. fn's writes and the gate
// share one transaction (PostgreSQL, SQL Server) or one connection (MySQL).
//
// A tenant in ctx makes the transaction tenant-scoped on PostgreSQL and SQL
// Server, exactly as execTenantScoped does; the lock is taken inside it.
func (s *SecretStore) inGate(ctx context.Context, mode keyGateMode, wait time.Duration, fn func(q querier) error) error {
	if s.dialect == "mysql" {
		return withMySQLGate(ctx, s.db, mode, wait, fn)
	}
	tx, err := beginTenantTx(ctx, s.db, plugin.Dialect(s.dialect), nil)
	if err != nil {
		return err
	}
	if tx == nil {
		if tx, err = s.db.BeginTx(ctx, nil); err != nil {
			return fmt.Errorf("secret-key gate: begin: %w", err)
		}
	}
	// Rollback on EVERY exit, panic included: the lock lives exactly as long as
	// this transaction, and a transaction abandoned by a panic holds it until
	// the connection is collected. Harmless after Commit.
	defer func() { _ = tx.Rollback() }()
	if err := acquireKeyGate(ctx, tx, s.dialect, mode, wait); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// gatedWrite is the write path of set-secret and reseal-secrets: take the gate
// exclusively, refuse unless every live worker can open version, then write --
// all on one connection, so nothing registers between the check and the write.
func (s *SecretStore) gatedWrite(ctx context.Context, version int, fn func(q querier) error) error {
	return s.inGate(ctx, keyGateExclusive, keyGateWriterWait, func(q querier) error {
		if s.beforeGateCheck != nil {
			s.beforeGateCheck()
		}
		if err := s.checkMayWrite(ctx, q, version); err != nil {
			return err
		}
		return fn(q)
	})
}

// RegisterUnderKeyGate is the worker's half: publish this worker's key set and
// run the boot check as ONE span under the shared gate.
//
// Any earlier registration for this worker id is replaced, so the same call
// serves first start and re-registration after a lapse. If check fails the
// registration is withdrawn -- rolled back on PostgreSQL and SQL Server, deleted
// on MySQL -- so a worker that refuses to start does not leave a row that
// blocks writers for the live window.
func (r *WorkerRegistry) RegisterUnderKeyGate(ctx context.Context, reg WorkerRegistration, check func(context.Context) error) error {
	span := func(q querier, withdraw func() error) error {
		if _, err := q.ExecContext(ctx, r.stmt("DELETE FROM ", r.table(),
			" WHERE worker_id = ", r.Dialect.placeholder(1)), reg.WorkerID); err != nil {
			return fmt.Errorf("worker registry: replace registration %s: %w", reg.WorkerID, err)
		}
		if err := r.registerOn(ctx, q, reg); err != nil {
			return err
		}
		// Withdraw on every exit but success. The explicit call below reports a
		// failed withdrawal alongside the check's error; this one is what covers
		// a panic in check, which would otherwise leave the row published.
		settled := false
		defer func() {
			if !settled {
				_ = withdraw()
			}
		}()
		if err := check(ctx); err != nil {
			settled = true
			if werr := withdraw(); werr != nil {
				return fmt.Errorf("%w (and the registration could not be withdrawn: %v)", err, werr)
			}
			return err
		}
		settled = true
		return nil
	}

	if r.Dialect == DialectMySQL {
		return withMySQLGate(ctx, r.DB, keyGateShared, keyGateWorkerWait, func(q querier) error {
			return span(q, func() error {
				_, err := q.ExecContext(ctx, r.stmt("DELETE FROM ", r.table(),
					" WHERE worker_id = ", r.Dialect.placeholder(1)), reg.WorkerID)
				return err
			})
		})
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("worker registry: begin: %w", err)
	}
	// As in inGate: roll back on every exit, a panic in check included.
	defer func() { _ = tx.Rollback() }()
	if err := acquireKeyGate(ctx, tx, string(r.Dialect), keyGateShared, keyGateWorkerWait); err != nil {
		return err
	}
	if err := span(tx, tx.Rollback); err != nil {
		return err
	}
	return tx.Commit()
}
