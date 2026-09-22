package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// ErrQueueNotFound is returned for a name this tenant has not registered.
//
// It does NOT distinguish "no such name" from "a name another tenant owns",
// for the same reason ErrSecretNotFound does not: every statement below is
// tenant-scoped, so a foreign name simply is not there.
var ErrQueueNotFound = errors.New("queue not found")

// ErrQueueAlreadyExists is returned by CreateQueue for a name this tenant has
// already registered.
var ErrQueueAlreadyExists = errors.New("queue already exists")

// Queue is a declared concurrency limit and, optionally, a declared rate
// limit. cleat#1116: explicit registration rather than implicit-on-first-use,
// so a name exists before anything starts against it. See
// migrations/postgres/093_a_queue_declares_its_own_concurrency_limit.sql for
// the full design reasoning.
//
// THE CLAIM READS ConcurrencyLimit. A workflow whose concurrency_key equals a
// live queue's name is admitted only while that queue holds fewer than
// ConcurrencyLimit holders; the rest wait and are claimed as slots free. An
// unregistered name -- and a DISABLED queue, which is the same thing to the
// claim's LEFT JOIN -- keeps the bare-key mutex, N=1.
//
// This comment said the opposite until the claim was generalised: it read
// "THIS DOES NOT YET AFFECT THE CLAIM", which was true of the registration-only
// PR it was written in and false the moment the claim landed.
//
// RateLimit / RatePeriodSeconds are cleat#1918: DBOS parity for
// `Queue(name, limiter={limit, period})`, a cap on ADMISSIONS per rolling
// window, independent of ConcurrencyLimit -- a run can be rate-limited with a
// free concurrency slot, and vice versa. Both nil means unlimited, the same
// convention ConcurrencyLimit's own absence (no queues row at all) already
// uses one level up. See
// migrations/postgres/096_a_queue_declares_its_own_rate_limit.sql.
type Queue struct {
	Name              string
	ConcurrencyLimit  int
	RateLimit         *int
	RatePeriodSeconds *int
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DisabledAt        *time.Time
}

// QueueStore holds per-tenant declared queues.
//
// Shaped exactly like SecretStore (engine/tenant_secrets.go): one type
// parameterised by dialect, not three concrete PostgresStore/MySQLStore/
// MSSQLStore implementations, and not a method on the shared Store interface.
// The interface has fourteen test doubles (see
// engine/concurrency_key_holder.go's header for why that count matters); a
// registration-only capability nothing on the claim path calls yet has no
// business forcing all fourteen to grow a method they have no opinion about.
type QueueStore struct {
	db      *sql.DB
	dialect string
}

// NewQueueStore creates a QueueStore for the given dialect.
func NewQueueStore(db *sql.DB, dialect string) *QueueStore {
	return &QueueStore{db: db, dialect: dialect}
}

// CreateQueue registers a new queue for a tenant.
//
// CHECK-THEN-INSERT, not an upsert or a duplicate-key catch, following
// PutSecret's own precedent in this package: every dialect's upsert syntax
// either hides the tenant predicate behind a conflict target or projects
// tenant_id through a USING clause, and cleat's own guards refuse both kinds
// for the same reason PutSecret's comment gives -- a predicate a reader
// cannot see is a predicate a review cannot check. The race this trades for
// is a caller registering the same name twice at once, which registration is
// not on a path where that is likely.
// rateLimit and ratePeriodSeconds must both be nil (unlimited) or both set
// (>= 1) -- see Queue's doc comment. CreateQueue rejects one-without-the-other
// itself rather than leaving it to the CHECK constraint
// (ck_queues_rate_limit_paired), the same reasoning queueCreate in cleatctl
// gives for refusing --concurrency < 1 before the store's own check would:
// a constraint violation does not tell an operator which of the two flags
// they forgot.
func (s *QueueStore) CreateQueue(ctx context.Context, tenantID, name string, concurrencyLimit int, rateLimit, ratePeriodSeconds *int) error {
	if s == nil || s.db == nil {
		return errors.New("queue store has no database handle")
	}
	if !validQueueName(name) {
		return fmt.Errorf("queue name %q must match [A-Za-z0-9_.-]{1,128}", name)
	}
	if concurrencyLimit < 1 {
		return fmt.Errorf("queue concurrency limit must be >= 1, got %d", concurrencyLimit)
	}
	if err := validateRateLimitPair(rateLimit, ratePeriodSeconds); err != nil {
		return err
	}
	ctx, err := withQueueTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.execTenantScoped(ctx, func(q querier) error {
		var exists int
		err := q.QueryRowContext(ctx, getQueueExistsStmt(s.dialect), tenantID, name).Scan(&exists)
		if err == nil {
			return ErrQueueAlreadyExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = q.ExecContext(ctx, createQueueStmt(s.dialect), tenantID, name, concurrencyLimit,
			nullableInt(rateLimit), nullableInt(ratePeriodSeconds))
		return err
	})
}

// validateRateLimitPair enforces Queue's "both nil or both set" contract at
// the Go layer, in front of ck_queues_rate_limit_paired.
func validateRateLimitPair(rateLimit, ratePeriodSeconds *int) error {
	if (rateLimit == nil) != (ratePeriodSeconds == nil) {
		return errors.New("queue rate limit and rate period must both be set, or neither")
	}
	if rateLimit != nil && *rateLimit < 1 {
		return fmt.Errorf("queue rate limit must be >= 1, got %d", *rateLimit)
	}
	if ratePeriodSeconds != nil && *ratePeriodSeconds < 1 {
		return fmt.Errorf("queue rate period must be >= 1 second, got %d", *ratePeriodSeconds)
	}
	return nil
}

// nullableInt turns a possibly-nil *int into a driver value every dialect
// here accepts for a NULL-able INTEGER column: a bare nil interface{} binds
// as NULL on all three drivers this package uses, and the dereferenced value
// otherwise.
func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// GetQueue returns one tenant's queue by name.
func (s *QueueStore) GetQueue(ctx context.Context, tenantID, name string) (Queue, error) {
	if s == nil || s.db == nil {
		return Queue{}, errors.New("queue store has no database handle")
	}
	var q Queue
	q.Name = name
	var disabledAt sql.NullTime
	var rateLimit, ratePeriodSeconds sql.NullInt64
	ctx, err := withQueueTenant(ctx, tenantID)
	if err != nil {
		return Queue{}, err
	}
	err = s.execTenantScoped(ctx, func(qr querier) error {
		return qr.QueryRowContext(ctx, getQueueStmt(s.dialect), tenantID, name).
			Scan(&q.ConcurrencyLimit, &rateLimit, &ratePeriodSeconds, &q.CreatedAt, &q.UpdatedAt, &disabledAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Queue{}, ErrQueueNotFound
	}
	if err != nil {
		return Queue{}, err
	}
	if disabledAt.Valid {
		q.DisabledAt = &disabledAt.Time
	}
	q.RateLimit, q.RatePeriodSeconds = nullInt64Pair(rateLimit, ratePeriodSeconds)
	return q, nil
}

// nullInt64Pair converts the two nullable columns a queue's rate limit reads
// as into the *int pair Queue carries. Both columns are NULL or both are set
// (ck_queues_rate_limit_paired), so this never has to represent "only one is
// present" -- if it ever saw that, it would be reporting a row the constraint
// should have refused, and returning nil/nil would silently hide exactly that.
func nullInt64Pair(a, b sql.NullInt64) (*int, *int) {
	if !a.Valid || !b.Valid {
		return nil, nil
	}
	av, bv := int(a.Int64), int(b.Int64)
	return &av, &bv
}

// ListQueues returns every queue a tenant has registered, including disabled
// ones -- callers that only want live queues filter on DisabledAt == nil.
func (s *QueueStore) ListQueues(ctx context.Context, tenantID string) ([]Queue, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("queue store has no database handle")
	}
	var out []Queue
	ctx, err := withQueueTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	err = s.execTenantScoped(ctx, func(qr querier) error {
		rows, err := qr.(rowQuerier).QueryContext(ctx, listQueuesStmt(s.dialect), tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var q Queue
			var disabledAt sql.NullTime
			var rateLimit, ratePeriodSeconds sql.NullInt64
			if err := rows.Scan(&q.Name, &q.ConcurrencyLimit, &rateLimit, &ratePeriodSeconds,
				&q.CreatedAt, &q.UpdatedAt, &disabledAt); err != nil {
				return err
			}
			if disabledAt.Valid {
				q.DisabledAt = &disabledAt.Time
			}
			q.RateLimit, q.RatePeriodSeconds = nullInt64Pair(rateLimit, ratePeriodSeconds)
			out = append(out, q)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DisableQueue retires a queue without deleting it, following the soft-flag
// pattern cleat#1702 standardised (workflow_schedules.enabled and
// workflow_defs.deprecated before it). Idempotent -- disabling an
// already-disabled queue succeeds and leaves its DisabledAt unchanged.
//
// # WHAT DISABLING DOES AT CLAIM TIME, WHICH IS NOT "REFUSE"
//
// Every claim statement joins `queues` with `AND q.disabled_at IS NULL`, so a
// disabled queue is indistinguishable from an unregistered name -- and an
// unregistered name takes the bare-key arm, which is the concurrency_keys
// mutex. So disabling DROPS ADMISSION TO ONE AT A TIME; it does not stop the
// work. Runs keep being claimed, serially, and nothing is cancelled.
//
// This comment claimed "new acquisition is refused, existing holders drain"
// until TestADisabledQueueFallsBackToTheBareKeyMutex was written, which is
// what a doc comment describing an intent rather than a caller is worth. The
// fallback is the better behaviour of the two and is kept deliberately:
// refusing instead would let one operator command silently wedge every run
// carrying that key, with no error anywhere to say why.
func (s *QueueStore) DisableQueue(ctx context.Context, tenantID, name string) error {
	if s == nil || s.db == nil {
		return errors.New("queue store has no database handle")
	}
	ctx, err := withQueueTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.execTenantScoped(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, disableQueueStmt(s.dialect), tenantID, name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			// Either the name does not exist, or it is already disabled (the
			// statement's WHERE excludes rows with disabled_at already set).
			// Distinguish them rather than reporting ErrQueueNotFound for an
			// already-disabled queue, which would be surprising -- disabling
			// twice must succeed, per this method's own doc comment.
			if _, getErr := s.GetQueue(ctx, tenantID, name); getErr != nil {
				return getErr
			}
		}
		return nil
	})
}

// EnableQueue puts a retired queue back into service, clearing disabled_at.
//
// The other half of DisableQueue, and it exists because the entity contract
// this table conforms to (docs/reference/entity-lifecycle.md) makes retirement
// REVERSIBLE by design -- "deletion is a separate, irreversible operation".
// Every sibling already honours that: suspend-tenant has resume-tenant,
// versions deprecate has versions restore. Shipping the disable half alone
// would have handed an operator a one-way door the contract says is not one.
//
// Idempotent in both directions, like DisableQueue: enabling a live queue
// succeeds and changes nothing. Unknown names are ErrQueueNotFound, which is
// the one case a caller does want to hear about.
func (s *QueueStore) EnableQueue(ctx context.Context, tenantID, name string) error {
	if s == nil || s.db == nil {
		return errors.New("queue store has no database handle")
	}
	ctx, err := withQueueTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.execTenantScoped(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, enableQueueStmt(s.dialect), tenantID, name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			// No row updated means either "no such queue" or "already live",
			// because the statement's WHERE excludes rows whose disabled_at is
			// already NULL. GetQueue separates them, exactly as DisableQueue
			// does for the mirror case.
			if _, getErr := s.GetQueue(ctx, tenantID, name); getErr != nil {
				return getErr
			}
		}
		return nil
	})
}

// SetQueueRateLimit sets or clears a registered queue's rate limit.
// rateLimit/ratePeriodSeconds both nil clears it (unlimited, matching a
// freshly created queue's default); both set applies it; one-without-the-
// other is refused by validateRateLimitPair before any statement runs.
//
// UNLIKE DisableQueue/EnableQueue, THIS TAKES EFFECT AT THE NEXT CLAIM
// IMMEDIATELY, not on a toggle with its own semantics to explain -- setting a
// rate limit is changing a number the claim reads, not a lifecycle
// transition. It shares their idempotent-on-no-such-queue error handling
// because it is the same UPDATE-then-distinguish shape, not because it is the
// same kind of operation.
func (s *QueueStore) SetQueueRateLimit(ctx context.Context, tenantID, name string, rateLimit, ratePeriodSeconds *int) error {
	if s == nil || s.db == nil {
		return errors.New("queue store has no database handle")
	}
	if err := validateRateLimitPair(rateLimit, ratePeriodSeconds); err != nil {
		return err
	}
	ctx, err := withQueueTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.execTenantScoped(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, setQueueRateLimitStmt(s.dialect),
			nullableInt(rateLimit), nullableInt(ratePeriodSeconds), tenantID, name)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			if _, getErr := s.GetQueue(ctx, tenantID, name); getErr != nil {
				return getErr
			}
		}
		return nil
	})
}

// withQueueTenant puts tenantID into ctx the way beginTenantTx requires --
// tenantctx.From(ctx), not a query parameter -- and returns it in a form the
// caller can pass straight through. Every exported QueueStore method takes
// this step first.
//
// UNLIKE SecretStore, WHICH RELIES ON ITS CALLER. PutSecret and GetSecret take
// tenantID as a parameter but never wrap ctx themselves; they work today only
// because their one real caller, ResolveSecretRefs, runs on a plugin-call ctx
// that plugin_call_context.go already wrapped via this same tenantctx.With.
// QueueStore's caller is cmd/cleatctl's `queue` subcommand, and it reaches
// these methods on a bare context.Background() -- which is exactly why the
// wrapping has to happen here. A test calling CreateQueue on a bare context is
// PR 1's own falsification, and it failed on SQL Server specifically:
// PostgreSQL's RLS exempts the superuser connection tests use, so the missing
// tenant context was invisible there; SQL Server's FILTER PREDICATE is not
// exempted the same way, so GetQueue read back "not found" after a successful
// CreateQueue. Wrapping here, rather than requiring every future caller to
// remember to, is what makes that failure mode structural rather than a
// caller's responsibility to get right every time.
func withQueueTenant(ctx context.Context, tenantID string) (context.Context, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return ctx, fmt.Errorf("queue store: tenant id %q is not a UUID: %w", tenantID, err)
	}
	return tenantctx.With(ctx, tid), nil
}

// execTenantScoped runs fn inside a transaction carrying this request's
// tenant, where the dialect needs one. Identical to SecretStore's own helper
// of the same name (engine/tenant_secrets.go) -- not shared between the two
// because they differ only in the store type threaded through, and a shared
// generic here would cost more to read than the four lines it would save.
func (s *QueueStore) execTenantScoped(ctx context.Context, fn func(querier) error) error {
	tx, err := beginTenantTx(ctx, s.db, plugin.Dialect(s.dialect), nil)
	if err != nil {
		return err
	}
	if tx == nil {
		return fn(s.db)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// rowQuerier adds QueryContext to querier, needed only by ListQueues. A
// second interface rather than widening querier itself: querier's whole point
// (stated on its own declaration in tenant_secrets.go) is being the subset
// both *sql.DB and *sql.Tx already satisfy, and both satisfy this too, so
// nothing loses the ability to be passed through execTenantScoped.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func validQueueName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

func getQueueExistsStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT 1 FROM queues WHERE tenant_id = ? AND name = ?`
	case "mssql":
		return `SELECT 1 FROM queues WHERE tenant_id = @p1 AND name = @p2`
	default:
		return `SELECT 1 FROM queues WHERE tenant_id = $1 AND name = $2`
	}
}

func createQueueStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `INSERT INTO queues (tenant_id, name, concurrency_limit, rate_limit, rate_period_seconds) VALUES (?, ?, ?, ?, ?)`
	case "mssql":
		return `INSERT INTO queues (tenant_id, name, concurrency_limit, rate_limit, rate_period_seconds) VALUES (@p1, @p2, @p3, @p4, @p5)`
	default:
		return `INSERT INTO queues (tenant_id, name, concurrency_limit, rate_limit, rate_period_seconds) VALUES ($1, $2, $3, $4, $5)`
	}
}

func getQueueStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT concurrency_limit, rate_limit, rate_period_seconds, created_at, updated_at, disabled_at FROM queues WHERE tenant_id = ? AND name = ?`
	case "mssql":
		return `SELECT concurrency_limit, rate_limit, rate_period_seconds, created_at, updated_at, disabled_at FROM queues WHERE tenant_id = @p1 AND name = @p2`
	default:
		return `SELECT concurrency_limit, rate_limit, rate_period_seconds, created_at, updated_at, disabled_at FROM queues WHERE tenant_id = $1 AND name = $2`
	}
}

func listQueuesStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT name, concurrency_limit, rate_limit, rate_period_seconds, created_at, updated_at, disabled_at FROM queues WHERE tenant_id = ? ORDER BY name`
	case "mssql":
		return `SELECT name, concurrency_limit, rate_limit, rate_period_seconds, created_at, updated_at, disabled_at FROM queues WHERE tenant_id = @p1 ORDER BY name`
	default:
		return `SELECT name, concurrency_limit, rate_limit, rate_period_seconds, created_at, updated_at, disabled_at FROM queues WHERE tenant_id = $1 ORDER BY name`
	}
}

// setQueueRateLimitStmt carries no disabled_at predicate, unlike
// enable/disableQueueStmt below: a rate limit is configuration, not a
// lifecycle transition, and applies (or is cleared) whether the queue is
// live or retired -- the same reason queueList prints a disabled queue's
// concurrency_limit rather than hiding it.
func setQueueRateLimitStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE queues SET rate_limit = ?, rate_period_seconds = ?, updated_at = NOW(6) WHERE tenant_id = ? AND name = ?`
	case "mssql":
		return `UPDATE queues SET rate_limit = @p1, rate_period_seconds = @p2, updated_at = SYSUTCDATETIME() WHERE tenant_id = @p3 AND name = @p4`
	default:
		return `UPDATE queues SET rate_limit = $1, rate_period_seconds = $2, updated_at = now() WHERE tenant_id = $3 AND name = $4`
	}
}

func enableQueueStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE queues SET disabled_at = NULL, updated_at = NOW(6) WHERE tenant_id = ? AND name = ? AND disabled_at IS NOT NULL`
	case "mssql":
		return `UPDATE queues SET disabled_at = NULL, updated_at = SYSUTCDATETIME() WHERE tenant_id = @p1 AND name = @p2 AND disabled_at IS NOT NULL`
	default:
		return `UPDATE queues SET disabled_at = NULL, updated_at = now() WHERE tenant_id = $1 AND name = $2 AND disabled_at IS NOT NULL`
	}
}

func disableQueueStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE queues SET disabled_at = NOW(6), updated_at = NOW(6) WHERE tenant_id = ? AND name = ? AND disabled_at IS NULL`
	case "mssql":
		return `UPDATE queues SET disabled_at = SYSUTCDATETIME(), updated_at = SYSUTCDATETIME() WHERE tenant_id = @p1 AND name = @p2 AND disabled_at IS NULL`
	default:
		return `UPDATE queues SET disabled_at = now(), updated_at = now() WHERE tenant_id = $1 AND name = $2 AND disabled_at IS NULL`
	}
}
