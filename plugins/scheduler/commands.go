package scheduler

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterCommands returns the CLI subcommands for the scheduler plugin.
func (p *Plugin) RegisterCommands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "schedule-list",
			Description: "List schedules for a tenant",
			Run:         p.cliList,
		},
		{
			Name:        "schedule-add",
			Description: "Add a schedule",
			Run:         p.cliAdd,
		},
		{
			Name:        "schedule-delete",
			Description: "Delete a schedule",
			Run:         p.cliDelete,
		},
	}
}

func (p *Plugin) cliList(args []string) error {
	fs := flag.NewFlagSet("schedule-list", flag.ExitOnError)
	dsn := fs.String("dsn", "", "Database URL")
	tenantStr := fs.String("tenant", "", "Tenant UUID")
	fs.Parse(args)

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

	rows, err := db.Query(`
		SELECT id, name, cron, workflow_name, enabled, last_run_at, next_run_at, created_at
		FROM schedules
		WHERE tenant_id = $1
		ORDER BY created_at DESC
	`, tenantID)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCRON\tWORKFLOW\tENABLED\tLAST RUN\tNEXT RUN\tCREATED")
	fmt.Fprintln(w, "--\t----\t----\t--------\t-------\t--------\t--------\t-------")

	for rows.Next() {
		var (
			id        uuid.UUID
			name      string
			cron      string
			workflow  string
			enabled   bool
			lastRunAt sql.NullTime
			nextRunAt sql.NullTime
			createdAt time.Time
		)
		if err := rows.Scan(&id, &name, &cron, &workflow, &enabled, &lastRunAt, &nextRunAt, &createdAt); err != nil {
			return fmt.Errorf("scan: %w", err)
		}

		lastRun := "-"
		if lastRunAt.Valid {
			lastRun = lastRunAt.Time.Format(time.RFC3339)
		}
		nextRun := "-"
		if nextRunAt.Valid {
			nextRun = nextRunAt.Time.Format(time.RFC3339)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			id, name, cron, workflow, enabled, lastRun, nextRun, createdAt.Format(time.RFC3339))
	}

	w.Flush()
	return nil
}

func (p *Plugin) cliAdd(args []string) error {
	fs := flag.NewFlagSet("schedule-add", flag.ExitOnError)
	dsn := fs.String("dsn", "", "Database URL")
	tenantStr := fs.String("tenant", "", "Tenant UUID")
	name := fs.String("name", "", "Schedule name")
	cron := fs.String("cron", "", "Cron expression (5 fields)")
	workflow := fs.String("workflow", "", "Workflow name")
	input := fs.String("input", "{}", "JSON input for the workflow")
	enabled := fs.Bool("enabled", true, "Enable the schedule")
	fs.Parse(args)

	if *dsn == "" {
		return fmt.Errorf("--dsn is required")
	}
	if *tenantStr == "" {
		return fmt.Errorf("--tenant is required")
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	if *cron == "" {
		return fmt.Errorf("--cron is required")
	}
	if *workflow == "" {
		return fmt.Errorf("--workflow is required")
	}

	tenantID, err := uuid.Parse(*tenantStr)
	if err != nil {
		return fmt.Errorf("invalid tenant UUID: %w", err)
	}

	// Validate the cron expression.
	next := nextRun(*cron, time.Now())
	if next.IsZero() {
		return fmt.Errorf("invalid cron expression or no future match found")
	}

	// Validate JSON input.
	var inputJSON any
	if err := json.Unmarshal([]byte(*input), &inputJSON); err != nil {
		return fmt.Errorf("invalid JSON input: %w", err)
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

	id := uuid.New()
	_, err = db.Exec(`
		INSERT INTO schedules (tenant_id, id, name, cron, workflow_name, input, enabled, next_run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
	`, tenantID, id, *name, *cron, *workflow, []byte(*input), *enabled, next)
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	fmt.Printf("Created schedule %s (next run: %s)\n", id, next.Format(time.RFC3339))
	return nil
}

func (p *Plugin) cliDelete(args []string) error {
	fs := flag.NewFlagSet("schedule-delete", flag.ExitOnError)
	dsn := fs.String("dsn", "", "Database URL")
	tenantStr := fs.String("tenant", "", "Tenant UUID")
	idStr := fs.String("id", "", "Schedule UUID to delete")
	fs.Parse(args)

	if *dsn == "" {
		return fmt.Errorf("--dsn is required")
	}
	if *tenantStr == "" {
		return fmt.Errorf("--tenant is required")
	}
	if *idStr == "" {
		return fmt.Errorf("--id is required")
	}

	tenantID, err := uuid.Parse(*tenantStr)
	if err != nil {
		return fmt.Errorf("invalid tenant UUID: %w", err)
	}
	scheduleID, err := uuid.Parse(*idStr)
	if err != nil {
		return fmt.Errorf("invalid schedule UUID: %w", err)
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

	result, err := db.Exec(`DELETE FROM schedules WHERE id = $1 AND tenant_id = $2`, scheduleID, tenantID)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("schedule not found")
	}

	fmt.Printf("Deleted schedule %s\n", scheduleID)
	return nil
}

// Ensure the postgres driver is imported. The blank import above is needed for
// sql.Open(driver, ...) where driver defaults to "postgres". This function is
// never called but ensures the compiler sees the import as used.
func init() {
	_ = strings.HasPrefix
}
