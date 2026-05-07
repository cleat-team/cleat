// Command cleat is the workflow transformer CLI.
//
// Usage:
//
//	cleat build <package>     — analyze and compile a workflow package
//	cleat vet <package>       — validate a workflow package (no compilation)
//
// The build command runs the full transformer pipeline: package loading,
// call graph construction, cleat closure computation, HostCalls threading
// verification, WASM import generation, host adapter generation, WASM export
// generation, and compilation.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"go/token"
	"time"

	_ "github.com/lib/pq"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/callgraph"
	"github.com/rcownie/cleat/internal/closure"
	"github.com/rcownie/cleat/internal/host"
	"github.com/rcownie/cleat/internal/transform"
	"github.com/rcownie/cleat/internal/wasm"
)

var dbConnStr string

func main() {
	flag.StringVar(&dbConnStr, "db", "", "PostgreSQL connection string (or set CLEAT_DATABASE_URL)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cleat <build|vet|deploy|versions|rollback|dev|schedule|run|dag|init> [flags] <args>\n")
		fmt.Fprintf(os.Stderr, "  cleat build [-o <dir>] [--target <target>] <package>\n")
		fmt.Fprintf(os.Stderr, "  cleat vet <package>\n")
		fmt.Fprintf(os.Stderr, "  cleat deploy [--name <name>] [--namespace <ns>] [--task-queue <queue>] <wasm-file>\n")
		fmt.Fprintf(os.Stderr, "  cleat versions <workflow-name>\n")
		fmt.Fprintf(os.Stderr, "  cleat rollback <workflow-name> <version>\n")
		fmt.Fprintf(os.Stderr, "  cleat dev [--input <json>] [--entry-point <name>] <package>\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule add <name> --cron <expr> --def <wf-name> [--entry-point <name>] [--input <json>]\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule list\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule delete <name>\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule enable <name>\n")
		fmt.Fprintf(os.Stderr, "  cleat schedule disable <name>\n")
		fmt.Fprintf(os.Stderr, "  cleat run [--wasm <file>] [--entry-point <name>] [--input <json>] [--api-addr <addr>] <package>\n")
		fmt.Fprintf(os.Stderr, "Common flags:\n")
		fmt.Fprintf(os.Stderr, "  --db <connstr>  PostgreSQL connection string\n")
		fmt.Fprintf(os.Stderr, "Example: cleat build -o ./out ./testdata/basic/\n")
	}
	flag.Parse()

	args := flag.Args()
	// init doesn't require a second argument (pattern)
	if len(args) >= 1 && args[0] == "init" {
		runInit(flag.Args()[1:])
		return
	}
	if len(args) < 2 {
		flag.Usage()
		os.Exit(1)
	}

	command := args[0]
	pattern := args[1]

	switch command {
	case "build":
		var outDir string
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		var target string
		var entry string
		fs.StringVar(&outDir, "o", "", "output directory for generated files")

		fs.StringVar(&target, "target", "go", "compilation target: go, tinygo, rust, java, assemblyscript, or python")
		fs.StringVar(&entry, "entry", "", "entry point in 'file.py:func_name' format (for Python target)")
		fs.Parse(os.Args[2:])
		if !isValidTarget(target) {
			fmt.Fprintf(os.Stderr, "Error: unknown target %q. Valid targets: go, tinygo, rust, java, assemblyscript, python\n", target)
			os.Exit(1)
		}
		remainder := fs.Args()
		if len(remainder) > 0 {
			pattern = remainder[0]
		}
		if entry != "" {
			// Use --entry as the pattern for Python builds.
			runBuild(entry, outDir, target)
		} else {
			runBuild(pattern, outDir, target)
		}
	case "vet":
		runVet(pattern)
	case "deploy":
		runDeploy(flag.Args()[1:])
	case "versions":
		runVersions(args[1])
	case "rollback":
		if len(args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: cleat rollback <workflow-name> <version>\n")
			os.Exit(1)
		}
		version, err := strconv.Atoi(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: version must be a number, got %q\n", args[2])
			os.Exit(1)
		}
		runRollback(args[1], version)
	case "dev":
		runDev(flag.Args()[1:])
	case "schedule":
		runSchedule(flag.Args()[1:])
	case "run":
		runEmbedded(flag.Args()[1:])
	case "dag":
		runDag(flag.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		flag.Usage()
		os.Exit(1)
	}
}

func runBuild(pattern, outDir, target string) {
	if target == "java" {
		if outDir == "" {
			outDir = "."
		}
		runBuildJava(pattern, outDir)
		return
	}
	if target == "assemblyscript" {
		if outDir == "" {
			outDir = "."
		}
		runBuildAssemblyScript(pattern, outDir)
		return
	}
	if target == "rust" {
		if outDir == "" {
			outDir = "."
		}
		runBuildRust(pattern, outDir)
		return
	}
	if target == wasm.PythonTarget {
		if outDir == "" {
			outDir = "."
		}
		runBuildPython(pattern, outDir)
		return
	}
	result, cg, cr, threadingErrs, usage, tr := analyze(pattern)
	_ = cg

	fmt.Printf("  Analyzing package %s...\n", result.TargetPkg.Path)

	leafCount := len(cr.DurableLeaves)
	closureCount := len(cr.DurableClosure)
	fmt.Printf("  Found %d functions, %d entry point(s), %d in cleat closure.\n",
		result.NumFuncs, len(result.EntryPoints), leafCount+closureCount)
	fmt.Printf("  Durable leaves: %s\n", formatDurableLeaves(result, cr))
	fmt.Printf("  Verifying HostCalls threading... %s\n", formatThreadingStatus(threadingErrs))

	if len(threadingErrs) > 0 {
		fmt.Println()
		for _, e := range threadingErrs {
			fmt.Printf("  Error: %s\n", e.Message)
			if len(e.Chain) > 0 {
				fmt.Printf("         Call chain: %s\n", strings.Join(e.Chain, " → "))
			}
			if e.Line > 0 {
				fmt.Printf("         At: %d\n", e.Line)
			}
		}
		os.Exit(1)
	}

	warnCount := cr.NumWarnings()
	if warnCount > 0 {
		fmt.Println()
		for funcName, warns := range cr.Warnings {
			for _, w := range warns {
				fmt.Printf("  Warning: %s:%d: %s [%s]\n",
					analyzer.ShortName(funcName), w.Line, w.Message, w.Code)
			}
		}
	}

	errCount := cr.NumErrors()
	if errCount > 0 {
		fmt.Println()
		for funcName, errs := range cr.Errors {
			for _, e := range errs {
				fmt.Printf("  %s: %s:%d: %s\n", e.Code, analyzer.ShortName(funcName), e.Line, e.Message)
				if e.Suggestion != "" {
					fmt.Printf("    → %s\n", e.Suggestion)
				}
			}
		}
		os.Exit(1)
	}

	fmt.Println()

	outputs := wasm.BuildOutputs("main", usage, result)
	hostCount := usage.Count()
	fmt.Printf("  Generating WASM imports (%d host functions used)... ", hostCount)
	fmt.Println("OK")
	fmt.Printf("  Generating host adapter... OK\n")
	fmt.Printf("  Generating WASM exports (%d entry point(s))... OK\n", len(result.EntryPoints))

	if len(tr.AddedH) > 0 {
		fmt.Printf("  Auto-threading HostCalls into: %s\n", strings.Join(tr.AddedH, ", "))
	} else {
		fmt.Printf("  Auto-threading: no changes needed\n")
	}

	keepTempDir := false
	if outDir == "" {
		var err error
		outDir, err = os.MkdirTemp("", "cleat-build-**")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating temp directory: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			if !keepTempDir {
				os.RemoveAll(outDir)
			}
		}()
	}

	goVersion := result.GoVersion
	if goVersion == "" {
		goVersion = "1.26"
	}

	wasmFile := wasmOutputName(result)
	buildCfg := &wasm.BuildConfig{
		SrcDir:      result.TargetPkg.Dir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  result.ModulePath,
		ProjectRoot: result.ModuleDir,
		GoVersion:   goVersion,
		Outputs:     outputs,
		WASMOutput:  wasmFile,
		Target:      target,
		XfrmSource:  tr.Files,
	}

	if err := wasm.PrepareBuildDir(buildCfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing build directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("  Build directory: %s\n", outDir)

	wasmPath := filepath.Join(outDir, wasmFile)
	var cmd *exec.Cmd
	if target == "tinygo" {
		fmt.Printf("  Compiling WASM module (tinygo)...\n")
		cmd = exec.Command("tinygo", "build",
			"-target=wasip1",
			"-o", wasmPath,
			".",
		)
		// tinygo needs GOROOT and TINYGOROOT in its environment.
		// tinygo 0.36 requires host Go < 1.25. If CLEAT_TINYGO_GOROOT is set,
		// use it as GOROOT and add its bin to PATH ahead of the current PATH.
		cmd.Env = os.Environ()
		if tinygoGoroot := os.Getenv("CLEAT_TINYGO_GOROOT"); tinygoGoroot != "" {
			cmd.Env = append(cmd.Env, "GOROOT="+tinygoGoroot)
			cmd.Env = append(cmd.Env, "PATH="+tinygoGoroot+"/bin:"+os.Getenv("PATH"))
		} else if goroot := os.Getenv("GOROOT"); goroot != "" {
			cmd.Env = append(cmd.Env, "GOROOT="+goroot)
		}
		if tinygoroot := os.Getenv("TINYGOROOT"); tinygoroot != "" {
			cmd.Env = append(cmd.Env, "TINYGOROOT="+tinygoroot)
		}
	} else {
		fmt.Printf("  Compiling WASM module (GOOS=wasip1 GOARCH=wasm)...\n")
		cmd = exec.Command("go", "build",
			"-o", wasmPath,
			".",
		)
		cmd.Env = append(os.Environ(),
			"GOOS=wasip1",
			"GOARCH=wasm",
		)
	}
	cmd.Dir = outDir

	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error compiling WASM module:\n%s\n", string(out))
		os.Exit(1)
	}

	fi, err := os.Stat(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: WASM binary not found at %s\n", wasmPath)
		os.Exit(1)
	}
	fmt.Printf("  Wrote %s (%s)\n", wasmPath, formatSize(fi.Size()))
	keepTempDir = true
}

func runVet(pattern string) {
	result, _, cr, threadingErrs, usage, tr := analyze(pattern)
	_ = usage
	_ = tr

	fmt.Printf("Analyzing package %s...\n", result.TargetPkg.Path)
	fmt.Printf("  Package: %s\n", result.TargetPkg.Path)
	fmt.Printf("  Functions: %d (%d exported, %d unexported)\n",
		result.NumFuncs, result.NumExported, result.NumFuncs-result.NumExported)
	fmt.Printf("  Entry points: %s\n", strings.Join(shortEntryPoints(result), ", "))
	fmt.Printf("  Durable leaves: %d (%s)\n",
		len(cr.DurableLeaves), formatDurableLeaves(result, cr))

	exitCode := 0
	for _, e := range threadingErrs {
		fmt.Printf("  Error: %s\n", e.Message)
		if len(e.Chain) > 0 {
			fmt.Printf("         Call chain: %s\n", strings.Join(e.Chain, " → "))
		}
		exitCode = 1
	}
	for funcName, errs := range cr.Errors {
		for _, e := range errs {
			fmt.Printf("  %s:%d: %s: %s\n", analyzer.ShortName(funcName), e.Line, e.Code, e.Message)
			if e.Suggestion != "" {
				fmt.Printf("    → %s\n", e.Suggestion)
			}
			exitCode = 1
		}
	}
	for funcName, warns := range cr.Warnings {
		for _, w := range warns {
			fmt.Printf("  %s:%d: %s: %s\n", analyzer.ShortName(funcName), w.Line, w.Code, w.Message)
		}
	}

	if exitCode == 0 {
		fmt.Printf("  %s\n", "OK")
	}
	os.Exit(exitCode)
}

// runDeploy deploys a compiled WASM workflow to the database.
// Usage: cleat deploy [--name <name>] [--namespace <ns>] [--task-queue <queue>] <wasm-file>
func runDeploy(args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	nameFlag := fs.String("name", "", "workflow name (derived from filename if not set)")
	nsFlag := fs.String("namespace", "", "workflow namespace (default: \"default\")")
	taskQueueFlag := fs.String("task-queue", "default", "task queue for this workflow (e.g. default, gpu, high-memory)")
	fs.Parse(args)

	remainder := fs.Args()
	if len(remainder) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat deploy [--name <name>] [--namespace <ns>] [--task-queue <queue>] <wasm-file>\n")
		os.Exit(1)
	}
	wasmPath := remainder[0]
	_ = nsFlag

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading WASM file %s: %v\n", wasmPath, err)
		os.Exit(1)
	}

	// Extract metadata from the WASM custom section.
	meta, metaErr := wasm.ReadMetadata(wasmBytes)
	if metaErr == nil {
		fmt.Printf("  Workflow: %s v%d (ABI v%d, min compatible: %d)\n",
			meta.WorkflowName, meta.WorkflowVersion,
			meta.ABIVersion, meta.MinCompatibleVersion)
		if len(meta.PluginDeps) > 0 {
			fmt.Printf("  Plugin deps: %v\n", meta.PluginDeps)
		}
		if err := meta.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: metadata validation failed: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Printf("  Note: no cleat.metadata section found in WASM binary (%v)\n", metaErr)
		fmt.Printf("  Continuing with flags-only configuration.\n")
	}

	name := *nameFlag
	if name == "" {
		if metaErr == nil && meta.WorkflowName != "unknown" {
			name = meta.WorkflowName
		} else {
			name = strings.TrimSuffix(filepath.Base(wasmPath), ".wasm")
		}
	}

	connStr := getDBConnStr()

	if connStr == "" {
		version := 1
		if metaErr == nil && meta.WorkflowVersion > 0 {
			version = meta.WorkflowVersion
		}
		fmt.Printf("Would deploy workflow %q (version %d) from %s (%d bytes) to queue %q\n",
			name, version, wasmPath, len(wasmBytes), *taskQueueFlag)
		if metaErr == nil {
			fmt.Printf("  Metadata: %s v%d (ABI: %d, min ver: %d)\n",
				meta.WorkflowName, meta.WorkflowVersion,
				meta.ABIVersion, meta.MinCompatibleVersion)
		}
		fmt.Println("Dry run; set CLEAT_DATABASE_URL or --db to deploy.")
		return
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	var version int
	err = db.QueryRow("SELECT COALESCE(MAX(version), 0) + 1 FROM workflow_defs WHERE name = $1", name).Scan(&version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying max version: %v\n", err)
		os.Exit(1)
	}

	namespace := *nsFlag
	if namespace == "" {
		namespace = "default"
	}

	// Build the SQL with metadata columns if available.
	abiVersion := 1
	minVersion := 1
	pluginDepsJSON := "{}"
	if metaErr == nil {
		abiVersion = meta.ABIVersion
		minVersion = meta.MinCompatibleVersion
		if depsBytes, merr := json.Marshal(meta.PluginDeps); merr == nil {
			pluginDepsJSON = string(depsBytes)
		}
	}

	_, err = db.Exec(
		"INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, plugin_deps, min_version, entry_points, namespace, task_queue) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9)",
		name, version, wasmBytes, abiVersion, pluginDepsJSON, minVersion, []string{}, namespace, *taskQueueFlag,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inserting workflow definition: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deployed workflow %q version %d (%d bytes) to queue %q\n", name, version, len(wasmBytes), *taskQueueFlag)
	if metaErr == nil {
		fmt.Printf("  Metadata: %s v%d (ABI: %d, min ver: %d, plugins: %v)\n",
			meta.WorkflowName, meta.WorkflowVersion,
			meta.ABIVersion, meta.MinCompatibleVersion, meta.PluginDeps)
	}
}

func analyze(pattern string) (*analyzer.AnalysisResult, *callgraph.Graph, *closure.Result, []closure.ThreadingError, *wasm.UsageInfo, *transform.Result) {
	fset := token.NewFileSet()

	result, err := analyzer.LoadPackages(pattern, fset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading package: %v\n", err)
		os.Exit(1)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building call graph: %v\n", err)
		os.Exit(1)
	}

	cr := closure.Compute(result, cg)

	tr, err := transform.Transform(&transform.Config{
		Result:    result,
		CallGraph: cg,
		Closure:   cr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error in transform: %v\n", err)
		os.Exit(1)
	}

	threadingErrs := closure.VerifyThreading(result, cg, cr)

	usage := wasm.AnalyzeUsage(result, cr)

	return result, cg, cr, threadingErrs, usage, tr
}

func formatDurableLeaves(result *analyzer.AnalysisResult, cr *closure.Result) string {
	var names []string
	for name := range cr.DurableLeaves {
		names = append(names, analyzer.ShortName(name))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func formatThreadingStatus(errs []closure.ThreadingError) string {
	if len(errs) == 0 {
		return "OK"
	}
	return fmt.Sprintf("%d error(s)", len(errs))
}

func shortEntryPoints(result *analyzer.AnalysisResult) []string {
	var names []string
	for _, ep := range result.EntryPoints {
		names = append(names, analyzer.ShortName(ep))
	}
	return names
}

func wasmOutputName(result *analyzer.AnalysisResult) string {
	if len(result.EntryPoints) == 0 {
		return "output.wasm"
	}
	return wasm.ToSnakeCase(analyzer.ShortName(result.EntryPoints[0])) + ".wasm"
}

// getDBConnStr returns the database connection string from the --db flag
// or the CLEAT_DATABASE_URL environment variable.
func getDBConnStr() string {
	if dbConnStr != "" {
		return dbConnStr
	}
	return os.Getenv("CLEAT_DATABASE_URL")
}

// runVersions lists all deployed versions of a workflow, latest first.
func runVersions(name string) {
	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Fprintf(os.Stderr, "Error: --db flag or CLEAT_DATABASE_URL is required\n")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	rows, err := db.Query("SELECT version FROM workflow_defs WHERE name = $1 ORDER BY version DESC", name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying versions: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning row: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(version)
		found = true
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}
	if !found {
		fmt.Printf("No versions found for workflow %q\n", name)
	}
}

// runRollback sets the active version for a workflow by confirming the version
// exists and printing instructions for new instances.
func runRollback(name string, version int) {
	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Fprintf(os.Stderr, "Error: --db flag or CLEAT_DATABASE_URL is required\n")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	var exists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM workflow_defs WHERE name = $1 AND version = $2)", name, version).Scan(&exists)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking version: %v\n", err)
		os.Exit(1)
	}
	if !exists {
		fmt.Fprintf(os.Stderr, "Error: workflow %q version %d not found\n", name, version)
		os.Exit(1)
	}

	fmt.Printf("Rolled back %q to version %d. New instances will use version %d.\n", name, version, version)
}

// runSchedule manages cron schedules for recurring workflow execution.
func runSchedule(args []string) {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat schedule <add|list|delete|enable|disable> [args]\n")
		os.Exit(1)
	}

	subCmd := args[0]
	remainder := args[1:]

	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Fprintf(os.Stderr, "Error: --db flag or CLEAT_DATABASE_URL is required\n")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "Error pinging database: %v\n", err)
		os.Exit(1)
	}

	store := host.NewPostgresStore(db)
	ctx := context.Background()

	switch subCmd {
	case "add":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule add <name> --cron <expr> --def <wf-name> [--entry-point <name>] [--input <json>]\n")
			os.Exit(1)
		}
		fs := flag.NewFlagSet("schedule add", flag.ExitOnError)
		cronExpr := fs.String("cron", "", "cron expression (5-field: minute hour dom month dow)")
		defName := fs.String("def", "", "workflow definition name")
		entryPoint := fs.String("entry-point", "", "entry point function name")
		inputJSON := fs.String("input", "{}", "workflow input JSON")
		fs.Parse(remainder)

		fsArgs := fs.Args()
		if len(fsArgs) < 1 || *cronExpr == "" || *defName == "" {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule add <name> --cron <expr> --def <wf-name> [--entry-point <name>] [--input <json>]\n")
			os.Exit(1)
		}
		name := fsArgs[0]

		nextRun := host.NextCronTime(*cronExpr, time.Now())
		sch := host.Schedule{
			Name:           name,
			DefName:        *defName,
			EntryPoint:     *entryPoint,
			CronExpression: *cronExpr,
			Input:          json.RawMessage(*inputJSON),
			Enabled:        true,
			NextRunAt:      nextRun,
		}

		if err := store.CreateSchedule(ctx, sch); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created schedule %q: %s every %s (next at %s)\n",
			name, *defName, *cronExpr, nextRun.Format(time.RFC3339))

	case "list":
		schedules, err := store.ListSchedules(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing schedules: %v\n", err)
			os.Exit(1)
		}
		if len(schedules) == 0 {
			fmt.Println("No schedules found.")
			return
		}
		fmt.Printf("%-20s %-20s %-20s %-7s %s\n", "NAME", "DEFINITION", "CRON", "ENABLED", "NEXT RUN")
		for _, sch := range schedules {
			enabled := "no"
			if sch.Enabled {
				enabled = "yes"
			}
			fmt.Printf("%-20s %-20s %-20s %-7s %s\n",
				sch.Name, sch.DefName, sch.CronExpression, enabled,
				sch.NextRunAt.Format(time.RFC3339))
		}

	case "delete":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule delete <name>\n")
			os.Exit(1)
		}
		if err := store.DeleteSchedule(ctx, remainder[0]); err != nil {
			fmt.Fprintf(os.Stderr, "Error deleting schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted schedule %q\n", remainder[0])

	case "enable":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule enable <name>\n")
			os.Exit(1)
		}
		if err := store.SetScheduleEnabled(ctx, remainder[0], true); err != nil {
			fmt.Fprintf(os.Stderr, "Error enabling schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Enabled schedule %q\n", remainder[0])

	case "disable":
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat schedule disable <name>\n")
			os.Exit(1)
		}
		if err := store.SetScheduleEnabled(ctx, remainder[0], false); err != nil {
			fmt.Fprintf(os.Stderr, "Error disabling schedule: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Disabled schedule %q\n", remainder[0])

	default:
		fmt.Fprintf(os.Stderr, "Unknown schedule subcommand: %s\n", subCmd)
		os.Exit(1)
	}
}

func isValidTarget(t string) bool {
	valid := map[string]bool{
		"go": true, "tinygo": true, "rust": true,
		"java": true, "assemblyscript": true, "python": true,
	}
	return valid[t]
}

func formatSize(n int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
	)
	switch {
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
