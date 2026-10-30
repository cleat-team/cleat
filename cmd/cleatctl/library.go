package main

// cleat#3597: `cleatctl library publish` and `cleatctl library list`.
//
// These two commands are different from every other cleatctl command in one way that decides
// where they are dispatched: they are for the LIBRARY PUBLISHER's database login (D40), not for
// the operator's superuser one. That role can write the library schema and nothing else (it cannot
// read workflow_defs; measured: `versions list` fails with it). The store itself still opens for
// that login, so the store is not the reason: it is that `library` has no use for it, and that the
// tenant-scoped warning main() prints right after connecting ("this tool can see only one tenant's
// rows") describes the operator's questions, which the publisher asks none of. So main hands
// `library` the bare *sql.DB the moment it is connected, before the warning and before the store.
//
// A member is named by its own stamp. The binary's cleat.metadata carries workflow_name,
// workflow_version, the ABI, plugin_deps, child_versions, entry points and exposure; publish
// reads them from the file and does not let the command line rename or renumber it, so a
// member cannot be filed under a name its bytes do not claim.

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/wasm"
)

func printLibraryUsage() {
	fmt.Fprint(os.Stderr, `Usage: cleatctl --db <publisher-dsn> library <command>

Connect with a login that holds cleat_library_publisher (PostgreSQL and SQL Server);
the operator's superuser login works too, and the application role does not.

Commands:
  library publish <library> <bundle-version> <member.wasm>... [--published-by <name>] [--json]
      publish one immutable bundle. Each member is named by its own cleat.metadata stamp.
      A member or bundle that is already published with the same content is a no-op;
      different content under the same key is refused. One transaction: all or nothing.
  library list [<library>] [--json]
      list published bundles and their members (digest and size, never the bytes)
`)
}

// runLibrary dispatches the library subcommands. It is called by main() before the store is
// opened, with the connection the publisher gave.
func runLibrary(ctx context.Context, db *sql.DB, d engine.Dialect, args []string) {
	if len(args) < 1 {
		printLibraryUsage()
		osExit(1)
	}
	switch args[0] {
	case "publish":
		libraryPublish(ctx, db, d, args[1:])
	case "list":
		libraryList(ctx, db, d, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown library command %q\n\n", args[0])
		printLibraryUsage()
		osExit(1)
	}
}

func libraryPublish(ctx context.Context, db *sql.DB, d engine.Dialect, args []string) {
	fs := flag.NewFlagSet("library publish", flag.ContinueOnError)
	publishedBy := fs.String("published-by", "", "who is publishing, recorded beside the bundle (default: the operating-system user)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	positional, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(1)
	}
	if len(positional) < 3 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl library publish <library> <bundle-version> <member.wasm>... [--published-by <name>] [--json]")
		osExit(1)
	}
	req := engine.PublishLibraryRequest{Library: positional[0], BundleVersion: positional[1], PublishedBy: *publishedBy}
	if req.PublishedBy == "" {
		req.PublishedBy = engine.CommandLineActor()
	}
	for _, path := range positional[2:] {
		m, err := libraryMemberFromFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			osExit(1)
		}
		req.Members = append(req.Members, m)
	}

	res, err := engine.PublishLibraryBundle(ctx, db, d, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
	}
	if *asJSON {
		printJSON(res)
		return
	}
	switch {
	case res.BundleAlreadyPublished:
		fmt.Printf("library %s bundle %s is already published with exactly these %d member(s); nothing written\n",
			res.Library, res.BundleVersion, len(req.Members))
	default:
		fmt.Printf("published library %s bundle %s: %d member(s) written, %d already published (unchanged)\n",
			res.Library, res.BundleVersion, res.MembersWritten, res.MembersAlreadyPublished)
	}
}

// libraryMemberFromFile reads one member from its binary: bytes, and everything the stamp
// carries. A binary with no readable stamp cannot be published, because the stamp is what
// names it; a sidecar schema file that exists and is not valid JSON is refused rather than
// skipped, because a published member is immutable and a record missing its schema could not
// be corrected afterwards.
func libraryMemberFromFile(path string) (engine.PublishLibraryMember, error) {
	var m engine.PublishLibraryMember
	b, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(b) == 0 {
		return m, fmt.Errorf("%s is empty", path)
	}
	meta, err := wasm.ReadMetadata(b)
	if err != nil || meta == nil {
		return m, fmt.Errorf("%s has no readable cleat.metadata stamp, which is what names a library member "+
			"(its workflow_name and workflow_version); build it with `cleat build` so it is stamped: %v", path, err)
	}
	if meta.WorkflowName == "" {
		return m, fmt.Errorf("%s: its cleat.metadata stamp has no workflow_name", path)
	}
	m = engine.PublishLibraryMember{
		Name:          meta.WorkflowName,
		Version:       meta.WorkflowVersion,
		WasmBytes:     b,
		ABIVersion:    meta.ABIVersion,
		MinVersion:    meta.MinCompatibleVersion,
		PluginDeps:    meta.PluginDeps,
		ChildVersions: meta.ChildVersions,
		EntryPoints:   meta.EntryPoints,
		Exposure:      engine.ExposureClass(meta.Exposure),
	}
	if schemaBytes, err := os.ReadFile(path + ".schema.json"); err == nil {
		var schemas map[string]engine.EntryPointSchema
		if err := json.Unmarshal(schemaBytes, &schemas); err != nil {
			return m, fmt.Errorf("%s.schema.json is not valid JSON: %w", path, err)
		}
		m.EntryPointSchemas = schemas
	}
	return m, nil
}

func libraryList(ctx context.Context, db *sql.DB, d engine.Dialect, args []string) {
	fs := flag.NewFlagSet("library list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	positional, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(1)
	}
	if len(positional) > 1 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl library list [<library>] [--json]")
		osExit(1)
	}
	library := ""
	if len(positional) == 1 {
		library = positional[0]
	}
	bundles, err := engine.ListLibraryBundles(ctx, db, d, library)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
	}
	if *asJSON {
		if bundles == nil {
			bundles = []engine.LibraryBundleSummary{}
		}
		printJSON(bundles)
		return
	}
	if len(bundles) == 0 {
		fmt.Println("no published bundles")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LIBRARY\tBUNDLE\tPUBLISHED\tBY\tMEMBERS")
	for _, b := range bundles {
		members := ""
		for i, m := range b.Members {
			if i > 0 {
				members += ", "
			}
			members += fmt.Sprintf("%s@%d", m.Name, m.Version)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", b.Library, b.BundleVersion, b.PublishedAt.UTC().Format("2006-01-02 15:04:05Z"), b.PublishedBy, members)
	}
	w.Flush()
}

func printJSON(v any) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: encoding the result: %v\n", err)
		osExit(1)
	}
	fmt.Println(string(out))
}
