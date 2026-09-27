package jobqueue

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterCommands returns CLI subcommands for the job queue.
func (p *Plugin) RegisterCommands() []plugin.Command {
	return []plugin.Command{{
		Name:        "jobqueue-enqueue",
		Description: "Enqueue a job (--tenant=<uuid> --queue=<name> [--payload='{}'] [--dsn=<url>])",
		Run: func(args []string) error {
			fs := flag.NewFlagSet("enqueue", flag.ExitOnError)
			dsn := fs.String("dsn", os.Getenv("CLEAT_DATABASE_URL"), "PostgreSQL DSN (default: $CLEAT_DATABASE_URL)")
			tenantStr := fs.String("tenant", "", "Tenant UUID")
			queueName := fs.String("queue", "", "Queue name")
			payloadStr := fs.String("payload", "{}", "JSON payload")
			if err := fs.Parse(args); err != nil {
				return err
			}

			if *tenantStr == "" || *queueName == "" {
				fmt.Fprintf(os.Stderr, "Usage: cleat jobqueue enqueue --tenant=<uuid> --queue=<name> [--payload='{}'] [--dsn=<url>]\n")
				return fmt.Errorf("tenant and queue are required")
			}

			tenantID, err := uuid.Parse(*tenantStr)
			if err != nil {
				return fmt.Errorf("invalid tenant UUID: %w", err)
			}

			// Validate that the payload is valid JSON.
			var validate json.RawMessage
			if err := json.Unmarshal([]byte(*payloadStr), &validate); err != nil {
				return fmt.Errorf("invalid JSON payload: %w", err)
			}

			if *dsn == "" {
				return fmt.Errorf("database URL required: set DATABASE_URL env var or pass --dsn")
			}

			driver := os.Getenv("CLEAT_DB_DRIVER")
			if driver == "" {
				driver = "postgres"
			}
			db, err := sql.Open(driver, *dsn)
			if err != nil {
				return fmt.Errorf("connect to database: %w", err)
			}
			defer db.Close()

			if err := db.Ping(); err != nil {
				return fmt.Errorf("ping database: %w", err)
			}

			jobID := uuid.New()

			// IN A TRANSACTION THAT SETS cleat.tenant_id, and that is the whole
			// point rather than tidiness. cleat#1512.
			//
			// task_queue became tenant-scoped in migration version 3, and its
			// policy calls cleat.assert_tenant_set(), which RAISES when the
			// setting is absent. This command does NOT go through
			// plugin.PluginDB / SQLDBAdapter -- it opens its own *sql.DB from
			// --dsn -- so engine.beginTenantTx never runs for it and nothing
			// else was ever going to set the value. Measured against a policied
			// table as a non-superuser, the bare INSERT this replaced fails:
			//
			//	ERROR: cleat.tenant_id is not set -- tenant context required
			//
			// PostgreSQL applies a FOR ALL policy's USING expression as the
			// WITH CHECK for INSERT when no WITH CHECK is given, so having the
			// tenant in the VALUES list does not help: the check runs before
			// the row exists to be checked against.
			//
			// A transaction rather than a bare SET, because *sql.DB is a pool
			// and a session-level SET may land on a different connection from
			// the INSERT. `true` is set_config's is_local flag, so the setting
			// reverts at COMMIT and cannot follow the connection back.
			tx, err := db.Begin()
			if err != nil {
				return fmt.Errorf("enqueue job: begin: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			if _, err := tx.Exec(
				`SELECT set_config('cleat.tenant_id', $1, true)`, tenantID.String()); err != nil {
				return fmt.Errorf("enqueue job: set tenant scope: %w", err)
			}

			if _, err = tx.Exec(`
				INSERT INTO task_queue (tenant_id, queue_name, job_id, payload)
				VALUES ($1, $2, $3, $4)
			`, tenantID, *queueName, jobID, []byte(*payloadStr)); err != nil {
				return fmt.Errorf("enqueue job: %w", err)
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("enqueue job: commit: %w", err)
			}

			fmt.Println(jobID.String())
			return nil
		},
	}}
}
