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
			dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL DSN (default: $DATABASE_URL)")
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
			_, err = db.Exec(`
				INSERT INTO task_queue (tenant_id, queue_name, job_id, payload)
				VALUES ($1, $2, $3, $4)
			`, tenantID, *queueName, jobID, []byte(*payloadStr))
			if err != nil {
				return fmt.Errorf("enqueue job: %w", err)
			}

			fmt.Println(jobID.String())
			return nil
		},
	}}
}
