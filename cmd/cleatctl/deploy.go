package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/mod/semver"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
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
// incrementing the latest deployed version. If the LATEST version already
// holds the same binary, the deployment is skipped and nothing is written.
//
// Until cleat#2947 that skip had never fired against a real store: it compared
// against WorkflowDef.WASMBytes, and no dialect's ListWorkflowDefs selects
// wasm_bytes, so the field was always empty. It reads through LoadWASM now,
// against the latest version only. The check carries its own reasons -- both
// for reading at all and for reading one version rather than all of them.
func deployWorkflow(ctx context.Context, store engine.WorkflowStore, db *sql.DB, args []string) {
	fs := flag.NewFlagSet("deploy workflow", flag.ContinueOnError)
	// cleat#1981: the one escape hatch validate-input-at-start asks for, for
	// a schema that turns out to be wrong in production. Per-definition only
	// -- there is deliberately no per-request equivalent, so a caller cannot
	// switch validation off for its own requests.
	noValidateInput := fs.Bool("no-validate-input", false, "disable cleat#1981 start-input validation for this version, even if it carries a schema")
	// cleat#1986: this version's exposure class. A source-level declaration is a
	// later slice, so the deploy path is the only way to set it today -- and with
	// nothing declaring a class yet there is nothing to tighten against, so this
	// simply sets it.
	// cleat#1986 slice 2c-ii. The default is EMPTY, not `auth`, and that is the
	// whole reason this slice works: an omitted flag has to mean "no manifest
	// opinion" so the artifact's own declaration can stand. It used to default
	// to `auth`, which is indistinguishable from `--exposure auth` -- so on an
	// artifact declaring `internal`, a plain `cleatctl deploy workflow X x.wasm`
	// read as a request to LOOSEN and was refused (or, before this rule, was
	// silently obeyed).
	exposureFlag := fs.String("exposure", "",
		"exposure class for this version: auth, internal, or public (not available until the per-tenant opt-in exists). "+
			"Omitted means no opinion: the class the artifact declares in its own source, or auth if it declares none. "+
			"May tighten that class but never loosen it (cleat#1986)")
	positional, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(1)
	}

	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl deploy workflow <name> <wasm-file> [--no-validate-input] [--exposure auth|internal]")
		osExit(1)
	}

	// The REQUESTED class, validated here so a bad flag is named as a bad flag.
	// "" means the caller expressed no opinion and the artifact's declaration
	// decides; that is not a class, so it is not run through ParseExposure.
	requested := engine.ExposureClass("")
	if *exposureFlag != "" {
		var ok bool
		requested, ok = engine.ParseExposure(*exposureFlag)
		if !ok {
			fmt.Fprintf(os.Stderr, "error: --exposure must be one of %q, %q or %q, got %q\n",
				engine.ExposureAuth, engine.ExposurePublic, engine.ExposureInternal, *exposureFlag)
			osExit(1)
		}
	}

	name := positional[0]
	wasmPath := positional[1]

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

	// cleat#1986 slice 2c-ii: the artifact's SOURCE declaration, then the
	// tighten-only rule.
	//
	// A metadata read that FAILS is treated as no declaration, deliberately
	// rather than laxly: wasm.ReadMetadata rejects a Component Model binary
	// outright (see the note on restampWorkflowVersion below), so a Python
	// artifact takes this path -- and the Python build writes no exposure section
	// at all today, which is the separate slice cleat#1986 still tracks. But a
	// read that SUCCEEDS and carries an unrecognised class is a different thing,
	// and ResolveExposure refuses that one: the value came out of the file, and
	// treating a malformed stamp as an absence would deploy as `auth` exactly
	// what the source meant to protect.
	declared := engine.ExposureClass("")
	if meta, metaErr := wasm.ReadMetadata(wasmBytes); metaErr == nil {
		declared = engine.ExposureClass(meta.Exposure)
	}

	// ResolveDeployableExposure is the tighten-only rule AND the `public` gate,
	// in one place so the three deploy paths cannot drift apart about which
	// classes may be stored. See its doc comment; the gate is deliberately
	// checked against the RESOLVED class, so an artifact whose source declares
	// `public` is caught here as well as a flag that asks for it.
	exposure, err := engine.ResolveDeployableExposure(declared, requested)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
	}

	// SHA256 of the artifact exactly as it sits on disk. Used only for the
	// success line at the end. The dedup check further down does NOT use this
	// value: it hashes both sides after normalising cleat.metadata's
	// workflow_version out, because the restamp assigns a version the file on
	// disk does not carry.
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
		}
	}

	// Default ABI version to 1 if no existing versions.
	abiVersion := 1
	minVersion := 1
	pluginDeps := map[string]string{}

	// If there's an existing latest version, use its ABI version and compute minVersion.
	if len(existingDefs) > 0 {
		latest := existingDefs[0] // ListWorkflowDefs returns ordered by version DESC.

		// cleat#2947. Skip when the binary about to be deployed is the one the
		// LATEST version already holds.
		//
		// Read through LoadWASM rather than WorkflowDef.WASMBytes, which is why
		// the comparison that used to sit in the loop above never once fired: no
		// dialect's ListWorkflowDefs selects wasm_bytes
		// (engine/store_deployment.go:288, engine/mysql_ops.go:905/911,
		// engine/mssql_deployment.go:387/392), so that field was always empty.
		//
		// THE LATEST VERSION ONLY, and it is a decision rather than a shortcut.
		// Checking every listed def costs one LoadWASM per version, and LoadWASM
		// returns the whole binary -- ~19MB for a Python component -- so a
		// genuinely new artifact, which matches nothing and therefore reads every
		// version, would pay N x 19MB on every deploy, growing without bound as
		// the workflow accumulates versions. One read covers the case this guard
		// exists for: re-deploying the binary you just built.
		//
		// It is also the CORRECT semantic rather than merely the cheap one. The
		// chain is linear (MinVersion = latest.Version), so re-deploying an
		// artifact matching an OLDER version is a deliberate move forward -- a
		// rollback expressed as a new version. A guard that skipped on any
		// version's match would refuse that silently and leave the previous
		// binary current, which is the failure class this repo files as "an
		// operation that reports success without doing the thing".
		//
		// Both sides are normalised because restampWorkflowVersion (below) writes
		// the version this command ASSIGNS into the stored binary, while the file
		// on disk still carries whatever `cleat build --version` wrote. Compared
		// as stored, an identical artifact could never match its own copy.
		if storedBytes, loadErr := store.LoadWASM(ctx, name, latest.Version); loadErr == nil {
			incoming := sha256.Sum256(normaliseWorkflowVersion(wasmBytes))
			stored := sha256.Sum256(normaliseWorkflowVersion(storedBytes))
			if stored == incoming {
				fmt.Printf("WASM unchanged: %s v%d already has the same binary (skipped)\n", name, latest.Version)
				return
			}
		} else {
			// Not fatal -- the deploy proceeds and creates a new version, which
			// is exactly what it did before this guard worked at all. But NOT
			// silent: a guard that fails without saying so is cleat#2947's own
			// subject, one layer down.
			fmt.Fprintf(os.Stderr, "warning: could not read %s v%d to check for an unchanged binary, deploying a new version: %v\n",
				name, latest.Version, loadErr)
		}

		abiVersion = latest.ABIVersion
		// New version's MinVersion = previous version (linear migration chain).
		minVersion = latest.Version
		pluginDeps = latest.PluginDeps
		if pluginDeps == nil {
			pluginDeps = map[string]string{}
		}
	}

	// cleat#1980: `cleat build` writes a schema sidecar next to the WASM
	// binary when it computed one -- wasm.Metadata itself is deliberately
	// barred from carrying this (see wasm/metadata_carries_no_entry_point_parameters_test.go),
	// so there is nowhere inside wasmBytes to read it back from. Its absence
	// is not an error: an older build, or a build from a language
	// internal/jsonschema has no emitter for yet, simply has none, and this
	// deploy proceeds exactly as it did before cleat#1980.
	//
	// UNLIKE pluginDeps above, this is never carried forward from the
	// previous version on a redeploy that doesn't supply one: a schema
	// describes THIS wasm file's actual entry points, and copying the prior
	// version's would describe a binary that is no longer what's being
	// deployed.
	var entryPointSchemas map[string]engine.EntryPointSchema
	if schemaBytes, err := os.ReadFile(wasmPath + ".schema.json"); err == nil {
		if err := json.Unmarshal(schemaBytes, &entryPointSchemas); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s.schema.json is not valid JSON, deploying without entry point schemas: %v\n", wasmPath, err)
			entryPointSchemas = nil
		}
	}

	// cleat#2944. The stamp inside the binary and the version this row records
	// must agree: cmd/cleat-worker's pre-flight releases a run whose binary
	// reports a different workflow_version from the workflow_defs.version it was
	// queued against, on every claim, so the run loops between claim and release
	// and never executes.
	//
	// This command is documented as "deploys a new version"
	// (docs/explanation/workflow-versioning.md:251) and assigns MAX(version)+1
	// above. Before this it stored the binary unchanged, still carrying whatever
	// `cleat build --version` wrote (1 by default) -- so every redeploy of a
	// rebuilt artifact produced a row that the only available binary could not
	// serve. Restamping rather than adopting the stamp: the version recorded
	// here is the one this command assigns.
	wasmBytes, err = restampWorkflowVersion(wasmBytes, nextVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error deploying %s v%d: %v\n", name, nextVersion, err)
		osExit(1)
	}

	def := &engine.WorkflowDef{
		Name:                    name,
		Version:                 nextVersion,
		WASMBytes:               wasmBytes,
		ABIVersion:              abiVersion,
		MinVersion:              minVersion,
		PluginDeps:              pluginDeps,
		EntryPointSchemas:       entryPointSchemas,
		InputValidationDisabled: *noValidateInput,
		Exposure:                exposure,
		CreatedAt:               time.Now(),
	}

	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		fmt.Fprintf(os.Stderr, "error deploying %s v%d: %v\n", name, nextVersion, err)
		osExit(1)
	}

	fmt.Printf("Deployed %s v%d (ABI v%d, minVersion=%d, %d bytes, SHA256=%x)\n",
		name, nextVersion, abiVersion, minVersion, len(wasmBytes), hash[:8])
}

// restampWorkflowVersion returns wasmBytes with cleat.metadata's
// workflow_version set to version, so the binary and the workflow_defs row it
// is stored in agree (cleat#2944).
//
// A binary carrying no readable cleat.metadata is returned unchanged and without
// error: there is no stamp to disagree with, and cmd/cleat-worker's pre-flight
// reads that same metadata, so it cannot fire either. That is also what keeps
// this correct on a tree without cleat#2941 -- wasm.ReadMetadata rejects a
// Component Model binary there, so a Python artifact is stored exactly as it is
// today, and the version disagreement is unreachable for the same reason.
//
// A write failure IS an error, not a warning: continuing would store a binary
// whose stamp disagrees with its row, which is the defect this exists to close.
func restampWorkflowVersion(wasmBytes []byte, version int) ([]byte, error) {
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil || meta == nil {
		return wasmBytes, nil
	}
	if meta.WorkflowVersion == version {
		return wasmBytes, nil
	}
	// SetMetadataField rather than a Metadata round-trip. The struct models the
	// keys the engine reads, and a build writes others it does not --
	// stamp_metadata.py writes sdk_language, sdk_version and created_at, and
	// Rust/Java/AssemblyScript inject sdk_version -- so rebuilding the payload
	// from the struct would rewrite the whole section and drop them. This
	// changes one key and leaves every other as the build wrote it. (Found by
	// cleat-review on this PR's first head, where the round-trip silently lost
	// all four.)
	out, err := wasm.SetMetadataField(wasmBytes, "workflow_version", json.RawMessage(strconv.Itoa(version)))
	if errors.Is(err, wasm.ErrNotAJSONObject) {
		// The one shape that is left alone: valid JSON with no keys to patch, so
		// there is nothing to restamp. Stored as built, which is what develop did.
		//
		// A stamp of 0 is deliberately NOT skipped, and an earlier version of this
		// function that skipped non-positive stamps was wrong. 0 is what every
		// standalone stamper defaults to when given no version -- Java's
		// inject-metadata.sh (`:-0`), Rust's inject_metadata.rs, AssemblyScript's
		// inject-metadata.js, and Python without CLEAT_WORKFLOW_VERSION -- and
		// cmd/cleat-worker's pre-flight compares with `!=`, exempting nothing, so
		// such a binary is released on every claim and its runs never execute.
		// Restamping a 0 IS the fix cleat#2944 exists for.
		//
		// The `cleat deploy` analogy that produced the bug is worth stating so it
		// is not reapplied: there the question is "should this stamp be the
		// RECORDED version?", and declining 0 is right because a version of 0
		// cannot be recorded. Here the question is "does the stored binary agree
		// with the row?", and 0 is the case that most needs changing.
		return wasmBytes, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not stamp the binary with workflow_version %d: %w", version, err)
	}
	return out, nil
}

// normaliseWorkflowVersion returns wasmBytes with cleat.metadata's
// workflow_version set to a fixed value, so two binaries differing only in that
// stamp hash the same. cleat#2947.
//
// The deploy dedup check needs this because restampWorkflowVersion above writes
// the version this command ASSIGNS into the stored binary, while the file on
// disk still carries whatever `cleat build --version` wrote. Comparing the two
// as stored would therefore never match, so the guard would not merely fail to
// fire -- it would be wrong, and would look wired while doing nothing.
//
// Setting both sides to the same constant removes the stamp from the
// comparison without removing it from the artifact. The rewrite is
// deterministic, which is what makes it usable as a normaliser: SetMetadataField
// decodes the payload into a map and re-encodes it, and encoding/json sorts map
// keys, so equal content yields equal bytes. See its doc comment for what else
// changes (interior whitespace) and why nothing depends on that.
//
// A binary with no readable cleat.metadata comes back unchanged, and that is
// correctness rather than a fallback: restampWorkflowVersion leaves exactly that
// shape alone too, so the stored bytes and the file on disk are already the same
// and there is nothing to normalise. ErrNotAJSONObject -- valid JSON with no
// keys to patch -- is the same case, and is deliberately not distinguished.
func normaliseWorkflowVersion(wasmBytes []byte) []byte {
	meta, err := wasm.ReadMetadata(wasmBytes)
	if err != nil || meta == nil {
		return wasmBytes
	}
	out, err := wasm.SetMetadataField(wasmBytes, "workflow_version", json.RawMessage("0"))
	if err != nil {
		return wasmBytes
	}
	return out
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

	// A new version adds a row rather than overwriting the old one -- the
	// command this replaces had one row per NAME, which is the model
	// plugin_defs deliberately does not use. Versions are IMMUTABLE
	// (cleat#2135): redeploying an existing (name, version) with the same
	// bytes is a no-op, and with different bytes is refused, naming both
	// checksums. There is no override for that refusal -- publish a new
	// version instead.
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
