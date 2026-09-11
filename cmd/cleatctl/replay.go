package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
)

// runReplay replays a workflow's event history to diagnose issues.
//
// Usage: cleatctl [--db <dsn>] replay <workflow-id> --entry-point <name> [--verbose]
//
// Reads the workflow instance, its WASM binary, and its event history, then
// replays through the engine and reports results. This is a diagnostic tool
// for debugging stuck or unexpectedly-failed workflows.
func runReplay(ctx context.Context, store engine.WorkflowStore, db *sql.DB, args []string) {
	flags := parseReplayFlags(args)
	if flags == nil {
		return // usage already printed
	}

	workflowID := flags.workflowID
	entryPoint := flags.entryPoint
	verbose := flags.verbose

	// Load the workflow instance from the database.
	inst, err := loadWorkflowInstance(ctx, db, workflowID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading workflow instance %q: %v\n", workflowID, err)
		osExit(1)
	}

	fmt.Printf("Workflow: %s\n", workflowID)
	fmt.Printf("  Definition: %s (v%d)\n", inst.DefName, inst.DefVersion)
	fmt.Printf("  Status: %s\n", inst.Status)
	fmt.Printf("  Entry point: %s\n", entryPoint)

	// Load event history.
	events, err := store.LoadEventHistory(ctx, workflowID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading event history for %q: %v\n", workflowID, err)
		osExit(1)
	}
	fmt.Printf("  Events: %d\n", len(events))

	// Load WASM binary.
	wasmBytes, err := store.LoadWASM(ctx, inst.DefName, inst.DefVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading WASM for %s v%d: %v\n", inst.DefName, inst.DefVersion, err)
		osExit(1)
	}
	fmt.Printf("  WASM size: %d bytes\n", len(wasmBytes))

	// Build a minimal Engine for replay with a stub caller that returns
	// an error if replay diverges into fresh execution.
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating runtime: %v\n", err)
		osExit(1)
	}

	engine := engine.NewEngine(rt, &replayStubCaller{},
		engine.WithDefName(inst.DefName),
		engine.WithDefVersion(inst.DefVersion),
	)

	if verbose {
		fmt.Println()
		fmt.Println("Event history:")
		for _, ev := range events {
			fmt.Printf("  step=%d type=%s", ev.Step, ev.EventType)
			if ev.Service != "" {
				fmt.Printf(" service=%s op=%s", ev.Service, ev.Op)
			}
			if ev.Request != "" {
				fmt.Printf(" request=%s", truncate(ev.Request, 60))
			}
			if ev.Response != "" {
				fmt.Printf(" response=%s", truncate(ev.Response, 60))
			}
			if ev.Err != "" {
				fmt.Printf(" err=%s", truncate(ev.Err, 60))
			}
			fmt.Println()
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Println("Replaying...")

	result, resultHistory, suspended, deferrals, queryState, err := engine.Replay(
		ctx, wasmBytes, entryPoint, inst.Input, events,
	)

	if err != nil {
		fmt.Fprintf(os.Stderr, "replay error: %v\n  Check that --entry-point matches an export in the deployed WASM and that the workflow event history is not corrupted.\n", err)
		osExit(1)
	}

	fmt.Println()
	fmt.Println("Replay results:")
	fmt.Printf("  Result: %s\n", truncate(result, 200))
	if suspended != nil {
		fmt.Printf("  Suspended: reason=%s suspend_until=%v\n", suspended.Reason, suspended.SuspendUntil)
	}
	fmt.Printf("  Deferrals: %d\n", len(deferrals))
	fmt.Printf("  Query state: %d keys\n", len(queryState))
	for k, v := range queryState {
		fmt.Printf("    %s = %s\n", k, truncate(v, 80))
	}
	fmt.Printf("  Replayed events: %d\n", len(resultHistory))
	if verbose {
		for i, ev := range resultHistory {
			fmt.Printf("    [%d] step=%d type=%s", i, ev.Step, ev.EventType)
			if ev.Response != "" {
				fmt.Printf(" response=%s", truncate(ev.Response, 60))
			}
			if ev.Err != "" {
				fmt.Printf(" err=%s", truncate(ev.Err, 60))
			}
			fmt.Println()
		}
	}
}

// replayFlags holds parsed command-line flags for the replay subcommand.
type replayFlags struct {
	workflowID string
	entryPoint string
	verbose    bool
}

// parseReplayFlags parses flags from the args slice. Returns nil if flag
// parsing failed (usage was printed).
func parseReplayFlags(args []string) *replayFlags {
	f := &replayFlags{}

	// Collect positional and flag arguments.
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--entry-point":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --entry-point requires a value")
				return nil
			}
			f.entryPoint = args[i]
		case "--verbose", "-v":
			f.verbose = true
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) < 1 {
		printReplayUsage()
		return nil
	}
	f.workflowID = positional[0]

	if f.entryPoint == "" {
		fmt.Fprintln(os.Stderr, "error: --entry-point is required (the WASM export name to invoke)")
		fmt.Fprintln(os.Stderr)
		printReplayUsage()
		return nil
	}

	return f
}

// printReplayUsage prints the help text for the replay command.
func printReplayUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl [--db <dsn>] replay <workflow-id> --entry-point <name> [--verbose]

Replay a workflow's event history for diagnostic purposes.

Arguments:
  <workflow-id>       The workflow instance ID to replay.

Flags:
  --entry-point <name>  WASM export name to invoke (required).
  --verbose, -v         Print detailed event history and replay output.

This command reads the workflow's event history and WASM binary from the
database, replays through the engine, and reports the result. It does NOT
modify the database (read-only).

Examples:
  cleatctl --db "postgres://..." replay abc-123 --entry-point HandleLead
  cleatctl --db "postgres://..." replay abc-123 --entry-point HandleLead -v

`)
}

// loadWorkflowInstance loads a single workflow instance by ID from the database.
func loadWorkflowInstance(ctx context.Context, db *sql.DB, id string) (*engine.WorkflowInstance, error) {
	// FOUR things were wrong with this statement, and it had never run.
	// cleat#1208.
	//
	//   min_version              belongs to workflow_defs, not here
	//   error                    the column is error_msg
	//   COALESCE(result, '')     result is jsonb; '' is not valid json
	//   COALESCE(tenant_id, '')  tenant_id is uuid NOT NULL
	//   assigned_to              nullable, scanned into a plain string
	//
	// Only the first two were in the bug report. The next two are downstream of
	// them -- PostgreSQL stops at the first unresolved name, so nobody could see
	// them until it was fixed, and TestEveryInlineStatementParsesOnPostgres
	// reported both on its first run against the half-fixed statement.
	//
	// THE FIFTH IS THE INTERESTING ONE, because that test cannot find it and
	// never will. PREPARE parses and plans; it says nothing about the Go value
	// a column is scanned into, so a nullable column read into a non-pointer
	// string parses perfectly and fails at run time with "converting NULL to
	// string is unsupported". It took actually running the command against a
	// row. A guard that checks the SQL is not a guard that checks the read, and
	// it is worth knowing which one you have.
	//
	// MinVersion is dropped rather than sourced from somewhere else: it is a
	// property of a workflow DEFINITION, nothing downstream of this function
	// reads it, and joining workflow_defs to populate a field neither command
	// prints would be inventing a requirement to justify a column name.
	row := db.QueryRowContext(ctx, `
		SELECT id, def_name, def_version, status, input,
		       COALESCE(result::text, ''), COALESCE(error_msg, ''),
		       COALESCE(error_code, ''), COALESCE(error_op, ''),
		       COALESCE(assigned_to, ''), next_wake_at, tenant_id::text,
		       created_at, generation
		FROM workflow_instances
		WHERE id = $1
	`, id)

	var inst engine.WorkflowInstance
	err := row.Scan(
		&inst.ID, &inst.DefName, &inst.DefVersion,
		&inst.Status, &inst.Input, &inst.Result, &inst.Error,
		&inst.ErrorCode, &inst.ErrorOp, &inst.AssignedTo,
		&inst.NextWakeAt, &inst.TenantID, &inst.CreatedAt, &inst.Generation,
	)
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

// replayStubCaller is a stub ServiceCaller that returns an error, used during
// diagnostic replay when the replay diverges into fresh execution.
type replayStubCaller struct{}

func (r *replayStubCaller) Call(ctx context.Context, service, operation, request string) (string, error) {
	return "", fmt.Errorf("replay stub: cannot make live call to %s.%s during diagnostic replay", service, operation)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
