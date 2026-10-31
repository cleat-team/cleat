package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantdrop"
)

// Dropping a tenant, the store half. cleat#3552, B3b-2. The route that calls it is in cmd/cleat-worker.
//
// A drop is ONE TRANSACTION that does, in this order:
//
//  1. lock the tenant's admin.tenants row, so that a resume, a second drop or a suspend cannot interleave;
//  2. check the preconditions the lock makes stable: the tenant is suspended, and it has no unsettled run;
//  3. read where the tenant's own audit chain ends, because the drop is about to delete it;
//  4. write the tombstone;
//  5. call admin.drop_tenant, which deletes the tenant.
//
// Everything is atomic, so a drop that fails anywhere leaves the tenant exactly as it was, with no
// tombstone, and a drop that succeeds leaves the tombstone and no tenant. The tombstone is the one record of
// the drop that is atomic with it. The audit log's row for the request is enqueued after the handler
// returns (plugins/auditlog/middleware.go), so a crash in between loses it; the tombstone holds the
// principal, the time and the end of the dropped chain without it.
//
// WHAT THIS DOES NOT DO, so a reader does not credit it with more. It does not stop a run that is already
// executing: it REFUSES while any run is unsettled, and the operator settles or cancels them first. It does
// not close the window between a start that read the suspended flag before the suspend and the insert of its
// run: a run inserted in that window after the count is deleted by the drop, as every row of the tenant is.
// And it does not clear what the worker holds in memory for the tenant; the egress allowlist cache lives out
// its TTL.

// The two reasons a drop is refused for a tenant the caller may act on. Both are carried by
// DropRefusedError, which an API turns into a 409.
const (
	DropRefusedNotSuspended  = "not_suspended"
	DropRefusedUnsettledRuns = "unsettled_runs"
)

// DropRefusedError is a drop that cannot proceed yet and may succeed after the operator acts.
type DropRefusedError struct {
	Reason string
	// Unsettled is the number of runs that have not reached a terminal status. Set for
	// DropRefusedUnsettledRuns.
	Unsettled int64
}

func (e *DropRefusedError) Error() string {
	switch e.Reason {
	case DropRefusedNotSuspended:
		return "auth: the tenant must be suspended before it is dropped"
	default:
		return fmt.Sprintf("auth: the tenant has %d unsettled run(s); settle or cancel them before dropping it", e.Unsettled)
	}
}

// DropRequest is what a caller asks for.
type DropRequest struct {
	TenantID uuid.UUID

	// DroppedBy is the principal making the request: the same string the tenant's created_by uses, so
	// "clew-service" for a provision key and "operator:<KeyID>" for an admin key. Required.
	DroppedBy string

	// OnlyCreator, when set, limits the drop to a tenant that principal created. A tenant it did not
	// create, one made offline and one that does not exist all answer ErrTenantNotFound, so the answer
	// confirms nothing about a tenant the caller has no claim to. Empty means any tenant, and is for the
	// caller that is entitled to say so; it is a separate field rather than a flag so that an empty
	// variable cannot widen access by accident -- it can only fail to narrow it, and the handler that sets
	// it is the one deciding by scope.
	OnlyCreator string

	// Schema is the schema holding this deployment's cleat tables, the worker's --schema. PostgreSQL only,
	// and required there: admin.drop_tenant refuses an empty one rather than resolve through search_path.
	Schema string
}

// Tombstone is the record a drop leaves.
type Tombstone struct {
	TenantID  uuid.UUID
	Name      string
	OrgID     uuid.UUID
	CreatedBy string
	DroppedBy string
	DroppedAt time.Time

	// AuditChainSeq and AuditChainHash are where the dropped tenant's own audit chain ended. Nil when the
	// tenant had no chain: the audit plugin was not installed, or never recorded a row for it.
	AuditChainSeq  *int64
	AuditChainHash *string
}

// DropResult is the outcome of DropTenant.
type DropResult struct {
	Tombstone Tombstone

	// AlreadyDropped is true when the tenant was gone and its tombstone was found: a repeat of a drop that
	// succeeded. Nothing was changed by this call.
	AlreadyDropped bool
}

// dropStmts are one dialect's statements.
type dropStmts struct {
	lockTenant      string // name, org_id, created_by, suspended
	readTombstone   string
	insertTombstone string
	unsettled       string
}

func dropStatements(dialect string) (dropStmts, error) {
	switch dialect {
	case DialectPostgres:
		return dropStmts{
			lockTenant:    `SELECT name, org_id::text, COALESCE(created_by, ''), suspended FROM admin.tenants WHERE tenant_id = $1 FOR UPDATE`,
			readTombstone: `SELECT name, org_id::text, COALESCE(created_by, ''), dropped_by, dropped_at, audit_chain_seq, audit_chain_hash FROM admin.tenant_tombstones WHERE dropped_tenant_id = $1`,
			insertTombstone: `INSERT INTO admin.tenant_tombstones (dropped_tenant_id, name, org_id, created_by, dropped_by, audit_chain_seq, audit_chain_hash)
				VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7) RETURNING dropped_at`,
			unsettled: tenantdrop.UnsettledPostgres,
		}, nil
	case DialectMSSQL:
		return dropStmts{
			lockTenant:    `SELECT name, LOWER(CONVERT(NVARCHAR(36), org_id)), ISNULL(created_by, N''), suspended FROM admin.tenants WITH (UPDLOCK, ROWLOCK) WHERE tenant_id = @p1`,
			readTombstone: `SELECT name, LOWER(CONVERT(NVARCHAR(36), org_id)), ISNULL(created_by, N''), dropped_by, dropped_at, audit_chain_seq, audit_chain_hash FROM admin.tenant_tombstones WHERE dropped_tenant_id = @p1`,
			insertTombstone: `INSERT INTO admin.tenant_tombstones (dropped_tenant_id, name, org_id, created_by, dropped_by, audit_chain_seq, audit_chain_hash)
				OUTPUT INSERTED.dropped_at
				VALUES (@p1, @p2, @p3, NULLIF(@p4, N''), @p5, @p6, @p7)`,
			unsettled: tenantdrop.UnsettledMSSQL,
		}, nil
	default:
		return dropStmts{}, fmt.Errorf("auth: dropping a tenant is not supported for %s: multi-tenancy is PostgreSQL and SQL Server only", dialect)
	}
}

// DropTenant drops a tenant and leaves its tombstone. See the file comment for the order and what is
// atomic.
//
// Repeating a drop that succeeded answers AlreadyDropped with the tombstone and changes nothing, so a
// caller whose response was lost can repeat the request. Who may see that is decided as for the tenant
// itself: with OnlyCreator set, the tombstone's recorded creator must match, or it is ErrTenantNotFound.
func (s *TenantStore) DropTenant(ctx context.Context, req DropRequest) (DropResult, error) {
	st, err := dropStatements(s.dialect)
	if err != nil {
		return DropResult{}, err
	}
	if strings.TrimSpace(req.DroppedBy) == "" {
		return DropResult{}, errors.New("auth: a drop needs the principal making it")
	}
	if req.TenantID == uuid.Nil {
		return DropResult{}, errors.New("auth: the default tenant cannot be dropped")
	}
	if s.dialect == DialectPostgres && req.Schema == "" {
		return DropResult{}, errors.New("auth: a PostgreSQL drop needs the schema the cleat tables live in (the worker's --schema)")
	}

	tx, done, err := s.beginDropTx(ctx, req.TenantID)
	if err != nil {
		return DropResult{}, err
	}
	defer done()
	defer func() { _ = tx.Rollback() }()

	// 1. Lock the tenant.
	var name, orgID, createdBy string
	var suspended bool
	switch err := tx.QueryRowContext(ctx, st.lockTenant, req.TenantID.String()).Scan(&name, &orgID, &createdBy, &suspended); {
	case errors.Is(err, sql.ErrNoRows):
		// No tenant. If a tombstone says it was dropped, this is a repeat.
		tomb, found, terr := readTombstone(ctx, tx, st, req.TenantID)
		if terr != nil {
			return DropResult{}, terr
		}
		if !found || !mayActOn(req.OnlyCreator, tomb.CreatedBy) {
			return DropResult{}, ErrTenantNotFound
		}
		return DropResult{Tombstone: tomb, AlreadyDropped: true}, nil
	case err != nil:
		return DropResult{}, err
	}
	if !mayActOn(req.OnlyCreator, createdBy) {
		return DropResult{}, ErrTenantNotFound
	}

	// 2. Preconditions.
	if !suspended {
		return DropResult{}, &DropRefusedError{Reason: DropRefusedNotSuspended}
	}
	var unsettled int64
	if err := tx.QueryRowContext(ctx, st.unsettled, req.TenantID.String()).Scan(&unsettled); err != nil {
		return DropResult{}, fmt.Errorf("auth: count the tenant's unsettled runs: %w", err)
	}
	if unsettled > 0 {
		return DropResult{}, &DropRefusedError{Reason: DropRefusedUnsettledRuns, Unsettled: unsettled}
	}

	// 3. Where the tenant's own audit chain ends.
	seq, hash, err := s.readChainHead(ctx, tx, req.TenantID)
	if err != nil {
		return DropResult{}, err
	}

	// 4. The tombstone.
	oid, err := uuid.Parse(orgID)
	if err != nil {
		return DropResult{}, fmt.Errorf("auth: the tenant's org id %q is not a UUID: %w", orgID, err)
	}
	var droppedAt time.Time
	if err := tx.QueryRowContext(ctx, st.insertTombstone,
		req.TenantID.String(), name, oid.String(), createdBy, req.DroppedBy, seq, hash).Scan(&droppedAt); err != nil {
		return DropResult{}, fmt.Errorf("auth: write the tombstone: %w", err)
	}

	// 5. The drop.
	switch s.dialect {
	case DialectPostgres:
		_, err = tx.ExecContext(ctx, `SELECT admin.drop_tenant($1::uuid, $2)`, req.TenantID.String(), req.Schema)
	default:
		// This procedure sets SESSION_CONTEXT tenant_id to the tenant being dropped and restores the caller's
		// value only on the normal path and in CATCH. A request that is cancelled inside it (the worker passes
		// r.Context()) is a TDS attention, CATCH does not run, the transaction rolls back and the dropped
		// tenant stays in the SESSION (measured, cleat#3647). It runs as the application login, the one the
		// tenant-scoped stores use. What keeps the stale value from the next statement is go-mssqldb v1.11.2
		// resetting the session when database/sql reuses the connection, and
		// engine.TestACancelledCallThatSetsTheSessionContextIsNotSeenByTheNextPoolUse is what notices if a
		// driver bump loses it. Not covered, cleat#3647 stays open for each: the worker's HTTP handler end to
		// end, how the tenant-scoped stores set their own context on a pooled connection, any other driver
		// or pooler, and READ_COMMITTED_SNAPSHOT on.
		_, err = tx.ExecContext(ctx, `EXEC admin.drop_tenant @tenant_id = @p1`, req.TenantID.String())
	}
	if err != nil {
		return DropResult{}, fmt.Errorf("auth: admin.drop_tenant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DropResult{}, err
	}
	return DropResult{Tombstone: Tombstone{
		TenantID: req.TenantID, Name: name, OrgID: oid, CreatedBy: createdBy,
		DroppedBy: req.DroppedBy, DroppedAt: droppedAt, AuditChainSeq: seq, AuditChainHash: hash,
	}}, nil
}

// mayActOn reports whether a caller limited to onlyCreator may act on a tenant created by createdBy.
// Empty onlyCreator is no limit; otherwise the creator must be recorded and equal, compared here in Go
// exactly as ConfirmProvisionedTenant and SuspendTenantByCreator compare it, because SQL Server's
// default collation is case-insensitive and PostgreSQL's is not.
func mayActOn(onlyCreator, createdBy string) bool {
	return onlyCreator == "" || (createdBy != "" && createdBy == onlyCreator)
}

// beginDropTx opens the transaction and gives it the tenant's scope. The returned func must be called
// when the transaction is over, and puts the connection back as it found it.
//
// PostgreSQL scopes with set_config(..., true), which is transaction-local and cannot outlive it.
// SQL Server's session context belongs to the CONNECTION, so the transaction is pinned to one and the
// context is cleared before the connection returns to the pool; a pooled connection that kept it would
// serve the next caller as this tenant.
func (s *TenantStore) beginDropTx(ctx context.Context, tenantID uuid.UUID) (*sql.Tx, func(), error) {
	switch s.dialect {
	case DialectPostgres:
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('cleat.tenant_id', $1, true)`, tenantID.String()); err != nil {
			_ = tx.Rollback()
			return nil, nil, fmt.Errorf("auth: scope the drop to the tenant: %w", err)
		}
		return tx, func() {}, nil
	default:
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return nil, nil, err
		}
		release := func() {
			// Not ctx: this must run even when the request was cancelled.
			_, _ = conn.ExecContext(context.Background(), `EXEC sp_set_session_context @key = N'tenant_id', @value = NULL`)
			_ = conn.Close()
		}
		if _, err := conn.ExecContext(ctx, `EXEC sp_set_session_context @key = N'tenant_id', @value = @p1`, tenantID.String()); err != nil {
			release()
			return nil, nil, fmt.Errorf("auth: scope the drop to the tenant: %w", err)
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			release()
			return nil, nil, err
		}
		return tx, release, nil
	}
}

// readChainHead reads where the tenant's audit chain ends. Both returns are nil when it has none.
//
// The audit log is a plugin, so its tables exist only where the plugin ran its migrations. On PostgreSQL
// the schema is read from admin.plugin_tables, the same registry admin.drop_tenant deletes by, so this
// reads exactly what the drop is about to remove; on SQL Server the plugin's tables are unqualified and
// OBJECT_ID resolves them the way the plugin does.
func (s *TenantStore) readChainHead(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID) (*int64, *string, error) {
	var seq sql.NullInt64
	var hash sql.NullString
	switch s.dialect {
	case DialectPostgres:
		var schema sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT schema_name FROM admin.plugin_tables
			 WHERE table_name = 'audit_chain_heads' AND tenant_scoped
			   AND to_regclass(format('%I.%I', schema_name, table_name)) IS NOT NULL
			 ORDER BY schema_name LIMIT 1`).Scan(&schema)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("auth: find the audit chain table: %w", err)
		}
		err = tx.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT seq, hash FROM %s.audit_chain_heads WHERE tenant_id = $1`, quoteIdent(schema.String)),
			tenantID.String()).Scan(&seq, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("auth: read the tenant's audit chain head: %w", err)
		}
	default:
		err := tx.QueryRowContext(ctx,
			`IF OBJECT_ID(N'audit_chain_heads', N'U') IS NULL
				SELECT CAST(NULL AS bigint), CAST(NULL AS char(64))
			ELSE
				EXEC sp_executesql N'SELECT seq, hash FROM audit_chain_heads WHERE tenant_id = @t', N'@t uniqueidentifier', @t = @p1`,
			tenantID.String()).Scan(&seq, &hash)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("auth: read the tenant's audit chain head: %w", err)
		}
	}
	var s2 *int64
	var h2 *string
	if seq.Valid {
		s2 = &seq.Int64
	}
	if hash.Valid {
		h := strings.TrimSpace(hash.String)
		h2 = &h
	}
	return s2, h2, nil
}

// readTombstone reads a tenant's tombstone inside the drop's transaction.
func readTombstone(ctx context.Context, tx *sql.Tx, st dropStmts, tenantID uuid.UUID) (Tombstone, bool, error) {
	t, found, err := scanTombstone(tx.QueryRowContext(ctx, st.readTombstone, tenantID.String()).Scan, tenantID)
	if err != nil {
		return Tombstone{}, false, fmt.Errorf("auth: read the tombstone: %w", err)
	}
	return t, found, nil
}

// scanTombstone reads one tombstone row from a statement that selects, in this order, name, org_id, created_by,
// dropped_by, dropped_at, audit_chain_seq and audit_chain_hash. The read and the delete share it so that the row
// a caller was shown before a delete and the row the delete returns are scanned the same way. It returns the
// driver's error as it is: each caller says which statement failed, because "read the tombstone" on a failed
// DELETE sends whoever is debugging it to the wrong statement.
func scanTombstone(scan func(dest ...any) error, tenantID uuid.UUID) (Tombstone, bool, error) {
	t := Tombstone{TenantID: tenantID}
	var orgID string
	var seq sql.NullInt64
	var hash sql.NullString
	err := scan(&t.Name, &orgID, &t.CreatedBy, &t.DroppedBy, &t.DroppedAt, &seq, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Tombstone{}, false, nil
	}
	if err != nil {
		return Tombstone{}, false, err
	}
	if oid, perr := uuid.Parse(orgID); perr == nil {
		t.OrgID = oid
	}
	if seq.Valid {
		t.AuditChainSeq = &seq.Int64
	}
	if hash.Valid {
		h := strings.TrimSpace(hash.String)
		t.AuditChainHash = &h
	}
	return t, true, nil
}

// quoteIdent renders a PostgreSQL identifier the way quote_ident does.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
