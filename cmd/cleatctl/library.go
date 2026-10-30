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
	"net/url"
	"os"
	"strings"
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
                  [--replicate-to <publisher-dsn>]...
      publish one immutable bundle. Each member is named by its own cleat.metadata stamp.
      A member or bundle that is already published with the same content is a no-op;
      different content under the same key is refused. One transaction per database:
      all or nothing there.
      On a SHARDED deployment give --db the ADMIN shard (the first in the worker's shards
      file) and each other shard once with --replicate-to, in the worker's order, each with
      that shard's own publisher login. The admin shard is written first and a failure there
      changes nothing; every other shard is then written, and running the same command again
      completes any that failed.
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
	var replicateTo stringList
	fs.Var(&replicateTo, "replicate-to", "another shard's publisher DSN, repeatable, in the worker's shard order (--db is the admin shard)")
	positional, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(1)
	}
	if len(positional) < 3 {
		fmt.Fprintln(os.Stderr, "usage: cleatctl library publish <library> <bundle-version> <member.wasm>... [--published-by <name>] [--json] [--replicate-to <publisher-dsn>]...")
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

	if len(replicateTo) > 0 {
		libraryPublishSharded(ctx, db, d, req, replicateTo, *asJSON)
		return
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

// stringList is a flag that may be given more than once.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// libraryPublishSharded publishes to the admin shard (the --db connection) and then to every
// --replicate-to DSN, in order. Every extra DSN is opened and pinged BEFORE the first write: a
// typo or an unreachable shard is then found with nothing changed, and the failures left for
// the fan-out are the ones that cannot be known in advance. cleat#3597, after D55.
//
// A shard is called shard-N in every message, N being its place in the order, and the mapping from
// those names to a target is printed first, so an error can be read. A target is host and database
// only: never the user, never the password.
func libraryPublishSharded(ctx context.Context, admin *sql.DB, d engine.Dialect, req engine.PublishLibraryRequest, dsns []string, asJSON bool) {
	shards := []engine.LibraryShard{{Name: "shard-1", DB: admin}}
	targets := []string{"shard-1 = the --db connection (the admin shard)"}
	for i, dsn := range dsns {
		name := fmt.Sprintf("shard-%d", i+2)
		target := fmt.Sprintf("%s = %s", name, redactedTarget(dsn))
		if rd := detectDialect(dsn); rd.name != string(d) {
			fmt.Fprintf(os.Stderr, "error: %s is a %s database, but --db is %s: the shards of one deployment are one kind of database. Nothing was written to any shard.\n", target, rd.name, d)
			osExit(1)
		}
		if err := rejectMySQLScheme(dsn); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v. Nothing was written to any shard.\n", name, err)
			osExit(1)
		}
		rd := detectDialect(dsn)
		sdb, err := sql.Open(rd.driver, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s could not be opened: %v. Nothing was written to any shard.\n", target, err)
			osExit(1)
		}
		defer sdb.Close()
		if err := sdb.PingContext(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s cannot be reached: %v. Nothing was written to any shard.\n", target, err)
			osExit(1)
		}
		shards = append(shards, engine.LibraryShard{Name: name, DB: sdb})
		targets = append(targets, target)
	}

	res, err := engine.PublishLibraryBundleToShards(ctx, shards, d, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shards, in order:\n  %s\n", strings.Join(targets, "\n  "))
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
	}
	if asJSON {
		printJSON(res)
		return
	}
	fmt.Printf("published library %s bundle %s to %d shard(s):\n", res.Library, res.BundleVersion, len(res.Shards))
	for i, s := range res.Shards {
		switch {
		case s.BundleAlreadyPublished:
			fmt.Printf("  %s: already published with exactly these members; nothing written   (%s)\n", s.Shard, strings.SplitN(targets[i], " = ", 2)[1])
		default:
			fmt.Printf("  %s: %d member(s) written, %d already published (unchanged)   (%s)\n", s.Shard, s.MembersWritten, s.MembersAlreadyPublished, strings.SplitN(targets[i], " = ", 2)[1])
		}
	}
}

// redactedTarget names where a DSN points, as host and database, and nothing that authenticates.
// A DSN that does not parse as a URL is not echoed back: it may be a key=value or driver-specific
// form whose password cannot be told from the rest.
func redactedTarget(dsn string) string {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil || u.Host == "" {
		return "a database whose DSN is not a URL (not shown)"
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		db = u.Query().Get("database") // SQL Server names the database in the query
	}
	if db == "" {
		return u.Host
	}
	return u.Host + "/" + db
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
