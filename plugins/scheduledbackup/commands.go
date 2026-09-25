package scheduledbackup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterCommands returns CLI subcommands for the scheduled-backup plugin.
func (p *Plugin) RegisterCommands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "backup-run",
			Description: "Run a manual backup: --dsn=<url> --tenant=<uuid> --config=<uuid>",
			Run:         p.cliBackupRun,
		},
		{
			Name:        "backup-list",
			Description: "List backups for a tenant: --dsn=<url> --tenant=<uuid> [--config=<uuid>]",
			Run:         p.cliBackupList,
		},
	}
}

func (p *Plugin) cliBackupRun(cmds []string) error {
	fs := flag.NewFlagSet("backup-run", flag.ExitOnError)
	dsn := fs.String("dsn", "", "Database URL")
	tenantStr := fs.String("tenant", "", "Tenant UUID")
	configStr := fs.String("config", "", "Backup config UUID")
	dumpDir := fs.String("dump-dir", "/tmp/cleat-backups", "Directory for dump output")
	if err := fs.Parse(cmds); err != nil {
		return err
	}

	if *dsn == "" {
		return fmt.Errorf("--dsn is required")
	}
	if *tenantStr == "" {
		return fmt.Errorf("--tenant is required")
	}
	if *configStr == "" {
		return fmt.Errorf("--config is required")
	}

	tenantID, err := uuid.Parse(*tenantStr)
	if err != nil {
		return fmt.Errorf("invalid tenant UUID: %w", err)
	}
	configID, err := uuid.Parse(*configStr)
	if err != nil {
		return fmt.Errorf("invalid config UUID: %w", err)
	}

	driver := os.Getenv("CLEAT_DB_DRIVER")
	if driver == "" {
		driver = "postgres"
	}
	db, err := sql.Open(driver, *dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	conn, release, err := connScopedToTenant(context.Background(), db, tenantID)
	if err != nil {
		return err
	}
	defer release()

	// Fetch the backup config.
	var name, cronExpr, s3Bucket, s3Prefix string
	var retentionDays int
	err = conn.QueryRowContext(context.Background(), `
		SELECT name, cron, s3_bucket, s3_prefix, retention_days
		FROM backup_config WHERE id = $1 AND tenant_id = $2
	`, configID, tenantID).Scan(&name, &cronExpr, &s3Bucket, &s3Prefix, &retentionDays)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("backup config not found")
	}
	if err != nil {
		return fmt.Errorf("fetch config: %w", err)
	}

	// Create history entry.
	historyID := uuid.New()
	now := time.Now()
	filename := fmt.Sprintf("manual_%s_%s.dump", name, now.Format("20060102150405"))

	_, err = conn.ExecContext(context.Background(), `
		INSERT INTO backup_history (id, config_id, tenant_id, filename, status, started_at, created_at)
		VALUES ($1, $2, $3, $4, 'running', $5, $5)
	`, historyID, configID, tenantID, filename, now)
	if err != nil {
		return fmt.Errorf("create history entry: %w", err)
	}

	fmt.Printf("Starting backup %s for config %q...\n", historyID, name)

	// Execute pg_dump.
	//
	// SafeDumpPath rather than a bare Join (cleat#1305). Like the cron sweep,
	// this reads the name back out of backup_config and never passes through an
	// HTTP handler, so route-level validation does not reach it.
	dumpPath, err := SafeDumpPath(*dumpDir, filename)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dumpDir, 0755); err != nil {
		return fmt.Errorf("create dump dir: %w", err)
	}

	var stderr bytes.Buffer
	if err := runPgDump(context.Background(), *dsn, dumpPath, &stderr); err != nil {
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}

		conn.ExecContext(context.Background(), `
			UPDATE backup_history SET status = 'failed', error_message = $1, completed_at = now()
			WHERE id = $2
		`, errMsg, historyID)
		return fmt.Errorf("pg_dump failed: %s", errMsg)
	}

	// Read file size.
	var sizeBytes int64
	if fi, fiErr := os.Stat(dumpPath); fiErr == nil {
		sizeBytes = fi.Size()
	}

	// Update history with completed status.
	_, err = conn.ExecContext(context.Background(), `
		UPDATE backup_history SET status = 'completed', size_bytes = $1, completed_at = now()
		WHERE id = $2
	`, sizeBytes, historyID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update history: %v\n", err)
	}

	// Update config last_run_at and next_run_at.
	if nxt := nextRun(cronExpr, time.Now()); !nxt.IsZero() {
		conn.ExecContext(context.Background(), `
			UPDATE backup_config SET last_run_at = $1, next_run_at = $2, updated_at = now()
			WHERE id = $3
		`, time.Now(), nxt, configID)
	} else {
		conn.ExecContext(context.Background(), `
			UPDATE backup_config SET last_run_at = $1, next_run_at = NULL, updated_at = now()
			WHERE id = $2
		`, time.Now(), configID)
	}

	fmt.Printf("Backup completed: %s (%d bytes)\n", filename, sizeBytes)
	return nil
}

func (p *Plugin) cliBackupList(cmds []string) error {
	fs := flag.NewFlagSet("backup-list", flag.ExitOnError)
	dsn := fs.String("dsn", "", "Database URL")
	tenantStr := fs.String("tenant", "", "Tenant UUID")
	configStr := fs.String("config", "", "Optional config UUID filter")
	if err := fs.Parse(cmds); err != nil {
		return err
	}

	if *dsn == "" {
		return fmt.Errorf("--dsn is required")
	}
	if *tenantStr == "" {
		return fmt.Errorf("--tenant is required")
	}

	tenantID, err := uuid.Parse(*tenantStr)
	if err != nil {
		return fmt.Errorf("invalid tenant UUID: %w", err)
	}

	driver := os.Getenv("CLEAT_DB_DRIVER")
	if driver == "" {
		driver = "postgres"
	}
	db, err := sql.Open(driver, *dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	query := `
		SELECT h.id, h.config_id, h.filename, h.size_bytes, h.status, h.started_at, h.completed_at, h.error_message
		FROM backup_history h
		WHERE h.tenant_id = $1
	`
	qargs := []any{tenantID}
	argIdx := 2

	if *configStr != "" {
		cfgID, cfgErr := uuid.Parse(*configStr)
		if cfgErr != nil {
			return fmt.Errorf("invalid config UUID: %w", cfgErr)
		}
		query += fmt.Sprintf(" AND h.config_id = $%d", argIdx)
		qargs = append(qargs, cfgID)
		argIdx++ //nolint:ineffassign,staticcheck // Deliberate: keeps the placeholder counter correct so the next clause added below cannot silently reuse this one's $N. Deleting it is a latent SQL bug, not a cleanup.
	}

	query += " ORDER BY h.started_at DESC"

	conn, release, err := connScopedToTenant(context.Background(), db, tenantID)
	if err != nil {
		return err
	}
	defer release()

	rows, err := conn.QueryContext(context.Background(), query, qargs...)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	fmt.Printf("%-36s  %-36s  %-20s  %-12s  %-10s  %s\n", "ID", "CONFIG ID", "FILENAME", "SIZE", "STATUS", "STARTED AT")
	fmt.Println("----  ---------  --------  ----  ------  ----------")

	for rows.Next() {
		var (
			hid          uuid.UUID
			cid          uuid.UUID
			fn           string
			sizeBytes    sql.NullInt64
			status       string
			startedAt    time.Time
			completedAt  sql.NullTime
			errorMessage sql.NullString
		)
		if err := plugin.ScanRow(rows, &hid, &cid, &fn, &sizeBytes, &status, &startedAt, &completedAt, &errorMessage); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: scan row: %v\n", err)
			continue
		}

		sizeStr := "-"
		if sizeBytes.Valid {
			sizeStr = fmt.Sprintf("%d", sizeBytes.Int64)
		}

		fmt.Printf("%-36s  %-36s  %-20s  %-12s  %-10s  %s\n",
			hid, cid, fn, sizeStr, status, startedAt.Format(time.RFC3339))
	}

	return nil
}

// connScopedToTenant pins ONE connection and scopes it to tenantID for the
// life of the command. cleat#1512.
//
// These commands do NOT go through plugin.PluginDB -- each opens its own
// *sql.DB from --dsn -- so engine.beginTenantTx never runs for them and nothing
// sets cleat.tenant_id. Once backup_config and backup_history carry policies
// calling cleat.assert_tenant_set(), every statement fails. That is what
// shipped broken for `cleat jobqueue-enqueue` in cleat#1511 (fixed in #1517),
// so the policy and the CLI change land together here.
//
// A CONNECTION, NOT A TRANSACTION, and the difference is behavioural rather
// than stylistic. cliBackupRun records its own failures -- it writes
// `UPDATE backup_history SET status = 'failed'` and then returns an error.
// Wrapping the command in one transaction would roll that record back on
// exactly the path that needs it most, turning a recorded failure into a row
// stuck at 'running', which is the state cleat#1305 added markBackupFailed to
// avoid. Statements must keep autocommitting independently.
//
// A bare SET on the *sql.DB will not do either: it is a pool, and the SET may
// land on a different connection from the statements. Pinning one connection
// is what makes a session-level setting reliable.
//
// is_local is false here BECAUSE there is no transaction to scope it to. The
// returned cleanup RESETs it before releasing the connection, so it cannot
// follow the connection back into the pool -- the hazard beginTenantTx avoids
// with is_local=true.
func connScopedToTenant(ctx context.Context, db *sql.DB, tenantID uuid.UUID) (*sql.Conn, func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`SELECT set_config('cleat.tenant_id', $1, false)`, tenantID.String()); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("set tenant scope: %w", err)
	}
	return conn, func() {
		_, _ = conn.ExecContext(ctx, `SELECT set_config('cleat.tenant_id', '', false)`)
		_ = conn.Close()
	}, nil
}
