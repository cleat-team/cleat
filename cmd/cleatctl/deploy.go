package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"time"

	"golang.org/x/mod/semver"

	"github.com/cleat-team/cleat/engine"
)

func runDeploy(ctx context.Context, store engine.WorkflowStore, db *sql.DB, args []string) {
	if len(args) < 1 {
		printDeployUsage()
		osExit(1)
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
		osExit(1)
	}
}

func printDeployUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl deploy <subcommand> [<args>]

Subcommands:
  workflow <name> <wasm-file>  deploy a new workflow WASM binary
  plugin   <name> <version> <wasm-file>
                               deploy a plugin WASM binary at a semver version

`)
}

// deployWorkflow reads a WASM binary from a file and deploys it as a new
// workflow version. It computes the new version number automatically by
// incrementing the latest deployed version. If an exact version already
// exists with the same SHA256 hash, the deployment is skipped.
func deployWorkflow(ctx context.Context, store engine.WorkflowStore, db *sql.DB, args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl deploy workflow <name> <wasm-file>")
		osExit(1)
	}

	name := args[0]
	wasmPath := args[1]

	// Read WASM binary.
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", wasmPath, err)
		osExit(1)
	}
	if len(wasmBytes) == 0 {
		fmt.Fprintln(os.Stderr, "error: empty WASM file")
		osExit(1)
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

	def := &engine.WorkflowDef{
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
		osExit(1)
	}

	fmt.Printf("Deployed %s v%d (ABI v%d, minVersion=%d, %d bytes, SHA256=%x)\n",
		name, nextVersion, abiVersion, minVersion, len(wasmBytes), hash[:8])
}

// deployPlugin reads a plugin WASM binary and writes it to plugin_defs.
//
// IT USED TO WRITE TO plugin_registry, A TABLE NO MIGRATION HAS EVER CREATED
// (cleat#1216, cleat#1226), so the command could not work at all -- three
// statements against a name that resolves to nothing. It is not a rename:
// plugin_defs is keyed (name, version) and has config rather than metadata and
// no id and no updated_at, so every assumption the old code made about the
// shape was wrong too.
//
// THE VERSION IS REQUIRED, and that is a decision rather than an omission.
// plugin_defs.version is TEXT and NOT NULL, and engine.PluginLoader.Resolve-
// Plugin parses it as semver and SILENTLY SKIPS a row it cannot parse:
//
//	v := ensureVPrefix(p.Version)
//	if !semver.IsValid(v) { continue }
//
// so any invented default risks writing a plugin that is present in the table,
// listed by `cleat plugin list`, and resolvable by nothing. A content hash is
// invalid semver outright; "1" is valid but compares EQUAL to "1.0.0" while
// being a different primary key, which leaves two rows the resolver cannot
// tell apart; an auto-incrementing integer breaks `ORDER BY version DESC` in
// cmd/cleat/plugin_cmd.go, which is a text sort where "9" sorts above "10".
//
// `cleat plugin install` already writes registry semver into this column and
// `cleat plugin uninstall <name> <version>` already takes this argument shape,
// so requiring it is the only option that does not add a second convention.
func deployPlugin(ctx context.Context, db *sql.DB, args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl deploy plugin <name> <version> <wasm-file>")
		fmt.Fprintln(os.Stderr, "  <version> is a semver string, e.g. 1.0.0 -- plugin_defs is keyed (name, version)")
		fmt.Fprintln(os.Stderr, "  and the resolver compares versions as semver.")
		osExit(1)
	}

	name := args[0]
	version := args[1]
	wasmPath := args[2]

	// Refused here rather than accepted and skipped at resolution time. A
	// version the resolver cannot parse produces a deploy that reports success
	// and a plugin nothing can load -- the failure this command already had,
	// moved one step downstream.
	if !semver.IsValid(ensureSemverVPrefix(version)) {
		fmt.Fprintf(os.Stderr, "invalid plugin version %q: must be a semver string such as 1.0.0.\n", version)
		fmt.Fprintln(os.Stderr, "  The resolver compares plugin versions as semver and ignores rows it cannot")
		fmt.Fprintln(os.Stderr, "  parse, so a plugin deployed with this version would be silently unusable.")
		osExit(1)
	}

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", wasmPath, err)
		osExit(1)
	}

	hash := sha256.Sum256(wasmBytes)

	// DeployPlugin upserts on (name, version): redeploying a version replaces
	// its bytes and clears `deprecated`, and a new version adds a row rather
	// than overwriting the old one. The command this replaces had one row per
	// NAME, which is the model plugin_defs deliberately does not use.
	if err := engine.NewPluginLoader(db, nil).DeployPlugin(ctx, name, version, wasmBytes, nil); err != nil {
		fmt.Fprintf(os.Stderr, "error deploying plugin %s v%s: %v\n", name, version, err)
		osExit(1)
	}

	fmt.Printf("Deployed plugin %s v%s (%d bytes, SHA256=%x)\n", name, version, len(wasmBytes), hash[:8])
}

// ensureSemverVPrefix mirrors engine's ensureVPrefix, which is unexported.
// The two must agree: this decides what is accepted, and that one decides what
// is resolvable, so a divergence would reintroduce exactly the gap above.
func ensureSemverVPrefix(v string) string {
	if len(v) > 0 && v[0] == 'v' {
		return v
	}
	return "v" + v
}
