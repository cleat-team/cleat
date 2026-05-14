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

	"github.com/cleat-team/cleat/internal/plugin"
)

// cleanupOrphanedHistoryQuery marks running backup_history rows as failed when
// their started_at is more than 1 hour ago (worker crashed or timed out).
var cleanupOrphanedHistoryQuery = plugin.Query{
	Default: `
		UPDATE backup_history
		SET status = 'failed', error_message = 'worker crashed or timed out',
		    completed_at = now()
		WHERE status = 'running'
		  AND started_at < now() - INTERVAL '1 hour'`,
	MySQL: `
		UPDATE backup_history
		SET status = 'failed', error_message = 'worker crashed or timed out',
		    completed_at = NOW()
		WHERE status = 'running'
		  AND started_at < NOW() - INTERVAL 1 HOUR`,
	MSSQL: `
		UPDATE backup_history
		SET status = 'failed', error_message = 'worker crashed or timed out',
		    completed_at = SYSUTCDATETIME()
		WHERE status = 'running'
		  AND started_at < DATEADD(hour, -1, SYSUTCDATETIME())`,
}

// dueBackupsQuery provides dialect-specific FOR UPDATE SKIP LOCKED equivalents.
var dueBackupsQuery = plugin.Query{
	Default: `
		SELECT id, tenant_id, name, cron
		FROM backup_config
		WHERE enabled = true AND next_run_at <= now()
		FOR UPDATE SKIP LOCKED`,
	MySQL: `
		SELECT id, tenant_id, name, cron
		FROM backup_config
		WHERE enabled = true AND next_run_at <= NOW()
		FOR UPDATE SKIP LOCKED`,
	MSSQL: `
		SELECT id, tenant_id, name, cron
		FROM backup_config WITH (UPDLOCK, READPAST, ROWLOCK)
		WHERE enabled = true AND next_run_at <= now()`,
}

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

// dueBackup holds a backup config row claimed from the database.
type dueBackup struct {
	id       uuid.UUID
	tenantID uuid.UUID
	name     string
	cronExpr string
}

// cleanupOrphanedHistory marks running backup_history entries with a started_at
// older than 1 hour as failed, under the assumption that the worker crashed or
// timed out.
func (p *Plugin) cleanupOrphanedHistory(ctx context.Context) {
	result, err := p.db.Exec(ctx, plugin.Rebind(cleanupOrphanedHistoryQuery.For(p.dialect), p.dialect))
	if err != nil {
		p.logger.Error("scheduledbackup: cleanup orphaned history", "error", err)
		return
	}
	if result > 0 {
		p.logger.Info("scheduledbackup: cleaned up orphaned history entries", "count", result)
	}
}

// runDueBackups atomically claims due backup configs via FOR UPDATE SKIP
// LOCKED inside a transaction, advances their next_run_at immediately, then
// executes pg_dump outside the transaction so row locks are not held across
// potentially-long backup operations.
func (p *Plugin) runDueBackups(ctx context.Context) {
	p.cleanupOrphanedHistory(ctx)

	tx, err := p.db.Begin(ctx)
	if err != nil {
		p.logger.Error("scheduledbackup: begin transaction", "error", err)
		return
	}
	defer tx.Rollback() // no-op after Commit

	rows, err := tx.Query(ctx, plugin.Rebind(dueBackupsQuery.For(p.dialect), p.dialect))
	if err != nil {
		p.logger.Error("scheduledbackup: query due backups", "error", err)
		return
	}

	var due []dueBackup
	for rows.Next() {
		var b dueBackup
		if err := rows.Scan(&b.id, &b.tenantID, &b.name, &b.cronExpr); err != nil {
			p.logger.Error("scheduledbackup: scan due backup", "error", err)
			continue
		}
		due = append(due, b)

		// Advance next_run_at under the transaction lock so other
		// workers skip this config even if this worker crashes
		// before completing the backup.
		p.updateNextRunTx(ctx, tx, b.id, b.cronExpr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		p.logger.Error("scheduledbackup: rows iteration error", "error", err)
	}

	if err := tx.Commit(); err != nil {
		p.logger.Error("scheduledbackup: commit transaction", "error", err)
		return
	}

	for _, b := range due {
		p.logger.Info("scheduledbackup: running scheduled backup",
			"config_id", b.id, "tenant", b.tenantID, "name", b.name)
		// Use a background context so the backup completes even if
		// the originating ticker context is cancelled.
		p.executeScheduledBackup(context.Background(), b.id, b.tenantID, b.name, b.cronExpr)
	}
}

// executeScheduledBackup runs pg_dump for a single backup config and records
// the result in backup_history.
func (p *Plugin) executeScheduledBackup(ctx context.Context, configID, tenantID uuid.UUID, name, cronExpr string) {
	now := time.Now()
	filename := fmt.Sprintf("scheduled_%s_%s.dump", name, now.Format("20060102150405"))

	// Create history entry with status "running".
	historyID := uuid.New()
	_, err := p.db.Exec(ctx, plugin.Rebind(`
		INSERT INTO backup_history (id, config_id, tenant_id, filename, status, started_at, created_at)
		VALUES ($1, $2, $3, $4, 'running', $5, $5)
	`, p.dialect), historyID, configID, tenantID, filename, now)
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

		p.db.Exec(ctx, plugin.Rebind(`
			UPDATE backup_history SET status = 'failed', error_message = $1, completed_at = now()
			WHERE id = $2
		`, p.dialect), errMsg, historyID)

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

	p.db.Exec(ctx, plugin.Rebind(`
		UPDATE backup_history SET status = 'completed', size_bytes = $1, completed_at = now()
		WHERE id = $2
	`, p.dialect), sizeBytes, historyID)

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

	_, err := p.db.Exec(ctx, plugin.Rebind(`
		UPDATE backup_config
		SET last_run_at = $1, next_run_at = $2, updated_at = now()
		WHERE id = $3
	`, p.dialect), now, nextRunAt, configID)
	if err != nil {
		p.logger.Error("scheduledbackup: update next_run_at",
			"config_id", configID, "error", err)
	}
}

// updateNextRunTx is like updateNextRun but runs on an existing transaction.
func (p *Plugin) updateNextRunTx(ctx context.Context, tx plugin.PluginTx, configID uuid.UUID, cronExpr string) {
	now := time.Now()
	next := nextRun(cronExpr, now)
	var nextRunAt *time.Time
	if !next.IsZero() {
		nextRunAt = &next
	}

	_, err := tx.Exec(ctx, plugin.Rebind(`
		UPDATE backup_config
		SET last_run_at = $1, next_run_at = $2, updated_at = now()
		WHERE id = $3
	`, p.dialect), now, nextRunAt, configID)
	if err != nil {
		p.logger.Error("scheduledbackup: update next_run_at (tx)",
			"config_id", configID, "error", err)
	}
}
