package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rcownie/cleat/internal/host"
)

func runDeploy(ctx context.Context, store host.WorkflowStore, db *sql.DB, args []string) {
	if len(args) < 1 {
		printDeployUsage()
		os.Exit(1)
	}

	sub := args[0]
	switch sub {
	case "workflow":
		deployWorkflow(ctx, store, db, args[1:])
	case "plugin":
		deployPlugin(ctx, db, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown deploy subcommand: %s\n\n", sub)
		printDeployUsage()
		os.Exit(1)
	}
}

func printDeployUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl deploy <subcommand> [<args>]

Subcommands:
  workflow <name> <wasm-file>  deploy a new workflow WASM binary
  plugin   <name> <wasm-file>  deploy a plugin WASM binary

`)
}

// deployWorkflow reads a WASM binary from a file and deploys it as a new
// workflow version. It computes the new version number automatically by
// incrementing the latest deployed version. If an exact version already
// exists with the same SHA256 hash, the deployment is skipped.
func deployWorkflow(ctx context.Context, store host.WorkflowStore, db *sql.DB, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl deploy workflow <name> <wasm-file>")
		os.Exit(1)
	}

	name := args[0]
	wasmPath := args[1]

	// Read WASM binary.
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", wasmPath, err)
		os.Exit(1)
	}
	if len(wasmBytes) == 0 {
		fmt.Fprintln(os.Stderr, "error: empty WASM file")
		os.Exit(1)
	}

	// Compute SHA256 hash for dedup.
	hash := sha256.Sum256(wasmBytes)

	// Determine next version number.
	var nextVersion int
	existingDefs, err := store.ListWorkflowDefs(ctx, name)
	if err != nil {
		// Assume no existing versions.
		nextVersion = 1
	} else {
		nextVersion = 1
		for _, def := range existingDefs {
			if def.Version >= nextVersion {
				nextVersion = def.Version + 1
			}
			// Check for duplicate WASM.
			if len(def.WASMBytes) > 0 {
				existingHash := sha256.Sum256(def.WASMBytes)
				if existingHash == hash {
					fmt.Printf("WASM unchanged: %s v%d already has the same binary (skipped)\n", name, def.Version)
					return
				}
			}
		}
	}

	// Default ABI version to 1 if no existing versions.
	abiVersion := 1
	minVersion := 1
	pluginDeps := map[string]string{}

	// If there's an existing latest version, use its ABI version and compute minVersion.
	if len(existingDefs) > 0 {
		latest := existingDefs[0] // ListWorkflowDefs returns ordered by version DESC.
		abiVersion = latest.ABIVersion
		// New version's MinVersion = previous version (linear migration chain).
		minVersion = latest.Version
		pluginDeps = latest.PluginDeps
		if pluginDeps == nil {
			pluginDeps = map[string]string{}
		}
	}

	def := &host.WorkflowDef{
		Name:       name,
		Version:    nextVersion,
		WASMBytes:  wasmBytes,
		ABIVersion: abiVersion,
		MinVersion: minVersion,
		PluginDeps: pluginDeps,
		CreatedAt:  time.Now(),
	}

	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		fmt.Fprintf(os.Stderr, "error deploying %s v%d: %v\n", name, nextVersion, err)
		os.Exit(1)
	}

	fmt.Printf("Deployed %s v%d (ABI v%d, minVersion=%d, %d bytes, SHA256=%x)\n",
		name, nextVersion, abiVersion, minVersion, len(wasmBytes), hash[:8])
}

// deployPlugin reads a plugin WASM binary and registers it in the
// plugin_registry table. This is a simple database insert.
func deployPlugin(ctx context.Context, db *sql.DB, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl deploy plugin <name> <wasm-file>")
		os.Exit(1)
	}

	name := args[0]
	wasmPath := args[1]

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", wasmPath, err)
		os.Exit(1)
	}

	hash := sha256.Sum256(wasmBytes)

	// Check if a plugin with this name already exists.
	var existingID string
	err = db.QueryRowContext(ctx, `SELECT id FROM plugin_registry WHERE name = $1`, name).Scan(&existingID)
	if err == nil {
		// Update existing plugin.
		_, err = db.ExecContext(ctx, `UPDATE plugin_registry SET wasm_bytes = $1, updated_at = now() WHERE name = $2`, wasmBytes, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error updating plugin %s: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Printf("Updated plugin %s (%d bytes, SHA256=%x)\n", name, len(wasmBytes), hash[:8])
		return
	}

	// Insert new plugin.
	meta := json.RawMessage(`{"deployed_by": "cleatctl"}`)
	_, err = db.ExecContext(ctx, `
		INSERT INTO plugin_registry (name, wasm_bytes, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, now(), now())
	`, name, wasmBytes, meta)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error deploying plugin %s: %v\n", name, err)
		os.Exit(1)
	}

	fmt.Printf("Deployed plugin %s (%d bytes, SHA256=%x)\n", name, len(wasmBytes), hash[:8])
}
