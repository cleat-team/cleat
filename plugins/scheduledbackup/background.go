package scheduledbackup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// Run starts the background backup scheduler loop. Every 60 seconds it queries
// the backup_config table for enabled configs whose next_run_at <= now() and
// executes pg_dump for each due backup. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("scheduledbackup: no database, background loop disabled")
		<-ctx.Done()
		return nil
	}

	if p.config.DSN == "" {
		p.logger.Warn("scheduledbackup: no DSN configured, background loop disabled")
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	p.logger.Info("scheduledbackup: background loop started, interval=60s")

	// Run once immediately on startup.
	p.runDueBackups(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("scheduledbackup: background loop stopped")
			return nil

		case <-ticker.C:
			p.runDueBackups(ctx)
		}
	}
}

// runDueBackups finds enabled backup configs where next_run_at <= now()
// and executes pg_dump for each. After execution, it updates next_run_at
// and last_run_at on the config and records the result in backup_history.
func (p *Plugin) runDueBackups(ctx context.Context) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, cron
		FROM backup_config
		WHERE enabled = true AND next_run_at <= now()
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		p.logger.Error("scheduledbackup: query due backups", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id       uuid.UUID
			tenantID uuid.UUID
			name     string
			cronExpr string
		)
		if err := rows.Scan(&id, &tenantID, &name, &cronExpr); err != nil {
			p.logger.Error("scheduledbackup: scan due backup", "error", err)
			continue
		}

		p.logger.Info("scheduledbackup: running scheduled backup",
			"config_id", id,
			"tenant", tenantID,
			"name", name,
		)

		p.executeScheduledBackup(context.Background(), id, tenantID, name, cronExpr)
	}

	if err := rows.Err(); err != nil {
		p.logger.Error("scheduledbackup: rows iteration error", "error", err)
	}
}

// executeScheduledBackup runs pg_dump for a single backup config and records
// the result in backup_history.
func (p *Plugin) executeScheduledBackup(ctx context.Context, configID, tenantID uuid.UUID, name, cronExpr string) {
	now := time.Now()
	filename := fmt.Sprintf("scheduled_%s_%s.dump", name, now.Format("20060102150405"))

	// Create history entry with status "running".
	historyID := uuid.New()
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO backup_history (id, config_id, tenant_id, filename, status, started_at, created_at)
		VALUES ($1, $2, $3, $4, 'running', $5, $5)
	`, historyID, configID, tenantID, filename, now)
	if err != nil {
		p.logger.Error("scheduledbackup: create history entry", "config_id", configID, "error", err)
		return
	}

	// Execute pg_dump.
	dumpPath := filepath.Join(p.config.DumpDir, filename)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "pg_dump", "-f", dumpPath, p.config.DSN)
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = err.Error()
		}
		p.logger.Error("scheduledbackup: pg_dump failed",
			"config_id", configID, "history_id", historyID, "error", errMsg,
		)

		p.db.ExecContext(ctx, `
			UPDATE backup_history SET status = 'failed', error_message = $1, completed_at = now()
			WHERE id = $2
		`, errMsg, historyID)

		// Still update next_run_at so the schedule can try again later.
		p.updateNextRun(ctx, configID, cronExpr, now)
		return
	}

	// Read the file size.
	var sizeBytes int64
	if fi, fiErr := os.Stat(dumpPath); fiErr == nil {
		sizeBytes = fi.Size()
	}

	p.logger.Info("scheduledbackup: scheduled backup completed",
		"config_id", configID,
		"history_id", historyID,
		"filename", filename,
		"size_bytes", sizeBytes,
	)

	p.db.ExecContext(ctx, `
		UPDATE backup_history SET status = 'completed', size_bytes = $1, completed_at = now()
		WHERE id = $2
	`, sizeBytes, historyID)

	p.updateNextRun(ctx, configID, cronExpr, time.Now())
}

// updateNextRun calculates and updates the next_run_at and last_run_at for a
// backup config after a backup attempt (successful or failed).
func (p *Plugin) updateNextRun(ctx context.Context, configID uuid.UUID, cronExpr string, now time.Time) {
	next := nextRun(cronExpr, now)
	var nextRunAt *time.Time
	if !next.IsZero() {
		nextRunAt = &next
	}

	_, err := p.db.ExecContext(ctx, `
		UPDATE backup_config
		SET last_run_at = $1, next_run_at = $2, updated_at = now()
		WHERE id = $3
	`, now, nextRunAt, configID)
	if err != nil {
		p.logger.Error("scheduledbackup: update next_run_at",
			"config_id", configID, "error", err)
	}
}
