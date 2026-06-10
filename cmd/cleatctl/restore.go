package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// restore-workflow command
// ---------------------------------------------------------------------------

// runRestoreWorkflow restores a single workflow instance from a backup file.
// The backup file is expected to be a newline-delimited JSON (NDJSON) file
// where each line is a JSON object with a "table" field and a "row" field.
//
// Supported tables:
//   - workflow_instances   (1 row per workflow)
//   - event_history        (N rows per workflow)
//   - workflow_signals     (N rows per workflow)
//   - workflow_promises    (N rows per workflow)
//   - child_workflows      (N rows per workflow)
//
// Limitations:
//   - FK constraints require restoring related rows (child workflows, signals,
//     promises) together with the parent. The tool restores all rows that
//     reference the given workflow ID.
//   - ON CONFLICT DO NOTHING is used, so restoring a workflow that already
//     exists is safe (no-op).
//   - The backup must be in NDJSON format produced by cleatctl backup-workflow
//     or a compatible tool.
//   - Tenants, schedules, and workflow_defs are NOT restored by this command.
func runRestoreWorkflow(ctx context.Context, store engine.WorkflowStore, db *sql.DB, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl restore-workflow <workflow-id> <backup-file>")
		osExit(1)
	}

	workflowID := args[0]
	backupPath := args[1]

	// Open and validate the backup file.
	f, err := os.Open(backupPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening backup file: %v\n", err)
		osExit(1)
	}
	defer f.Close()

	// Read the backup file line by line, collecting rows that match the
	// workflow ID. We use a two-pass approach:
	//   1. First pass: find the workflow_instance row and collect all related rows.
	//   2. Second pass: insert all collected rows with ON CONFLICT DO NOTHING.
	scanner := bufio.NewScanner(f)
	// Increase scanner buffer for potentially large rows (e.g., WASM bytes).
	scanner.Buffer(make([]byte, 0, 10*1024*1024), 10*1024*1024)

	type backupRow struct {
		Table string          `json:"table"`
		Row   json.RawMessage `json:"row"`
	}

	var instanceRows []backupRow
	var eventRows []backupRow
	var signalRows []backupRow
	var promiseRows []backupRow
	var childRows []backupRow

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		var br backupRow
		if err := json.Unmarshal([]byte(line), &br); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing backup line %d: %v\n", lineNo, err)
			osExit(1)
		}

		// Check if this row references our workflow ID.
		var rowMap map[string]any
		if err := json.Unmarshal(br.Row, &rowMap); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing row on line %d: %v\n", lineNo, err)
			osExit(1)
		}

		// For workflow_instances, match on the ID field.
		if br.Table == "workflow_instances" {
			if id, ok := rowMap["id"].(string); ok && id == workflowID {
				instanceRows = append(instanceRows, br)
				continue
			}
			// Also check for child workflow rows that have this workflow as parent.
			// Insert them too so FK constraints are satisfied.
			if parentID, ok := rowMap["parent_workflow_id"].(string); ok && parentID == workflowID {
				instanceRows = append(instanceRows, br)
			}
			continue
		}

		// For other tables, match on workflow_id field.
		if wid, ok := rowMap["workflow_id"].(string); ok && wid == workflowID {
			switch br.Table {
			case "event_history":
				eventRows = append(eventRows, br)
			case "workflow_signals":
				signalRows = append(signalRows, br)
			case "workflow_promises":
				promiseRows = append(promiseRows, br)
			case "child_workflows":
				childRows = append(childRows, br)
			default:
				fmt.Fprintf(os.Stderr, "warning: unknown table %q on line %d (skipped)\n", br.Table, lineNo)
			}
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error reading backup file: %v\n", err)
		osExit(1)
	}

	if len(instanceRows) == 0 {
		fmt.Fprintf(os.Stderr, "error: workflow %q not found in backup file\n", workflowID)
		osExit(1)
	}

	// ---- Insert phase ----
	// Use ON CONFLICT DO NOTHING for idempotent restore.
	fmt.Printf("Restoring workflow %s:\n", workflowID)
	fmt.Printf("  - %d workflow instance rows\n", len(instanceRows))
	fmt.Printf("  - %d event_history rows\n", len(eventRows))
	fmt.Printf("  - %d workflow_signals rows\n", len(signalRows))
	fmt.Printf("  - %d workflow_promises rows\n", len(promiseRows))
	fmt.Printf("  - %d child workflow rows (if applicable)\n", len(childRows))

	// Insert workflow instances first (parent table for FK constraints).
	for _, br := range instanceRows {
		if err := insertWorkflowInstance(ctx, db, br.Row); err != nil {
			fmt.Fprintf(os.Stderr, "error restoring workflow instance: %v\n", err)
			osExit(1)
		}
	}

	// Insert event_history rows.
	for _, br := range eventRows {
		if err := insertEventHistory(ctx, db, br.Row); err != nil {
			fmt.Fprintf(os.Stderr, "error restoring event_history row: %v\n", err)
			osExit(1)
		}
	}

	// Insert workflow_signals rows.
	for _, br := range signalRows {
		if err := insertWorkflowSignal(ctx, db, br.Row); err != nil {
			fmt.Fprintf(os.Stderr, "error restoring workflow_signal: %v\n", err)
			osExit(1)
		}
	}

	// Insert workflow_promises rows.
	for _, br := range promiseRows {
		if err := insertWorkflowPromise(ctx, db, br.Row); err != nil {
			fmt.Fprintf(os.Stderr, "error restoring workflow_promise: %v\n", err)
			osExit(1)
		}
	}

	// Insert child_workflows rows.
	for _, br := range childRows {
		if err := insertChildWorkflow(ctx, db, br.Row); err != nil {
			fmt.Fprintf(os.Stderr, "error restoring child_workflow: %v\n", err)
			osExit(1)
		}
	}

	fmt.Println("Restore complete.")
}

// ---------------------------------------------------------------------------
// Table-specific inserters (PostgreSQL, ON CONFLICT DO NOTHING)
// ---------------------------------------------------------------------------

func insertWorkflowInstance(ctx context.Context, db *sql.DB, rowJSON []byte) error {
	var row struct {
		ID        string `json:"id"`
		DefName   string `json:"def_name"`
		Status    string `json:"status"`
		Input     string `json:"input"`
		Result    string `json:"result,omitempty"`
		Error     string `json:"error,omitempty"`
		CreatedAt string `json:"created_at,omitempty"`
	}
	if err := json.Unmarshal(rowJSON, &row); err != nil {
		return fmt.Errorf("parse workflow_instance: %w", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, status, input, result, error, created_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6,
			CASE WHEN $7::TEXT != '' THEN $7::TIMESTAMPTZ ELSE NOW() END)
		ON CONFLICT (id) DO NOTHING
	`, row.ID, row.DefName, row.Status, nullIfEmpty(row.Input), nullIfEmpty(row.Result),
		nullIfEmpty(row.Error), row.CreatedAt)
	return err
}

func insertEventHistory(ctx context.Context, db *sql.DB, rowJSON []byte) error {
	var row struct {
		WorkflowID string `json:"workflow_id"`
		Step       int    `json:"step"`
		EventType  string `json:"event_type"`
		Service    string `json:"service,omitempty"`
		Operation  string `json:"operation,omitempty"`
		Request    string `json:"request,omitempty"`
		Response   string `json:"response,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(rowJSON, &row); err != nil {
		return fmt.Errorf("parse event_history: %w", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, request, response, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workflow_id, step) DO NOTHING
	`, row.WorkflowID, row.Step, row.EventType, nullIfEmpty(row.Service),
		nullIfEmpty(row.Operation), nullIfEmpty(row.Request),
		nullIfEmpty(row.Response), nullIfEmpty(row.Error))
	return err
}

func insertWorkflowSignal(ctx context.Context, db *sql.DB, rowJSON []byte) error {
	var row struct {
		WorkflowID    string `json:"workflow_id"`
		SignalName    string `json:"signal_name"`
		Payload       string `json:"payload,omitempty"`
		CorrelationID string `json:"correlation_id,omitempty"`
	}
	if err := json.Unmarshal(rowJSON, &row); err != nil {
		return fmt.Errorf("parse workflow_signal: %w", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_signals (workflow_id, signal_name, payload, correlation_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
	`, row.WorkflowID, row.SignalName, nullIfEmpty(row.Payload),
		nullIfEmpty(row.CorrelationID))
	return err
}

func insertWorkflowPromise(ctx context.Context, db *sql.DB, rowJSON []byte) error {
	var row struct {
		WorkflowID string `json:"workflow_id"`
		PromiseID  string `json:"promise_id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Result     string `json:"result,omitempty"`
	}
	if err := json.Unmarshal(rowJSON, &row); err != nil {
		return fmt.Errorf("parse workflow_promise: %w", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO workflow_promises (workflow_id, promise_id, name, status, result)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
	`, row.WorkflowID, row.PromiseID, row.Name, row.Status, nullIfEmpty(row.Result))
	return err
}

func insertChildWorkflow(ctx context.Context, db *sql.DB, rowJSON []byte) error {
	var row struct {
		WorkflowID string `json:"workflow_id"`
		ChildID    string `json:"child_id"`
		DefName    string `json:"def_name"`
		Input      string `json:"input,omitempty"`
	}
	if err := json.Unmarshal(rowJSON, &row); err != nil {
		return fmt.Errorf("parse child_workflow: %w", err)
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO child_workflows (workflow_id, child_id, def_name, input)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
	`, row.WorkflowID, row.ChildID, row.DefName, nullIfEmpty(row.Input))
	return err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// printRestoreUsage prints the help text for restore-workflow.
func printRestoreUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl restore-workflow <workflow-id> <backup-file>

Restore a single workflow instance from an NDJSON backup file.

The backup file should be in newline-delimited JSON format where each line
contains a JSON object with "table" and "row" fields. This format is produced
by cleatctl backup-workflow or compatible tools.

Supported tables:
  workflow_instances    (1 row per workflow, required)
  event_history         (N rows per workflow)
  workflow_signals      (N rows per workflow)
  workflow_promises     (N rows per workflow)
  child_workflows       (N rows per workflow)

Limitations:
  - FK constraints require restoring child workflows, signals, and promises
    together with the parent. The tool automatically restores all rows that
    reference the given workflow ID.
  - ON CONFLICT DO NOTHING is used throughout, so re-running restore on an
    already-restored workflow is safe (it becomes a no-op).
  - Workflow_defs and schedules are NOT restored by this command.
  - The backup file format is NDJSON; ensure the file is well-formed.

Environment:
  CLEAT_DB_URL   PostgreSQL DSN (alternative to --db)

`)
}
