package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/scheduledbackup"
)

// cleat#2247: scheduledbackup lost its tenant-facing HTTP API and its
// (never-wired, dead-code) CLI-command mechanism -- backup_config and
// backup_history stopped being tenant-scoped tables in the plugin's v4
// migration, and there is exactly one operator, not one per tenant. This is
// the sole surface left for managing them: cleatctl backup, following the
// same direct-SQL-against-*sql.DB pattern as slackworkspace.go and
// quota.go, since a bespoke plugin-CLI mechanism (plugin.HasCommands) that
// nothing ever called was not a seam worth keeping.
//
// "run" is REQUEST-only: it sets next_run_at = now() and lets
// scheduledbackup's own background loop (background.go's Run) pick the
// config up on its next tick, at most 60s later -- there is exactly one
// place pg_dump is invoked from, and it is not this process. See
// executeScheduledBackup's doc comment for the other half of that.
const (
	// $7 and $8, not $7 twice for created_at/updated_at: MySQL's rebound `?`
	// binds by APPEARANCE, not by number (CLAUDE.md), so a repeated $7
	// becomes two separate `?` slots there and the call site must supply
	// two arguments, even though Postgres and SQL Server bind $7 to the
	// same one twice. Confirmed directly -- an earlier version of this
	// statement with a single $7 reused failed on MySQL only, with
	// "sql: expected 8 arguments, got 7".
	backupConfigCreateSQL = `INSERT INTO backup_config
		(id, name, cron, retention_days, enabled, next_run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	backupConfigListSQL = `SELECT id, name, cron, retention_days, enabled, last_run_at, next_run_at, created_at, updated_at
		FROM backup_config ORDER BY name`
	backupConfigGetByIDSQL = `SELECT id, name, cron, retention_days, enabled
		FROM backup_config WHERE id = $1`
	backupConfigGetByNameSQL = `SELECT id, name, cron, retention_days, enabled
		FROM backup_config WHERE name = $1`
	backupConfigDeleteSQL = `DELETE FROM backup_config WHERE id = $1`
	// $1 and $2, not $1 reused: the same MySQL by-appearance binding trap as
	// backupConfigCreateSQL above.
	backupConfigRequestRunSQL = `UPDATE backup_config SET next_run_at = $1, updated_at = $2 WHERE id = $3`
)

// backupHistoryListSQL/backupHistoryListByConfigSQL are plugin.Query, not
// plain strings: LIMIT is PostgreSQL/MySQL syntax and SQL Server rejects it
// outright ("Incorrect syntax near 'LIMIT'"), so this needs a real MSSQL
// arm rather than a rewrite d.rebind can do by substitution alone.
// TestBackupCommandWorksOnEveryDialect/mssql failed on exactly this before
// the MSSQL arm existed -- `backup history` did not merely paginate wrong,
// it could not run at all.
var backupHistoryListSQL = plugin.Query{
	Default: `SELECT id, config_id, filename, size_bytes, status, started_at, completed_at, error_message
		FROM backup_history ORDER BY started_at DESC LIMIT $1`,
	MSSQL: `SELECT id, config_id, filename, size_bytes, status, started_at, completed_at, error_message
		FROM backup_history ORDER BY started_at DESC OFFSET 0 ROWS FETCH NEXT $1 ROWS ONLY`,
}

var backupHistoryListByConfigSQL = plugin.Query{
	Default: `SELECT id, config_id, filename, size_bytes, status, started_at, completed_at, error_message
		FROM backup_history WHERE config_id = $1 ORDER BY started_at DESC LIMIT $2`,
	MSSQL: `SELECT id, config_id, filename, size_bytes, status, started_at, completed_at, error_message
		FROM backup_history WHERE config_id = $1 ORDER BY started_at DESC OFFSET 0 ROWS FETCH NEXT $2 ROWS ONLY`,
}

func runBackup(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) < 1 {
		printBackupUsage()
		osExit(1)
		return
	}
	switch args[0] {
	case "config-create":
		runBackupConfigCreate(ctx, db, d, args[1:])
	case "config-list":
		runBackupConfigList(ctx, db, d, args[1:])
	case "config-update":
		runBackupConfigUpdate(ctx, db, d, args[1:])
	case "config-delete":
		runBackupConfigDelete(ctx, db, d, args[1:])
	case "run":
		runBackupRun(ctx, db, d, args[1:])
	case "history":
		runBackupHistory(ctx, db, d, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown backup subcommand: %s\n\n", args[0])
		printBackupUsage()
		osExit(1)
	}
}

func printBackupUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> backup <subcommand> [flags]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

  backup config-create --name <name> --cron "<cron expr>" [--retention-days N] [--disabled]
                                  create a scheduled backup config. name: %s
  backup config-list             list every backup config
  backup config-update (--id <uuid> | --name <name>) [--cron <expr>] [--retention-days N] (--enabled|--disabled)
                                  update one or more fields of an existing config
  backup config-delete (--id <uuid> | --name <name>)
                                  delete a backup config (its history rows are NOT deleted)
  backup run (--id <uuid> | --name <name>)
                                  request an immediate backup: sets next_run_at to now.
                                  Picked up by the background loop within 60s -- this
                                  does not run pg_dump itself.
  backup history [--id <uuid> | --name <name>] [--limit N]
                                  list backup attempts, newest first (default limit 50)
`, scheduledbackup.ConfigNameRule)
}

// backupFlags is the hand-rolled --flag/--flag=value scanner every cleatctl
// subcommand in this file uses, the same shape parseSlackFlags
// (slackworkspace.go) and parseQuotaFlags use and for the same reason:
// flag.FlagSet stops at the first non-flag argument.
type backupFlags struct {
	id            string
	name          string
	cron          string
	retentionDays string // parsed lazily: "" means not given, distinct from 0
	limit         string
	enabled       bool
	disabled      bool
}

func parseBackupFlags(args []string) (backupFlags, error) {
	var f backupFlags
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
				return "", fmt.Errorf("%s requires a value", key)
			}
			i++
			return args[i], nil
		}
		var err error
		switch key {
		case "--id":
			if f.id, err = next(); err != nil {
				return f, err
			}
		case "--name":
			if f.name, err = next(); err != nil {
				return f, err
			}
		case "--cron":
			if f.cron, err = next(); err != nil {
				return f, err
			}
		case "--retention-days":
			if f.retentionDays, err = next(); err != nil {
				return f, err
			}
		case "--limit":
			if f.limit, err = next(); err != nil {
				return f, err
			}
		case "--enabled":
			f.enabled = true
		case "--disabled":
			f.disabled = true
		default:
			return f, fmt.Errorf("unknown flag: %s", a)
		}
	}
	if f.enabled && f.disabled {
		return f, fmt.Errorf("--enabled and --disabled are mutually exclusive")
	}
	return f, nil
}

// resolveConfigID turns --id or --name into the config's id. Exactly one of
// idFlag/nameFlag must be non-empty -- checked by the caller, since the
// error message differs by subcommand (config-create has neither because it
// is the one command that MAKES an id).
//
// plugin.ScanRow, not a bare .Scan(&found): SQL Server returns
// UNIQUEIDENTIFIER in mixed-endian byte order, which uuid.UUID's own Scan
// accepts without error and turns into a DIFFERENT uuid (cleat#1137).
// Confirmed directly by TestBackupCommandWorksOnEveryDialect/mssql, before
// this fix: config-create reported the correct id, but immediately looking
// it back up by --name (which round-trips it through exactly this Scan)
// returned an id that then matched no row at all.
func resolveConfigID(ctx context.Context, db *sql.DB, d dialect, idFlag, nameFlag string) (uuid.UUID, error) {
	if idFlag != "" {
		id, err := uuid.Parse(idFlag)
		if err != nil {
			return uuid.Nil, fmt.Errorf("not a config UUID: %q: %w", idFlag, err)
		}
		var found uuid.UUID
		row := db.QueryRowContext(ctx, d.rebind(`SELECT id FROM backup_config WHERE id = $1`), id)
		if err := plugin.ScanRow(row, &found); err != nil {
			if err == sql.ErrNoRows {
				return uuid.Nil, fmt.Errorf("no backup config with id %s", id)
			}
			return uuid.Nil, fmt.Errorf("looking up config %s: %w", id, err)
		}
		return found, nil
	}

	var ids []uuid.UUID
	rows, err := db.QueryContext(ctx, d.rebind(`SELECT id FROM backup_config WHERE name = $1`), nameFlag)
	if err != nil {
		return uuid.Nil, fmt.Errorf("looking up config %q: %w", nameFlag, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := plugin.ScanRow(rows, &id); err != nil {
			return uuid.Nil, fmt.Errorf("reading config row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, fmt.Errorf("reading config rows: %w", err)
	}
	switch len(ids) {
	case 0:
		return uuid.Nil, fmt.Errorf("no backup config named %q", nameFlag)
	case 1:
		return ids[0], nil
	default:
		// name has no UNIQUE constraint (migrations.go v1): report the
		// ambiguity rather than silently picking one and touching the
		// wrong config.
		return uuid.Nil, fmt.Errorf("%d backup configs are named %q -- use --id instead", len(ids), nameFlag)
	}
}

func runBackupConfigCreate(ctx context.Context, db *sql.DB, d dialect, args []string) {
	f, err := parseBackupFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printBackupUsage()
		osExit(1)
		return
	}
	if f.name == "" || f.cron == "" {
		fmt.Fprintf(os.Stderr, "backup config-create requires --name and --cron\n\n")
		printBackupUsage()
		osExit(1)
		return
	}
	if !scheduledbackup.ValidConfigName(f.name) {
		fmt.Fprintf(os.Stderr, "invalid --name %q: %s\n", f.name, scheduledbackup.ConfigNameRule)
		osExit(1)
		return
	}
	now := time.Now().UTC()
	next := scheduledbackup.NextRun(f.cron, now)
	if next.IsZero() {
		fmt.Fprintf(os.Stderr, "invalid --cron %q: does not parse, or has no match within a year\n", f.cron)
		osExit(1)
		return
	}
	retentionDays := 30
	if f.retentionDays != "" {
		n, err := strconv.Atoi(f.retentionDays)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "invalid --retention-days %q: must be a non-negative integer\n", f.retentionDays)
			osExit(1)
			return
		}
		retentionDays = n
	}
	enabled := !f.disabled

	id := uuid.New()
	if _, err := db.ExecContext(ctx, d.rebind(backupConfigCreateSQL),
		id, f.name, f.cron, retentionDays, enabled, next, now, now); err != nil {
		fmt.Fprintf(os.Stderr, "creating backup config %q: %v\n", f.name, err)
		osExit(1)
		return
	}
	fmt.Printf("created backup config %s (%s), next run %s\n", f.name, id, next.Format(time.RFC3339))
}

func runBackupConfigList(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "backup config-list takes no arguments\n\n")
		printBackupUsage()
		osExit(1)
		return
	}
	rows, err := db.QueryContext(ctx, d.rebind(backupConfigListSQL))
	if err != nil {
		fmt.Fprintf(os.Stderr, "listing backup configs: %v\n", err)
		osExit(1)
		return
	}
	defer rows.Close()

	fmt.Printf("%-36s  %-20s  %-24s  %-9s  %-7s  %-25s  %-25s\n",
		"ID", "NAME", "CRON", "RETENTION", "ENABLED", "LAST_RUN_AT", "NEXT_RUN_AT")
	n := 0
	for rows.Next() {
		var id uuid.UUID
		var name, cron string
		var retentionDays int
		var enabled bool
		var lastRunAt, nextRunAt sql.NullTime
		var createdAt, updatedAt time.Time
		if err := plugin.ScanRow(rows, &id, &name, &cron, &retentionDays, &enabled, &lastRunAt, &nextRunAt, &createdAt, &updatedAt); err != nil {
			fmt.Fprintf(os.Stderr, "reading row: %v\n", err)
			osExit(1)
			return
		}
		fmt.Printf("%-36s  %-20s  %-24s  %-9d  %-7t  %-25s  %-25s\n",
			id, name, cron, retentionDays, enabled, formatNullTime(lastRunAt), formatNullTime(nextRunAt))
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "reading rows: %v\n", err)
		osExit(1)
		return
	}
	if n == 0 {
		fmt.Println("(no backup configs)")
	}
}

func formatNullTime(t sql.NullTime) string {
	if !t.Valid {
		return "-"
	}
	return t.Time.Format(time.RFC3339)
}

func runBackupConfigUpdate(ctx context.Context, db *sql.DB, d dialect, args []string) {
	f, err := parseBackupFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printBackupUsage()
		osExit(1)
		return
	}
	if f.id == "" && f.name == "" {
		fmt.Fprintf(os.Stderr, "backup config-update requires --id or --name\n\n")
		printBackupUsage()
		osExit(1)
		return
	}
	if f.cron == "" && f.retentionDays == "" && !f.enabled && !f.disabled {
		fmt.Fprintf(os.Stderr, "backup config-update requires at least one of --cron, --retention-days, --enabled, --disabled\n\n")
		printBackupUsage()
		osExit(1)
		return
	}
	id, err := resolveConfigID(ctx, db, d, f.id, f.name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}

	var sets []string
	var vals []any
	next := 1
	arg := func(v any) string {
		vals = append(vals, v)
		s := fmt.Sprintf("$%d", next)
		next++
		return s
	}

	if f.cron != "" {
		if scheduledbackup.NextRun(f.cron, time.Now().UTC()).IsZero() {
			fmt.Fprintf(os.Stderr, "invalid --cron %q: does not parse, or has no match within a year\n", f.cron)
			osExit(1)
			return
		}
		sets = append(sets, "cron = "+arg(f.cron))
		// Recompute next_run_at from the new schedule now, rather than
		// waiting for the background loop's next attempt to do it after a
		// backup runs -- otherwise a cron change with no backup due in the
		// meantime never takes effect.
		sets = append(sets, "next_run_at = "+arg(scheduledbackup.NextRun(f.cron, time.Now().UTC())))
	}
	if f.retentionDays != "" {
		n, err := strconv.Atoi(f.retentionDays)
		if err != nil || n < 0 {
			fmt.Fprintf(os.Stderr, "invalid --retention-days %q: must be a non-negative integer\n", f.retentionDays)
			osExit(1)
			return
		}
		sets = append(sets, "retention_days = "+arg(n))
	}
	if f.enabled {
		sets = append(sets, "enabled = "+arg(true))
	}
	if f.disabled {
		sets = append(sets, "enabled = "+arg(false))
	}
	sets = append(sets, "updated_at = "+arg(time.Now().UTC()))
	vals = append(vals, id)
	query := fmt.Sprintf("UPDATE backup_config SET %s WHERE id = $%d", strings.Join(sets, ", "), next)

	if _, err := db.ExecContext(ctx, d.rebind(query), vals...); err != nil {
		fmt.Fprintf(os.Stderr, "updating backup config %s: %v\n", id, err)
		osExit(1)
		return
	}
	fmt.Printf("updated backup config %s\n", id)
}

func runBackupConfigDelete(ctx context.Context, db *sql.DB, d dialect, args []string) {
	f, err := parseBackupFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printBackupUsage()
		osExit(1)
		return
	}
	if f.id == "" && f.name == "" {
		fmt.Fprintf(os.Stderr, "backup config-delete requires --id or --name\n\n")
		printBackupUsage()
		osExit(1)
		return
	}
	id, err := resolveConfigID(ctx, db, d, f.id, f.name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}
	if _, err := db.ExecContext(ctx, d.rebind(backupConfigDeleteSQL), id); err != nil {
		fmt.Fprintf(os.Stderr, "deleting backup config %s: %v\n", id, err)
		osExit(1)
		return
	}
	fmt.Printf("deleted backup config %s (its backup_history rows are unaffected)\n", id)
}

// runBackupRun is REQUEST-only. See this file's top comment: it does not
// invoke pg_dump, it only advances next_run_at so the background loop picks
// the config up on its own next tick.
func runBackupRun(ctx context.Context, db *sql.DB, d dialect, args []string) {
	f, err := parseBackupFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printBackupUsage()
		osExit(1)
		return
	}
	if f.id == "" && f.name == "" {
		fmt.Fprintf(os.Stderr, "backup run requires --id or --name\n\n")
		printBackupUsage()
		osExit(1)
		return
	}
	id, err := resolveConfigID(ctx, db, d, f.id, f.name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		osExit(1)
		return
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, d.rebind(backupConfigRequestRunSQL), now, now, id); err != nil {
		fmt.Fprintf(os.Stderr, "requesting a run for %s: %v\n", id, err)
		osExit(1)
		return
	}
	fmt.Printf("requested an immediate backup for %s -- picked up by the background loop within 60s\n", id)
}

func runBackupHistory(ctx context.Context, db *sql.DB, d dialect, args []string) {
	f, err := parseBackupFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		printBackupUsage()
		osExit(1)
		return
	}
	limit := 50
	if f.limit != "" {
		n, err := strconv.Atoi(f.limit)
		if err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "invalid --limit %q: must be a positive integer\n", f.limit)
			osExit(1)
			return
		}
		limit = n
	}

	var rows *sql.Rows
	if f.id != "" || f.name != "" {
		id, rerr := resolveConfigID(ctx, db, d, f.id, f.name)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", rerr)
			osExit(1)
			return
		}
		rows, err = db.QueryContext(ctx, d.rebind(backupHistoryListByConfigSQL.For(d.query)), id, limit)
	} else {
		rows, err = db.QueryContext(ctx, d.rebind(backupHistoryListSQL.For(d.query)), limit)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "listing backup history: %v\n", err)
		osExit(1)
		return
	}
	defer rows.Close()

	fmt.Printf("%-36s  %-36s  %-32s  %-10s  %-9s  %-25s  %-25s  %s\n",
		"ID", "CONFIG_ID", "FILENAME", "SIZE_BYTES", "STATUS", "STARTED_AT", "COMPLETED_AT", "ERROR")
	n := 0
	for rows.Next() {
		var id, configID uuid.UUID
		var filename, status string
		var sizeBytes sql.NullInt64
		var startedAt time.Time
		var completedAt sql.NullTime
		var errMsg sql.NullString
		if err := plugin.ScanRow(rows, &id, &configID, &filename, &sizeBytes, &status, &startedAt, &completedAt, &errMsg); err != nil {
			fmt.Fprintf(os.Stderr, "reading row: %v\n", err)
			osExit(1)
			return
		}
		size := "-"
		if sizeBytes.Valid {
			size = strconv.FormatInt(sizeBytes.Int64, 10)
		}
		errStr := "-"
		if errMsg.Valid && errMsg.String != "" {
			errStr = errMsg.String
		}
		fmt.Printf("%-36s  %-36s  %-32s  %-10s  %-9s  %-25s  %-25s  %s\n",
			id, configID, filename, size, status, startedAt.Format(time.RFC3339), formatNullTime(completedAt), errStr)
		n++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "reading rows: %v\n", err)
		osExit(1)
		return
	}
	if n == 0 {
		fmt.Println("(no backup history)")
	}
}
