package scheduledbackup

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/rcownie/cleat/internal/plugin"
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

	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	// Fetch the backup config.
	var name, cronExpr, s3Bucket, s3Prefix string
	var retentionDays int
	err = db.QueryRow(`
		SELECT name, cron, s3_bucket, s3_prefix, retention_days
		FROM backup_config WHERE id = $1 AND tenant_id = $2
	`, configID, tenantID).Scan(&name, &cronExpr, &s3Bucket, &s3Prefix, &retentionDays)
	if err == sql.ErrNoRows {
		return fmt.Errorf("backup config not found")
	}
	if err != nil {
		return fmt.Errorf("fetch config: %w", err)
	}

	// Create history entry.
	historyID := uuid.New()
	now := time.Now()
	filename := fmt.Sprintf("manual_%s_%s.dump", name, now.Format("20060102150405"))

	_, err = db.Exec(`
		INSERT INTO backup_history (id, config_id, tenant_id, filename, status, started_at, created_at)
		VALUES ($1, $2, $3, $4, 'running', $5, $5)
	`, historyID, configID, tenantID, filename, now)
	if err != nil {
		return fmt.Errorf("create history entry: %w", err)
	}

	fmt.Printf("Starting backup %s for config %q...\n", historyID, name)

	// Execute pg_dump.
	dumpPath := filepath.Join(*dumpDir, filename)
	if err := os.MkdirAll(*dumpDir, 0755); err != nil {
		return fmt.Errorf("create dump dir: %w", err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command("pg_dump", "-f", dumpPath, *dsn)
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}

		db.Exec(`
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
	_, err = db.Exec(`
		UPDATE backup_history SET status = 'completed', size_bytes = $1, completed_at = now()
		WHERE id = $2
	`, sizeBytes, historyID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update history: %v\n", err)
	}

	// Update config last_run_at and next_run_at.
	if nxt := nextRun(cronExpr, time.Now()); !nxt.IsZero() {
		db.Exec(`
			UPDATE backup_config SET last_run_at = $1, next_run_at = $2, updated_at = now()
			WHERE id = $3
		`, time.Now(), nxt, configID)
	} else {
		db.Exec(`
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

	db, err := sql.Open("postgres", *dsn)
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
	qargs := []interface{}{tenantID}
	argIdx := 2

	if *configStr != "" {
		cfgID, cfgErr := uuid.Parse(*configStr)
		if cfgErr != nil {
			return fmt.Errorf("invalid config UUID: %w", cfgErr)
		}
		query += fmt.Sprintf(" AND h.config_id = $%d", argIdx)
		qargs = append(qargs, cfgID)
		argIdx++
	}

	query += " ORDER BY h.started_at DESC"

	rows, err := db.Query(query, qargs...)
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
		if err := rows.Scan(&hid, &cid, &fn, &sizeBytes, &status, &startedAt, &completedAt, &errorMessage); err != nil {
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
