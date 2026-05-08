package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rcownie/cleat/internal/host"
	"github.com/rcownie/cleat/internal/plugin"
	"golang.org/x/mod/semver"
)

func runPlugin(args []string) {
	if len(args) < 1 {
		printPluginUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "validate":
		runPluginValidate(args[1:])
	case "install":
		runPluginInstall(args[1:])
	case "list":
		runPluginList(args[1:])
	case "update":
		runPluginUpdate(args[1:])
	case "uninstall":
		runPluginUninstall(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown plugin subcommand: %s\n", args[0])
		printPluginUsage()
		os.Exit(1)
	}
}

func printPluginUsage() {
	fmt.Fprintf(os.Stderr, "Usage: cleat plugin <subcommand> [flags] [args]\n")
	fmt.Fprintf(os.Stderr, "\nSubcommands:\n")
	fmt.Fprintf(os.Stderr, "  validate  Validate a plugin manifest\n")
	fmt.Fprintf(os.Stderr, "  install   Install a plugin from the index\n")
	fmt.Fprintf(os.Stderr, "  list      List installed or available plugins\n")
	fmt.Fprintf(os.Stderr, "  update    Check for plugin updates\n")
	fmt.Fprintf(os.Stderr, "  uninstall Mark a plugin version as deprecated\n")
	fmt.Fprintf(os.Stderr, "\nExamples:\n")
	fmt.Fprintf(os.Stderr, "  cleat plugin validate --manifest plugin.json\n")
	fmt.Fprintf(os.Stderr, "  cleat plugin install acme/salesforce@^1.0.0\n")
	fmt.Fprintf(os.Stderr, "  cleat plugin install acme/salesforce\n")
	fmt.Fprintf(os.Stderr, "  cleat plugin list\n")
	fmt.Fprintf(os.Stderr, "  cleat plugin list --available --index-url plugins/index.yaml\n")
	fmt.Fprintf(os.Stderr, "  cleat plugin update --all\n")
	fmt.Fprintf(os.Stderr, "  cleat plugin uninstall my-plugin 1.0.0\n")
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func runPluginValidate(args []string) {
	fs := flag.NewFlagSet("plugin validate", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "path to plugin manifest file")
	fs.Parse(args)

	if *manifestPath == "" {
		fmt.Fprintf(os.Stderr, "Usage: cleat plugin validate --manifest <file>\n")
		os.Exit(1)
	}

	m, err := plugin.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := plugin.ValidateManifest(m); err != nil {
		fmt.Fprintf(os.Stderr, "Validation failed:\n%v\n", err)
		os.Exit(1)
	}

	fmt.Println("valid")
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

func runPluginInstall(args []string) {
	fs := flag.NewFlagSet("plugin install", flag.ExitOnError)
	indexURL := fs.String("index-url", "https://plugins.cleat.dev/index.yaml", "plugin index URL or file path")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	dryRun := fs.Bool("dry-run", false, "print what would be done without making changes")
	fs.Parse(args)

	remainder := fs.Args()
	if len(remainder) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: cleat plugin install [--index-url <url>] [--yes] [--dry-run] <name>[@<constraint>]\n")
		os.Exit(1)
	}

	spec := remainder[0]
	name, constraint := parsePluginSpec(spec)

	ctx := context.Background()

	// Fetch the plugin index.
	idx, err := plugin.FetchIndex(ctx, *indexURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching plugin index: %v\n", err)
		os.Exit(1)
	}

	// Resolve the constraint to the best matching version.
	entry, version, err := idx.Resolve(name, constraint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving plugin %q: %v\n", name, err)
		os.Exit(1)
	}

	// Display plugin info.
	fmt.Printf("Plugin: %s\n", entry.Name)
	fmt.Printf("  Description: %s\n", entry.Description)
	fmt.Printf("  Author: %s\n", entry.Author)
	if entry.Repository != "" {
		fmt.Printf("  Repository: %s\n", entry.Repository)
	}
	fmt.Printf("  Version: %s\n", version.Version)
	if version.Description != "" {
		fmt.Printf("  Version description: %s\n", version.Description)
	}
	if version.MinCleatVersion != "" {
		fmt.Printf("  Requires cleat >= %s\n", version.MinCleatVersion)
	}

	// Bundled plugins don't need to be installed.
	if version.Bundled {
		fmt.Println()
		fmt.Println("This plugin is bundled with cleat-worker and does not need to be installed.")
		return
	}

	// Security warning for community plugins.
	if !entry.IsOfficial() {
		fmt.Println()
		fmt.Println("  SECURITY WARNING: This is a third-party plugin.")
		fmt.Println("  Plugins have access to your database and infrastructure.")
		fmt.Println("  Only install plugins from trusted sources.")
		fmt.Println("  Review the source code and manifest before installing.")
	}

	// Confirmation prompt.
	if !*yes {
		fmt.Println()
		fmt.Print("Install this plugin? [y/N] ")
		var response string
		fmt.Scanln(&response)
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Installation cancelled.")
			return
		}
	}

	if *dryRun {
		fmt.Println()
		fmt.Println("Dry run: no changes were made.")
		if version.WasmURL != "" {
			fmt.Printf("  Would download: %s\n", version.WasmURL)
		}
		if version.Checksum != "" {
			fmt.Printf("  Would verify checksum: %s\n", version.Checksum)
		}
		connStr := getDBConnStr()
		if connStr != "" {
			fmt.Printf("  Would deploy to database: %s v%s\n", name, version.Version)
		} else {
			fmt.Printf("  Would deploy to database (set --db or CLEAT_DATABASE_URL): %s v%s\n", name, version.Version)
		}
		return
	}

	// Download WASM binary.
	if version.WasmURL == "" {
		fmt.Println("Error: no WASM URL specified for this version; it may only be available as bundled.")
		os.Exit(1)
	}

	fmt.Printf("Downloading %s v%s...\n", name, version.Version)
	wasmBytes, err := plugin.DownloadWASM(ctx, version.WasmURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error downloading WASM: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Downloaded %d bytes\n", len(wasmBytes))

	// Verify checksum.
	if version.Checksum != "" {
		fmt.Print("Verifying checksum...")
		if err := plugin.VerifyChecksum(wasmBytes, version.Checksum); err != nil {
			fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(" OK")
	}

	// Deploy to database.
	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Println("No database connection configured. Skipping deployment.")
		fmt.Println("Set --db or CLEAT_DATABASE_URL to deploy to a database.")
		fmt.Printf("WASM binary for %s v%s is available at: %s\n", name, version.Version, version.WasmURL)
		return
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	loader := host.NewPluginLoader(db, nil)
	if err := loader.DeployPlugin(ctx, name, version.Version, wasmBytes, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Error deploying plugin: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully installed %s v%s\n", name, version.Version)
}

// parsePluginSpec splits a "name@constraint" string into name and constraint.
// If there is no "@", the constraint is empty (meaning latest).
func parsePluginSpec(spec string) (name, constraint string) {
	if idx := strings.Index(spec, "@"); idx >= 0 {
		return spec[:idx], spec[idx+1:]
	}
	return spec, ""
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func runPluginList(args []string) {
	fs := flag.NewFlagSet("plugin list", flag.ExitOnError)
	available := fs.Bool("available", false, "list available plugins from index")
	indexURL := fs.String("index-url", "https://plugins.cleat.dev/index.yaml", "plugin index URL or file path")
	fs.Parse(args)

	if *available {
		listAvailablePlugins(*indexURL)
		return
	}

	listInstalledPlugins()
}

func listAvailablePlugins(indexURL string) {
	ctx := context.Background()
	idx, err := plugin.FetchIndex(ctx, indexURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching plugin index: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%-28s %-20s %-48s %s\n", "NAME", "VERSIONS", "DESCRIPTION", "AUTHOR")
	fmt.Println(strings.Repeat("-", 120))
	for _, p := range idx.Plugins {
		versionStrs := make([]string, 0, len(p.Versions))
		for _, v := range p.Versions {
			s := v.Version
			if v.Bundled {
				s += " (bundled)"
			}
			versionStrs = append(versionStrs, s)
		}
		desc := p.Description
		if len(desc) > 45 {
			desc = desc[:42] + "..."
		}
		fmt.Printf("%-28s %-20s %-48s %s\n", p.Name, strings.Join(versionStrs, ", "), desc, p.Author)
	}
}

func listInstalledPlugins() {
	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Println("No database connection configured. Use --available to list from the plugin index.")
		fmt.Println("Set --db or CLEAT_DATABASE_URL to list installed plugins.")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(),
		`SELECT name, version, created_at, deprecated FROM plugin_defs ORDER BY name, version DESC`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying plugins: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	var found bool
	fmt.Printf("%-28s %-15s %-30s %s\n", "NAME", "VERSION", "INSTALLED AT", "STATUS")
	fmt.Println(strings.Repeat("-", 85))
	for rows.Next() {
		var name, version string
		var createdAt time.Time
		var deprecated bool
		if err := rows.Scan(&name, &version, &createdAt, &deprecated); err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning row: %v\n", err)
			os.Exit(1)
		}
		status := "active"
		if deprecated {
			status = "deprecated"
		}
		fmt.Printf("%-28s %-15s %-30s %s\n", name, version, createdAt.Format(time.RFC3339), status)
		found = true
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error iterating rows: %v\n", err)
		os.Exit(1)
	}
	if !found {
		fmt.Println("No plugins installed.")
	}
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

func runPluginUpdate(args []string) {
	fs := flag.NewFlagSet("plugin update", flag.ExitOnError)
	all := fs.Bool("all", false, "check all installed plugins for updates")
	indexURL := fs.String("index-url", "https://plugins.cleat.dev/index.yaml", "plugin index URL or file path")
	fs.Parse(args)

	name := ""
	if !*all {
		remainder := fs.Args()
		if len(remainder) < 1 {
			fmt.Fprintf(os.Stderr, "Usage: cleat plugin update [--all] [--index-url <url>] [<name>]\n")
			fmt.Fprintf(os.Stderr, "  cleat plugin update my-plugin\n")
			fmt.Fprintf(os.Stderr, "  cleat plugin update --all\n")
			os.Exit(1)
		}
		name = remainder[0]
	}

	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Println("No database connection configured. Set --db or CLEAT_DATABASE_URL.")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	idx, err := plugin.FetchIndex(ctx, *indexURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching plugin index: %v\n", err)
		os.Exit(1)
	}

	if *all {
		checkAllPluginUpdates(ctx, db, idx)
		return
	}

	checkSinglePluginUpdate(ctx, db, idx, name)
}

func checkAllPluginUpdates(ctx context.Context, db *sql.DB, idx *plugin.PluginIndex) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT name FROM plugin_defs WHERE NOT deprecated ORDER BY name`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying plugins: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var pluginName string
		if err := rows.Scan(&pluginName); err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning row: %v\n", err)
			continue
		}
		checkSinglePluginUpdate(ctx, db, idx, pluginName)
		found = true
	}
	if !found {
		fmt.Println("No plugins installed.")
	}
}

func checkSinglePluginUpdate(ctx context.Context, db *sql.DB, idx *plugin.PluginIndex, name string) {
	// Get current installed version (highest non-deprecated).
	var currentVersion string
	err := db.QueryRowContext(ctx,
		`SELECT version FROM plugin_defs WHERE name = $1 AND NOT deprecated ORDER BY version DESC LIMIT 1`,
		name).Scan(&currentVersion)
	if err == sql.ErrNoRows {
		fmt.Printf("%s: not installed\n", name)
		return
	}
	if err != nil {
		fmt.Printf("%s: error querying: %v\n", name, err)
		return
	}

	// Get latest available from index.
	_, latest, err := idx.Resolve(name, "")
	if err != nil {
		fmt.Printf("%s: %s (not found in index)\n", name, currentVersion)
		return
	}

	cv := ensureV(currentVersion)
	lv := "v" + latest.Version

	if semver.Compare(lv, cv) > 0 {
		fmt.Printf("%s: %s -> %s (update available)\n", name, currentVersion, latest.Version)
	} else {
		fmt.Printf("%s: %s (latest)\n", name, currentVersion)
	}
}

// ensureV adds a "v" prefix if not present.
func ensureV(v string) string {
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// ---------------------------------------------------------------------------
// uninstall (deprecate)
// ---------------------------------------------------------------------------

func runPluginUninstall(args []string) {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: cleat plugin uninstall <name> <version>\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  cleat plugin uninstall acme/salesforce 1.0.0\n")
		os.Exit(1)
	}
	name := args[0]
	version := args[1]

	connStr := getDBConnStr()
	if connStr == "" {
		fmt.Println("No database connection configured. Set --db or CLEAT_DATABASE_URL.")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	loader := host.NewPluginLoader(db, nil)
	if err := loader.DeprecatePlugin(context.Background(), name, version); err != nil {
		fmt.Fprintf(os.Stderr, "Error deprecating plugin: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deprecated %s v%s\n", name, version)
}
