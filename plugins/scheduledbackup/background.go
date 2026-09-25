package scheduledbackup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
)

// Stable codes recorded in backup_history.error_message, replacing raw
// pg_dump stderr / Go error text there. cleat#2247: backup configuration
// became operator-only, but backup_history is still read through
// `cleatctl backup-history` -- a less controlled surface than the worker's
// own structured log -- so the row itself carries a fixed code rather than
// arbitrary text that might embed a DSN, a hostname, or other detail an
// operator's log stream is the right place for. The full detail is always
// still logged via p.logger.Error at the call site that produced it.
const (
	backupErrDSNUnavailable = "dsn_unavailable"
	backupErrUnsafePath     = "unsafe_path"
	backupErrPgDumpFailed   = "pg_dump_failed"
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
//
// No tenant_id (cleat#2247): backup_config stopped being a tenant-scoped
// table in the v4 migration, so this is a single global sweep -- there is
// exactly one operator, not one per tenant, and nothing here needs to know
// which tenant a config used to belong to.
var dueBackupsQuery = plugin.Query{
	Default: `
		SELECT id, name, cron
		FROM backup_config
		WHERE enabled = true AND next_run_at <= now()
		FOR UPDATE SKIP LOCKED`,
	MySQL: `
		SELECT id, name, cron
		FROM backup_config
		WHERE enabled = true AND next_run_at <= NOW()
		FOR UPDATE SKIP LOCKED`,
	MSSQL: `
		SELECT id, name, cron
		FROM backup_config WITH (UPDLOCK, READPAST, ROWLOCK)
		WHERE enabled = 1 AND next_run_at <= now()`,
}

// Run starts the background backup scheduler loop. Every 60 seconds it queries
// the backup_config table for enabled configs whose next_run_at <= now() and
// dispatches pg_dump for each due backup. Returns promptly when ctx is
// cancelled -- it never blocks waiting for a backup to finish.
//
// A due backup runs on its own goroutine (see runDueBackups), deliberately
// detached from ctx: a backup already in flight when ctx is cancelled is left
// to run to completion rather than killed, because an interrupted pg_dump is
// a data-safety problem, not a shutdown-latency one. executeScheduledBackup's
// atomic rename is what makes that safe -- an interrupted backup never
// produces a file at its final name and is never recorded as completed. So
// Run returning does not mean every backup it dispatched has finished.
//
// A manual "run now" (cleatctl's `backup-run`, cleat#2247) is not a separate
// code path: it sets next_run_at = now() on the config and lets this same
// loop pick it up on its next tick (at most 60s later) or the next explicit
// call to runDueBackups. There is exactly one place pg_dump is invoked from.
//
// This used to also exit early -- parking on <-ctx.Done() like the p.db==nil
// case still does -- when p.config.DSN was empty at boot. cleat#1992 part 1b
// moved the DSN to a deployment secret fetched per attempt (backupDSN,
// plugin.go), so there is no longer a value to check here: the whole point
// of a deployment secret is that setting one after the worker has already
// started takes effect without a restart, and caching "unset" at Run's
// start would have defeated that for exactly this plugin. The scheduler
// plugin's own Run (plugins/scheduler/background.go) is the precedent this
// follows -- it gates only on p.db==nil and polls unconditionally, letting
// "nothing to do" fall out of an empty due-backups query rather than being
// special-cased at startup. An unresolvable DSN now surfaces per attempt,
// in executeScheduledBackup, recorded in backup_history like any other
// pg_dump failure.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("scheduledbackup: no database, background loop disabled")
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
//
// No tenant scoping anywhere in this function (cleat#2247): backup_config and
// backup_history are operator-only tables now, so there is nothing here for
// plugin.AcrossAllTenants/plugin.ForTenant to bypass or re-scope -- ctx is
// used as-is throughout, exactly like plugins/scheduler's own equivalent
// sweep before scheduler ever grew tenant awareness.
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
		if err := plugin.ScanRow(rows, &b.id, &b.name, &b.cronExpr); err != nil {
			p.logger.Error("scheduledbackup: scan due backup", "error", err)
			continue
		}
		due = append(due, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		p.logger.Error("scheduledbackup: rows iteration error", "error", err)
	}

	// updateNextRunTx runs a second statement on the same transaction, which
	// must happen after rows is closed (cleat#2291): a statement issued while
	// the due-backups result set is still open fails outright on MySQL
	// ("driver: bad connection", so the commit below never dispatches
	// anything) and on PostgreSQL leaves next_run_at unadvanced (the pq
	// driver refuses a second query on the same connection while one is
	// already in flight, so the config stays due and a second worker can
	// dispatch it again).
	//
	// Its error is no longer just logged (cleat#2291 follow-up): a config
	// whose advance fails is dropped from toDispatch rather than fired
	// blind. On a fatal, connection-level failure every remaining Exec on
	// this tx fails the same way and Commit below fails too, so nothing in
	// this batch dispatches -- the existing safety net. On an isolated,
	// per-row failure (this tx and connection otherwise healthy) the rest of
	// the batch is unaffected, and only the failed config is left due to be
	// retried on the next poll, rather than dispatched without a durable
	// advance behind it.
	toDispatch := due[:0]
	for _, b := range due {
		if err := p.updateNextRunTx(ctx, tx, b.id, b.cronExpr); err != nil {
			p.logger.Error("scheduledbackup: update next_run_at (tx)",
				"config_id", b.id, "error", err)
			continue
		}
		toDispatch = append(toDispatch, b)
	}

	if err := tx.Commit(); err != nil {
		p.logger.Error("scheduledbackup: commit transaction", "error", err)
		return
	}

	for _, b := range toDispatch {
		b := b
		p.logger.Info("scheduledbackup: dispatching scheduled backup",
			"config_id", b.id, "name", b.name)
		// Use a background context so the backup completes even if the
		// originating ticker context is cancelled.
		//
		// Run on its own goroutine, not inline, so a due backup can never
		// hold Run's own goroutine hostage -- Run must return promptly on
		// cancel even while a backup is running (cleat#2055). bgBackups lets
		// a test wait for it to actually finish instead of sleeping.
		//
		// plugin.RecoverGoroutine, not a bare go func: an unrecovered panic
		// here kills the worker PROCESS and every in-flight workflow with it
		// (cleat#1769), not just this one backup.
		p.bgBackups.Add(1)
		go func() {
			defer p.bgBackups.Done()
			plugin.RecoverGoroutine("scheduled-backup", nil, func() {
				p.executeScheduledBackup(context.Background(), b.id, b.name, b.cronExpr)
			})
		}()
	}
}

// executeScheduledBackup runs pg_dump for a single backup config and records
// the result in backup_history. It is the ONLY place pg_dump is invoked from
// (cleat#2247): a manual "run now" via cleatctl only sets next_run_at, so it
// is picked up by the very next call to runDueBackups rather than executing
// on its own path.
func (p *Plugin) executeScheduledBackup(ctx context.Context, configID uuid.UUID, name, cronExpr string) {
	now := time.Now()
	filename := fmt.Sprintf("scheduled_%s_%s.dump", name, now.Format("20060102150405"))

	// bookkeepCtx strips ctx's cancellation: this goroutine is already
	// detached from Run's ctx (see runDueBackups) so ctx is never cancelled
	// in production, but a test exercising the interrupted-backup path
	// directly can cancel it, and a bookkeeping write must still land
	// regardless -- a write lost to a cancelled context would leave the row
	// at 'running' until the 1-hour orphan sweep instead of promptly.
	bookkeepCtx := context.WithoutCancel(ctx)

	// Create history entry with status "running".
	historyID := uuid.New()
	_, err := p.db.Exec(bookkeepCtx, plugin.Rebind(`
		INSERT INTO backup_history (id, config_id, filename, status, started_at, created_at)
		VALUES ($1, $2, $3, 'running', $4, $4)
	`, p.dialect), historyID, configID, filename, now)
	if err != nil {
		p.logger.Error("scheduledbackup: create history entry", "config_id", configID, "error", err)
		return
	}

	// Fetched per attempt, not cached -- see backupDSN's doc comment
	// (plugin.go) and Run's, above, for why this can no longer be a
	// precondition checked once at Run's start.
	//
	// Nothing in this function touches next_run_at/last_run_at, deliberately
	// (cleat#2291 follow-up): runDueBackups already stamped both for this
	// config inside its claim transaction, via updateNextRunTx, before this
	// goroutine was even dispatched, so the schedule has already advanced
	// and will retry on its own. A second, completion-time recompute here
	// used to run regardless of outcome -- and cleat-review measured it
	// clobbering a manual "run now" trigger (cleatctl backup-run, which sets
	// next_run_at = now() to be picked up by the next poll): if that trigger
	// landed while this backup was still in flight, this call would
	// overwrite it with a value computed from cronExpr, silently discarding
	// the manual request.
	dsn, err := p.backupDSN(bookkeepCtx)
	if err != nil {
		p.logger.Error("scheduledbackup: refusing scheduled backup",
			"config_id", configID, "history_id", historyID, "error", err)
		p.markBackupFailed(bookkeepCtx, historyID, backupErrDSNUnavailable)
		return
	}

	// Execute pg_dump.
	//
	// SafeDumpPath rather than a bare Join (cleat#1305). This path never passes
	// through an HTTP handler, so the name validation on cleatctl's
	// backup-config-create/update commands does not reach it: a row already
	// in backup_config with a traversing name is executed from here on a
	// schedule, by the worker, with nobody watching. This is the guard that
	// covers those rows.
	dumpPath, err := SafeDumpPath(p.config.DumpDir, filename)
	if err != nil {
		p.logger.Error("scheduledbackup: refusing scheduled backup",
			"config_id", configID, "history_id", historyID, "error", err)
		p.markBackupFailed(bookkeepCtx, historyID, backupErrUnsafePath)
		return
	}

	// pg_dump writes to a temporary name and is renamed to dumpPath ONLY on
	// success, so a backup killed mid-dump -- by worker shutdown, a crash, or
	// anything else -- never leaves a file at the name a restore would look
	// for, and backup_history is never left saying 'completed' for it. It
	// stays 'running' (cleaned up below on failure, or by the 1-hour orphan
	// sweep in cleanupOrphanedHistory if even that update is lost).
	tmpPath := dumpPath + ".partial"
	var stderr bytes.Buffer
	dumpErr := runPgDump(ctx, dsn, tmpPath, &stderr)
	if dumpErr == nil {
		if renameErr := os.Rename(tmpPath, dumpPath); renameErr != nil {
			dumpErr = fmt.Errorf("rename partial dump to final name: %w", renameErr)
		}
	}
	if dumpErr != nil {
		os.Remove(tmpPath) // best-effort: pg_dump may have written partial data
		errMsg := stderr.String()
		if errMsg == "" {
			errMsg = dumpErr.Error()
		}
		p.logger.Error("scheduledbackup: pg_dump failed",
			"config_id", configID, "history_id", historyID, "error", errMsg,
		)

		p.db.Exec(bookkeepCtx, plugin.Rebind(`
			UPDATE backup_history SET status = 'failed', error_message = $1, completed_at = now()
			WHERE id = $2
		`, p.dialect), backupErrPgDumpFailed, historyID)
		return
	}

	// Read the file size, from the final path -- the rename above already
	// succeeded, so this is the completed dump, not the partial one.
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

	p.db.Exec(bookkeepCtx, plugin.Rebind(`
		UPDATE backup_history SET status = 'completed', size_bytes = $1, completed_at = now()
		WHERE id = $2
	`, p.dialect), sizeBytes, historyID)
}

// markBackupFailed records a failed backup attempt in backup_history, under
// one of the stable codes above rather than raw error text -- see their doc
// comment for why. Factored out when SafeDumpPath gained a refusal path
// (cleat#1305): a refused backup must leave the same trail as a failed one.
// Recording only the pg_dump failure would leave a history row stuck at
// 'running' forever for a config whose name the path check rejects -- which
// reads as a hung backup rather than a refused one, and is the state an
// operator would escalate.
//
// Takes ctx rather than building its own context.Background(), matching
// every other bookkeeping write in this file: the call site already holds
// bookkeepCtx, and building a second, unrelated background context here
// would be a second thing that could silently diverge from the first. There
// is nothing here for plugin.AcrossAllTenants/plugin.ForTenant to bypass or
// re-scope -- see runDueBackups' doc comment -- so plain ctx is correct, not
// a shortcut.
func (p *Plugin) markBackupFailed(ctx context.Context, historyID uuid.UUID, errCode string) {
	if _, err := p.db.Exec(ctx, plugin.Rebind(`
		UPDATE backup_history SET status = 'failed', error_message = $1, completed_at = now()
		WHERE id = $2
	`, p.dialect), errCode, historyID); err != nil {
		p.logger.Error("scheduledbackup: recording backup failure", "history_id", historyID, "error", err)
	}
}

// updateNextRunTx calculates and advances the next_run_at/last_run_at for a
// due backup config, on the claim transaction runDueBackups already holds.
// It is the ONLY place either column is written (cleat#2291 follow-up):
// executeScheduledBackup used to recompute and overwrite them again after
// the backup finished, which could clobber a manual "run now" trigger
// (cleatctl backup-run) issued while that backup was still in flight -- see
// executeScheduledBackup's doc comment.
func (p *Plugin) updateNextRunTx(ctx context.Context, tx plugin.PluginTx, configID uuid.UUID, cronExpr string) error {
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
	return err
}
