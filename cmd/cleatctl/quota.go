package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// defaultQuotaResource is the only resource plugins/tenantquota meters today
// (tenantquota.ResourceWorkflowStarts). Not imported from that package: it is
// an HTTP plugin, and pulling it in for one string constant would run its
// init() (plugin.Register) inside a DB CLI tool that never discovers plugins,
// for no benefit. If tenantquota grows a second resource, --resource is
// already how a caller names it.
const defaultQuotaResource = "workflow_starts"

// ErrQuotaConflict is returned when a tenant_quota row changed between the
// read and the write -- the same shape as engine.ErrTenantSettingsConflict,
// rebuilt here because that one is wired to engine.PostgresStore and quota
// must run on all three dialects (cleat#2046). See runSetQuota.
var ErrQuotaConflict = errors.New("quota changed since it was read")

// quotaRow mirrors one row of tenant_quota.
type quotaRow struct {
	limitCount    int64
	windowSeconds int
	enforce       bool
	updatedAt     time.Time
	existed       bool
}

// Written in PostgreSQL $N form and rewritten per dialect through d.rebind at
// every call site, the same convention as speaks_three_dialects_test.go's
// loadWorkflowInstanceSQL: named here rather than inlined so
// TestQuotaStatementsRebindPerDialect can assert the rewrite actually reaches
// all three placeholder forms, not just that it compiles.
const (
	quotaReadSQL = `
		SELECT limit_count, window_seconds, enforce, updated_at
		FROM tenant_quota
		WHERE tenant_id = $1 AND resource = $2
	`
	// created_at/updated_at are bound Go values ($6, $7 below), not SQL-side
	// now()/now(6)/SYSUTCDATETIME(). MySQL's bare now() truncates to WHOLE-
	// SECOND precision (NOW(6) is the microsecond form; plugin.Rebind's
	// now()->SYSUTCDATETIME() rewrite covers MSSQL but leaves MySQL's now()
	// untouched), while tenant_quota's column is TIMESTAMP(6). Measured
	// directly: two writes inside the same wall-clock second both truncated to
	// an IDENTICAL stored value, so a write using the first write's updated_at
	// as its stale-precondition matched the row's current (also-truncated,
	// coincidentally equal) value and was wrongly accepted --
	// TestQuotaCommandWorksOnEveryDialect/mysql caught it directly. Binding the
	// timestamp from Go sidesteps the SQL dialect's own now() precision
	// entirely, uniformly on all three.
	quotaInsertSQL = `
		INSERT INTO tenant_quota (tenant_id, resource, limit_count, window_seconds, enforce, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	// $N numbers appear in ascending textual order (SET before WHERE), not the
	// SQL's own natural grouping: MySQL's ? binds by TEXTUAL APPEARANCE, not by
	// number (CLAUDE.md's "MySQL binds `?` by APPEARANCE; $N and @pN bind by
	// NUMBER"). Postgres and MSSQL bind by number and would not have caught a
	// mismatch; MySQL did, immediately, with "Truncated incorrect DOUBLE
	// value: '<uuid>'" when the SET clause was written with $3.
	quotaUpdateSQL = `
		UPDATE tenant_quota
		SET limit_count = $1, window_seconds = $2, enforce = $3, updated_at = $4
		WHERE tenant_id = $5 AND resource = $6 AND updated_at = $7
	`
	quotaListTenantSQL = `
		SELECT tenant_id, resource, limit_count, window_seconds, enforce, updated_at
		FROM tenant_quota
		WHERE tenant_id = $1
		ORDER BY resource
	`
	quotaListAllSQL = `
		SELECT tenant_id, resource, limit_count, window_seconds, enforce, updated_at
		FROM tenant_quota
		ORDER BY tenant_id, resource
	`
)

// runQuota dispatches `cleatctl quota get|set|list`.
func runQuota(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) < 1 {
		printQuotaUsage()
		osExit(1)
		return
	}
	switch args[0] {
	case "get":
		runGetQuota(ctx, db, d, args[1:])
	case "set":
		runSetQuota(ctx, db, d, args[1:])
	case "list":
		runListQuota(ctx, db, d, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown quota subcommand: %s\n\n", args[0])
		printQuotaUsage()
		osExit(1)
	}
}

func printQuotaUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> quota <get|set|list> [flags]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

  quota get  --tenant <uuid> [--resource <name>]
  quota set  --tenant <uuid> [--resource <name>]
             [--limit-count N] [--window-seconds N] [--enforce=true|false]
  quota list [--tenant <uuid>]

--resource defaults to %q, the only resource cleat currently meters.

quota set is read-modify-write with an updated_at precondition, the same
shape as set-tenant-setting: if the row changed since this command read it,
the write is refused rather than applied.
`, defaultQuotaResource)
}

// parseQuotaFlags scans args for --tenant/--resource/--limit-count/
// --window-seconds/--enforce in any order, following set-tenant-setting's
// reasoning: flag.FlagSet stops parsing at the first non-flag argument, and
// every flag here is a --flag, so a hand-rolled scan avoids that trap outright
// rather than relying on a stable argument order.
func parseQuotaFlags(args []string) (tenantID, resource string, limitCount, windowSeconds int64, enforce string, show bool, err error) {
	limitCount, windowSeconds = -1, -1
	resource = defaultQuotaResource
	for i := 0; i < len(args); i++ {
		a := args[i]
		key, raw := a, ""
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			key, raw = a[:eq], a[eq+1:]
		}
		next := func() (string, error) {
			if raw != "" {
				return raw, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", key)
			}
			i++
			return args[i], nil
		}
		switch key {
		case "--tenant":
			if tenantID, err = next(); err != nil {
				return
			}
		case "--resource":
			if resource, err = next(); err != nil {
				return
			}
		case "--limit-count":
			var v string
			if v, err = next(); err != nil {
				return
			}
			if limitCount, err = strconv.ParseInt(v, 10, 64); err != nil {
				err = fmt.Errorf("--limit-count needs an integer, got %q", v)
				return
			}
		case "--window-seconds":
			var v string
			if v, err = next(); err != nil {
				return
			}
			if windowSeconds, err = strconv.ParseInt(v, 10, 64); err != nil {
				err = fmt.Errorf("--window-seconds needs an integer, got %q", v)
				return
			}
		case "--enforce":
			if enforce, err = next(); err != nil {
				return
			}
			if enforce != "true" && enforce != "false" {
				err = fmt.Errorf("--enforce needs true or false, got %q", enforce)
				return
			}
		case "--show", "-show":
			show = true
		default:
			err = fmt.Errorf("unknown flag: %s", a)
			return
		}
	}
	return
}

// quotaExecer is satisfied by both *sql.DB and *sql.Conn, so readQuota and
// writeQuota can run either against the pool directly or against one pinned
// connection -- see quotaConnFor for why MSSQL needs the latter.
type quotaExecer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// quotaConnFor returns the executer quota.go's read/write helpers should use
// for tenantID, and a closer to release it when done.
//
// On SQL Server, tenant_quota carries the same SECURITY POLICY /
// SESSION_CONTEXT('tenant_id') restriction droptenant_mssql.go documents for
// drop-tenant and setsecret.go documents for tenant_secrets: no role bypasses
// it (migrations/mssql/075 removed the disjunction that used to let one), so
// a read or write against the shared pool sees whatever tenant, if any, the
// connection handed out happened to carry last -- which for a fresh
// connection is none, and none means the filter/block predicate hides or
// refuses every row. This pins ONE *sql.Conn and sets the key on it before
// returning, mirroring setMSSQLTenantKey's use in droptenant_mssql.go: on the
// pool, but not on a fresh checkout, since sp_set_session_context is
// per-connection state that go-mssqldb clears on ResetSession.
//
// PostgreSQL and MySQL need none of this: cleatctl connects to PostgreSQL as
// an administrative role RLS does not apply to (rlsposture.go), and
// tenant_quota carries no equivalent restriction on MySQL -- it is a plain
// WHERE-scoped table there, like tenant_settings.
func quotaConnFor(ctx context.Context, db *sql.DB, d dialect, tenantID string) (quotaExecer, func(), error) {
	if d.name != "mssql" {
		return db, func() {}, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquiring a connection: %w", err)
	}
	if err := setMSSQLTenantKey(ctx, conn, tenantID); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, func() { conn.Close() }, nil
}

func readQuota(ctx context.Context, exec quotaExecer, d dialect, tenantID, resource string) (quotaRow, error) {
	row := exec.QueryRowContext(ctx, d.rebind(quotaReadSQL), tenantID, resource)
	var q quotaRow
	err := row.Scan(&q.limitCount, &q.windowSeconds, &q.enforce, &q.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return quotaRow{}, nil
	}
	if err != nil {
		return quotaRow{}, fmt.Errorf("reading quota for tenant %s resource %s: %w", tenantID, resource, err)
	}
	q.existed = true
	return q, nil
}

// writeQuota applies next over the row read as current, refusing with
// ErrQuotaConflict if the row moved since then.
//
// The UPDATE branch's WHERE ... AND updated_at = $N is dialect-portable
// as-is: a targeted UPDATE with a stale-precondition column needs nothing
// dialect-specific. The INSERT branch does need care -- rather than a
// three-way upsert (ON CONFLICT / ON DUPLICATE KEY / MERGE), it relies on the
// primary key violation a plain INSERT raises if a row appeared since the
// read, the same trade plugins/tenantquota/middleware.go makes for its
// counter-bucket INSERT: one statement, tolerant of exactly the error a
// collision produces, checked by substring across all three dialects rather
// than by a per-dialect switch (see isQuotaDuplicateKey).
//
// now is computed ONCE, here, and bound as a parameter -- see quotaInsertSQL's
// doc comment for why this does not delegate to SQL-side now().
func writeQuota(ctx context.Context, exec quotaExecer, d dialect, tenantID, resource string, current quotaRow, next quotaRow) error {
	now := time.Now().UTC()
	if !current.existed {
		_, err := exec.ExecContext(ctx, d.rebind(quotaInsertSQL), tenantID, resource, next.limitCount, next.windowSeconds, next.enforce, now, now)
		if err != nil {
			if isQuotaDuplicateKey(err) {
				return ErrQuotaConflict
			}
			return fmt.Errorf("writing quota: %w", err)
		}
		return nil
	}

	res, err := exec.ExecContext(ctx, d.rebind(quotaUpdateSQL), next.limitCount, next.windowSeconds, next.enforce, now, tenantID, resource, current.updatedAt)
	if err != nil {
		return fmt.Errorf("writing quota: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("writing quota: rows affected: %w", err)
	}
	if n == 0 {
		return ErrQuotaConflict
	}
	return nil
}

// isQuotaDuplicateKey reports whether err is a primary-key violation, in
// whichever of the three dialects' spellings. Deliberately dialect-blind for
// the same reason plugins/tenantquota/middleware.go's isDuplicateKey is: the
// only INSERT this guards is the one tenant_quota row this command itself is
// trying to create, so a false positive can only turn a real error on that
// one statement into ErrQuotaConflict -- a safe direction, since the operator
// is told to re-read rather than told the write silently succeeded.
func isQuotaDuplicateKey(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"duplicate key", "23505", "duplicate entry", "1062", "primary key", "2627"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func printQuotaRow(label, tenantID, resource string, q quotaRow) {
	fmt.Printf("%s\n", label)
	if !q.existed {
		fmt.Printf("  (no row: %s is unmetered for resource %s)\n", tenantID, resource)
		return
	}
	fmt.Printf("  resource        %s\n", resource)
	fmt.Printf("  limit_count     %d\n", q.limitCount)
	fmt.Printf("  window_seconds  %d\n", q.windowSeconds)
	fmt.Printf("  enforce         %t\n", q.enforce)
	fmt.Printf("  updated_at      %s\n", q.updatedAt.Format(time.RFC3339))
}

// runGetQuota prints one tenant's quota for one resource.
func runGetQuota(ctx context.Context, db *sql.DB, d dialect, args []string) {
	tenantID, resource, _, _, _, _, err := parseQuotaFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printQuotaUsage()
		osExit(1)
		return
	}
	if tenantID == "" {
		fmt.Fprintf(os.Stderr, "quota get requires --tenant\n\n")
		printQuotaUsage()
		osExit(1)
		return
	}
	if _, err := uuid.Parse(tenantID); err != nil {
		fmt.Fprintf(os.Stderr, "not a tenant UUID: %q: %v\n", tenantID, err)
		osExit(1)
		return
	}
	exec, closeExec, err := quotaConnFor(ctx, db, d, tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}
	defer closeExec()
	q, err := readQuota(ctx, exec, d, tenantID, resource)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}
	printQuotaRow(fmt.Sprintf("tenant %s:", tenantID), tenantID, resource, q)
}

// runSetQuota creates or updates one tenant's quota for one resource.
//
// Read-modify-write with an updated_at precondition, mirroring
// set-tenant-setting exactly (cleat#2046, owner decision 3B): a bare UPDATE
// loses a concurrent operator's change in silence, and the fix here is the
// same one -- read, compare on write, refuse rather than overwrite.
//
// Unlike tenant_settings' millisecond fields, limit_count and window_seconds
// are NOT NULL with a CHECK > 0 (see plugins/tenantquota/migrations.go): a
// quota row either exists with real values or does not exist at all, so
// there is no "0 means cleared" sentinel to preserve here. A field omitted
// on an UPDATE keeps its current value; on a fresh INSERT, --limit-count and
// --window-seconds are both required, since there is no current value to
// fall back to.
func runSetQuota(ctx context.Context, db *sql.DB, d dialect, args []string) {
	tenantID, resource, limitCount, windowSeconds, enforceFlag, show, err := parseQuotaFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printQuotaUsage()
		osExit(1)
		return
	}
	if tenantID == "" {
		fmt.Fprintf(os.Stderr, "quota set requires --tenant\n\n")
		printQuotaUsage()
		osExit(1)
		return
	}
	if _, err := uuid.Parse(tenantID); err != nil {
		fmt.Fprintf(os.Stderr, "not a tenant UUID: %q: %v\n", tenantID, err)
		osExit(1)
		return
	}

	exec, closeExec, err := quotaConnFor(ctx, db, d, tenantID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}
	defer closeExec()

	current, err := readQuota(ctx, exec, d, tenantID, resource)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}

	if show {
		printQuotaRow(fmt.Sprintf("tenant %s:", tenantID), tenantID, resource, current)
		return
	}

	if limitCount < 0 && windowSeconds < 0 && enforceFlag == "" {
		fmt.Fprintf(os.Stderr, "nothing to change: pass --limit-count, --window-seconds and/or --enforce, or --show to read them\n\n")
		printQuotaUsage()
		osExit(1)
		return
	}
	if !current.existed && (limitCount < 0 || windowSeconds < 0) {
		fmt.Fprintf(os.Stderr, "tenant %s has no quota row for resource %s yet: --limit-count and --window-seconds are both required to create one\n", tenantID, resource)
		osExit(1)
		return
	}

	next := current
	if limitCount >= 0 {
		if limitCount == 0 {
			fmt.Fprintf(os.Stderr, "--limit-count must be positive, got 0\n")
			osExit(1)
			return
		}
		next.limitCount = limitCount
	}
	if windowSeconds >= 0 {
		if windowSeconds == 0 {
			fmt.Fprintf(os.Stderr, "--window-seconds must be positive, got 0\n")
			osExit(1)
			return
		}
		next.windowSeconds = int(windowSeconds)
	}
	if enforceFlag != "" {
		next.enforce = enforceFlag == "true"
	}

	if err := writeQuota(ctx, exec, d, tenantID, resource, current, next); err != nil {
		if errors.Is(err, ErrQuotaConflict) {
			fmt.Fprintf(os.Stderr,
				"refused: tenant %s's quota for resource %s changed since this command read it.\n\n"+
					"Nothing was written. Someone else changed the row -- re-read it with\n"+
					"  cleatctl quota get --tenant %s --resource %s --db \"$DSN\"\n"+
					"and decide, rather than re-running this and overwriting their change.\n",
				tenantID, resource, tenantID, resource)
			osExit(1)
			return
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}

	next.existed = true
	printQuotaRow(fmt.Sprintf("tenant %s updated:", tenantID), tenantID, resource, next)
}

// runListQuota prints every quota row, or one tenant's rows if --tenant is
// given.
//
// Listing EVERY tenant is refused on SQL Server rather than silently
// returning an empty table. tenant_quota's SECURITY POLICY has no
// cross-tenant bypass on this dialect (see quotaConnFor's doc comment) -- a
// filter predicate applies to sysadmin and dbo alike, so a connection with no
// SESSION_CONTEXT('tenant_id') set does not see a partial or wrong-looking
// result, it sees zero rows for every tenant, which reads exactly like an
// empty database. That silent-empty shape is the one thing this whole file
// exists to refuse (see runSetQuota's ErrQuotaConflict): an operator who ran
// `quota list` expecting an inventory and got "(no quota rows)" would have no
// way to tell a genuinely empty deployment from this restriction. --tenant is
// required on this dialect; get and set already require it unconditionally.
func runListQuota(ctx context.Context, db *sql.DB, d dialect, args []string) {
	tenantID, _, _, _, _, _, err := parseQuotaFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printQuotaUsage()
		osExit(1)
		return
	}
	if tenantID != "" {
		if _, err := uuid.Parse(tenantID); err != nil {
			fmt.Fprintf(os.Stderr, "not a tenant UUID: %q: %v\n", tenantID, err)
			osExit(1)
			return
		}
	}
	if tenantID == "" && d.name == "mssql" {
		fmt.Fprintf(os.Stderr, "quota list requires --tenant on SQL Server: tenant_quota's "+
			"security policy has no cross-tenant bypass on this dialect, so a query with no "+
			"tenant selected would read as an empty table rather than a refusal\n")
		osExit(1)
		return
	}

	var rows *sql.Rows
	if tenantID != "" {
		var exec quotaExecer
		var closeExec func()
		exec, closeExec, err = quotaConnFor(ctx, db, d, tenantID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			osExit(1)
			return
		}
		defer closeExec()
		rows, err = exec.QueryContext(ctx, d.rebind(quotaListTenantSQL), tenantID)
	} else {
		rows, err = db.QueryContext(ctx, d.rebind(quotaListAllSQL))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "listing quotas: %v\n", err)
		osExit(1)
		return
	}
	defer rows.Close()

	fmt.Printf("%-36s  %-16s  %12s  %14s  %-7s  %s\n", "TENANT", "RESOURCE", "LIMIT_COUNT", "WINDOW_SECONDS", "ENFORCE", "UPDATED_AT")
	n := 0
	for rows.Next() {
		var tid, resource string
		var q quotaRow
		if err := rows.Scan(&tid, &resource, &q.limitCount, &q.windowSeconds, &q.enforce, &q.updatedAt); err != nil {
			fmt.Fprintf(os.Stderr, "listing quotas: scanning row: %v\n", err)
			osExit(1)
			return
		}
		fmt.Printf("%-36s  %-16s  %12d  %14d  %-7t  %s\n", tid, resource, q.limitCount, q.windowSeconds, q.enforce, q.updatedAt.Format(time.RFC3339))
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "listing quotas: %v\n", err)
		osExit(1)
		return
	}
	if n == 0 {
		fmt.Printf("(no quota rows)\n")
	}
}
