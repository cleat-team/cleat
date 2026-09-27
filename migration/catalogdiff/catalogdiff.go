// Package catalogdiff builds a structural snapshot of a database's schema and
// diffs two of them. It exists for cleat#2059: compacting migrations/*/'s
// growing file lists into a three-file baseline per dialect is a
// silent-regression machine, and reading the SQL cannot verify it (see
// docs/schema-partitioning-design.md, "Differential verification is
// non-negotiable"). The only trustworthy check is building two SCRATCH
// databases -- one from the full incremental migration chain, one from the
// compacted files -- and requiring an empty structural difference.
//
// Snapshot never mutates the database it reads. Diff never touches a
// database at all -- it operates on two Catalog values already in memory, so
// it can be unit-tested without a live connection.
package catalogdiff

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/migration"
)

// Catalog is a structural snapshot of one database's schema.
type Catalog struct {
	Dialect  migration.Dialect
	Tables   map[string]*Table // key: schema-qualified table name, e.g. "public.workflow_instances"
	Routines map[string]string // key: schema-qualified routine identity, value: normalized definition text
	Grants   []string          // normalized, sorted grant lines
}

// Table is one table's structural shape.
type Table struct {
	Name        string
	Columns     []Column
	Indexes     []Index
	Constraints []Constraint
	RowSecurity *RowSecurity // nil on every dialect except PostgreSQL
	Policies    []Policy     // empty on every dialect except PostgreSQL
}

// Column is one column's declared shape. Type names and defaults are taken
// verbatim from the database's own catalog (information_schema or its
// dialect equivalent) rather than reformatted, so two snapshots taken the
// same way are comparable without either side normalizing to guess at the
// other's convention.
type Column struct {
	Name     string
	DataType string
	Nullable bool
	Default  string
}

// Index is one index, keyed by name with its full definition text.
type Index struct {
	Name       string
	Definition string
}

// Constraint is one table constraint (check, foreign key, unique, primary
// key), keyed by name with its full definition text.
type Constraint struct {
	Name       string
	Definition string
}

// RowSecurity is PostgreSQL's relrowsecurity/relforcerowsecurity pair.
//
// These are deliberately captured as a distinct field rather than folded into
// a generic property list: a table can carry an identical set of policies and
// differ only in whether FORCE is set, and that difference is invisible to
// any comparison of columns, indexes and policy rows while being exactly the
// difference between a policy that binds the table's owner and one the owner
// silently bypasses. See docs/schema-partitioning-design.md's "Compare
// relrowsecurity and relforcerowsecurity explicitly" and
// TestDiffCatchesADroppedForce below, which is the design doc's second
// mandated known-positive.
type RowSecurity struct {
	Enabled bool // relrowsecurity
	Forced  bool // relforcerowsecurity
}

// Policy is one PostgreSQL row-level security policy.
type Policy struct {
	Name    string
	Command string
	Using   string
	Check   string
}

// schemaMigrationsTable is excluded from every Snapshot. It is the migration
// runner's OWN bootstrap table (migration/runner.go's ensureMigrationsTable),
// created identically by the same Go code regardless of which migration
// files ran -- it is infrastructure the harness's caller shares, not content
// either side of a compaction diff is being asked to verify.
//
// Excluding it is not cosmetic. On SQL Server its inline, unnamed
// `PRIMARY KEY` (migration/runner.go:695-698) gets a server-generated
// constraint and index name -- SQL Server's own auto-naming, which is not
// deterministic across two separate CREATE TABLE calls. Measured 2026-09-25:
// two scratch databases built from the byte-identical migration chain
// produced PK__schema_m__79B5C94C1DA7B4EB on one and
// PK__schema_m__79B5C94CC1648A3B on the other -- a spurious 4-line diff on
// every single run, with nothing wrong in either database. See
// TestSnapshotIsIdenticalForTwoBuildsOfTheSameChainMSSQL, which caught this
// on its first real-database run and is the reason this exclusion exists
// rather than being assumed safe.
const schemaMigrationsTable = "schema_migrations"

// Snapshot builds a Catalog for db under dialect. It only reads -- db must
// already have every migration applied that the caller wants captured.
func Snapshot(ctx context.Context, db *sql.DB, dialect migration.Dialect) (*Catalog, error) {
	switch dialect {
	case migration.DialectPostgres:
		return snapshotPostgres(ctx, db)
	case migration.DialectMySQL:
		return snapshotMySQL(ctx, db)
	case migration.DialectMSSQL:
		return snapshotMSSQL(ctx, db)
	default:
		return nil, fmt.Errorf("catalogdiff: unsupported dialect %q", dialect)
	}
}

// AssertNoPluginObjects returns an error if db shows any trace of a plugin
// migration having run against it.
//
// plugin.RunMigrations creates plugin_migrations lazily -- on its own first
// call (plugin/migration.go:516, createPluginMigrationsTableSQL) -- rather
// than as part of any core schema baseline. So the table's ABSENCE is proof
// nothing plugin-owned ever touched this database, and its presence is proof
// something did. cleat#2306's phase-2 harness
// (cmd/cleat-worker/a_uninstall_down_chain_is_classified_on_every_dialect_test.go)
// relies on the identical fact for the opposite reason -- there, the table's
// existence has to be excluded from a "what did this plugin add" diff; here,
// its existence is exactly the precondition failure being checked for.
//
// This is the check docs/schema-partitioning-design.md calls for under
// "Plugins run against neither side. This is a decision, and the harness
// must assert it rather than assume it" -- run it against BOTH scratch
// databases before trusting any Diff between them, because a plugin table,
// a plugin's pg_policy row, and two pg_class booleans are exactly the kind
// of spurious difference a stray plugin migration would introduce.
func AssertNoPluginObjects(ctx context.Context, db *sql.DB, dialect migration.Dialect) error {
	var query string
	switch dialect {
	case migration.DialectPostgres:
		query = `SELECT to_regclass('plugin_migrations') IS NOT NULL`
	case migration.DialectMySQL:
		query = `SELECT COUNT(*) > 0 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'plugin_migrations'`
	case migration.DialectMSSQL:
		query = `SELECT CASE WHEN OBJECT_ID('plugin_migrations') IS NOT NULL THEN 1 ELSE 0 END`
	default:
		return fmt.Errorf("catalogdiff: unsupported dialect %q", dialect)
	}
	var present bool
	if err := db.QueryRowContext(ctx, query).Scan(&present); err != nil {
		return fmt.Errorf("catalogdiff: checking for plugin_migrations: %w", err)
	}
	if present {
		return fmt.Errorf("catalogdiff: plugin_migrations exists -- a plugin migration ran against this database, which invalidates any core-migration catalog diff taken from it")
	}
	return nil
}

// Diff compares two catalogs of the SAME dialect and returns a sorted,
// human-readable list of differences. An empty slice means the two catalogs
// are structurally identical -- everything this package's Snapshot captures
// matched exactly.
//
// The comparison is deliberately a flat line-set difference rather than a
// field-by-field walk: both catalogs are rendered to a canonical text form
// (one line per fact -- one column, one index, one policy, one routine
// definition), the two line sets are sorted, and the symmetric difference is
// reported with "- only in A" / "+ only in B" prefixes, borrowed from diff's
// own convention. A changed routine body therefore shows as its old line
// removed and its new line added, which is exactly the shape a reviewer
// wants to see and requires no custom "what changed inside this function"
// logic. Two catalogs that differ only in map/slice ORDER produce an empty
// diff, because canonicalize sorts before comparing -- see
// TestSnapshotIsIdenticalForTwoBuildsOfTheSameChain, which is the identity
// check that proves this.
func Diff(a, b *Catalog) []string {
	if a.Dialect != b.Dialect {
		return []string{fmt.Sprintf("- dialect mismatch: %s vs %s", a.Dialect, b.Dialect)}
	}
	linesA := canonicalize(a)
	linesB := canonicalize(b)
	setB := make(map[string]bool, len(linesB))
	for _, l := range linesB {
		setB[l] = true
	}
	setA := make(map[string]bool, len(linesA))
	for _, l := range linesA {
		setA[l] = true
	}
	var diff []string
	for _, l := range linesA {
		if !setB[l] {
			diff = append(diff, "- "+l)
		}
	}
	for _, l := range linesB {
		if !setA[l] {
			diff = append(diff, "+ "+l)
		}
	}
	sort.Strings(diff)
	return diff
}

// canonicalize renders a Catalog to a deterministic, sorted list of one-line
// facts. Two catalogs built from identical schemas produce identical output
// regardless of the order Snapshot's queries happened to return rows in.
func canonicalize(c *Catalog) []string {
	var lines []string

	tableNames := make([]string, 0, len(c.Tables))
	for name := range c.Tables {
		tableNames = append(tableNames, name)
	}
	sort.Strings(tableNames)

	for _, tname := range tableNames {
		t := c.Tables[tname]
		lines = append(lines, fmt.Sprintf("TABLE %s", tname))

		cols := append([]Column(nil), t.Columns...)
		sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
		for _, col := range cols {
			lines = append(lines, fmt.Sprintf("TABLE %s COLUMN %s type=%s nullable=%t default=%s",
				tname, col.Name, col.DataType, col.Nullable, oneLine(col.Default)))
		}

		idxs := append([]Index(nil), t.Indexes...)
		sort.Slice(idxs, func(i, j int) bool { return idxs[i].Name < idxs[j].Name })
		for _, idx := range idxs {
			lines = append(lines, fmt.Sprintf("TABLE %s INDEX %s: %s", tname, idx.Name, oneLine(idx.Definition)))
		}

		cons := append([]Constraint(nil), t.Constraints...)
		sort.Slice(cons, func(i, j int) bool { return cons[i].Name < cons[j].Name })
		for _, con := range cons {
			lines = append(lines, fmt.Sprintf("TABLE %s CONSTRAINT %s: %s", tname, con.Name, oneLine(con.Definition)))
		}

		if t.RowSecurity != nil {
			lines = append(lines, fmt.Sprintf("TABLE %s ROWSECURITY enabled=%t forced=%t",
				tname, t.RowSecurity.Enabled, t.RowSecurity.Forced))
		}

		pols := append([]Policy(nil), t.Policies...)
		sort.Slice(pols, func(i, j int) bool { return pols[i].Name < pols[j].Name })
		for _, p := range pols {
			lines = append(lines, fmt.Sprintf("TABLE %s POLICY %s command=%s using=%s check=%s",
				tname, p.Name, p.Command, oneLine(p.Using), oneLine(p.Check)))
		}
	}

	routineNames := make([]string, 0, len(c.Routines))
	for name := range c.Routines {
		routineNames = append(routineNames, name)
	}
	sort.Strings(routineNames)
	for _, rname := range routineNames {
		lines = append(lines, fmt.Sprintf("ROUTINE %s: %s", rname, oneLine(c.Routines[rname])))
	}

	grants := append([]string(nil), c.Grants...)
	sort.Strings(grants)
	for _, g := range grants {
		lines = append(lines, "GRANT "+oneLine(g))
	}

	sort.Strings(lines)
	return lines
}

// oneLine collapses a possibly multi-line definition (a routine body, an
// index expression) to one line, so it participates correctly in the
// line-set diff above -- a definition that spans multiple lines would
// otherwise be split across several diff lines, some of which could
// coincidentally match the other catalog and mask the real difference.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return s
}
