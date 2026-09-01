package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestMSSQLValidatesEveryPostgresJSONBColumn turns a one-off sweep into a
// checked invariant:
//
//	a column PostgreSQL declares JSONB must carry an ISJSON check on SQL Server.
//
// JSONB is where the project actually commits to a value being JSON, so it is
// the right boundary. PostgreSQL stores event_history's request, response,
// signal_payload, child_input, new_input, plugin_input, plugin_output and
// promise_result as plain TEXT -- a service or plugin may legitimately return
// something that is not JSON -- so those must NOT be constrained on SQL Server
// either. This test enforces the rule in one direction only, deliberately: it
// says JSONB implies a check, not that a check implies JSONB.
//
// Why this exists rather than a note in the plan: the gap it closes was six
// columns wide and had been there since the schema was written, and two of them
// (`workflow_instances.plugin_vers`, `allowed_signals`) I did not even notice
// until a script enumerated them. Prose does not survive that.
//
// It reads the migration files rather than a live database, on purpose: those
// files ARE the schema (migration.Runner applies them, engine/testutil applies
// them for tests), and a file-based check needs no DSN, so it runs in every job
// and adds no skips.
//
// PER TABLE, NOT PER COLUMN NAME, and that is the whole subtlety. `payload` and
// `result` each appear on several tables and are checked on some of them, so
// matching `ISJSON(payload)` anywhere in the file reports
// `event_history.payload` as covered when the match actually came from
// `workflow_signals`. Both my first attempt at this audit and the §3.51 note it
// produced got that wrong.
func TestMSSQLValidatesEveryPostgresJSONBColumn(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the migrations directory")
	}
	root := filepath.Dir(filepath.Dir(thisFile)) // engine/ -> repo root

	pgSchema := readFileOrFail(t, filepath.Join(root, "migrations", "postgres", "001_schema.sql"))
	mssqlAll := readMSSQLMigrations(t, filepath.Join(root, "migrations", "mssql"))

	pgTables := splitCreateTableBlocks(pgSchema)
	mssqlTables := splitCreateTableBlocks(mssqlAll)
	if len(pgTables) == 0 || len(mssqlTables) == 0 {
		t.Fatalf("parsed %d PostgreSQL and %d SQL Server tables; the schema layout must have "+
			"changed and this guard is no longer reading it", len(pgTables), len(mssqlTables))
	}

	jsonbCol := regexp.MustCompile(`(?m)^\s+(\w+)\s+JSONB`)

	var missing []string
	checked := 0
	for table, pgBlock := range pgTables {
		mssqlBlock, present := mssqlTables[table]
		if !present {
			// Not every table exists on every dialect; that is a different
			// question and not this test's business.
			continue
		}
		for _, m := range jsonbCol.FindAllStringSubmatch(pgBlock, -1) {
			col := m[1]
			checked++
			if !mssqlHasISJSONCheck(mssqlAll, mssqlBlock, table, col) {
				missing = append(missing, table+"."+col)
			}
		}
	}

	if checked == 0 {
		t.Fatal("found no JSONB columns in the PostgreSQL schema, so this guard is " +
			"measuring nothing -- the parse is broken, not the schema")
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("PostgreSQL declares these JSONB, but SQL Server has no ISJSON check:\n  %s\n\n"+
			"(%d JSONB columns checked.) Add a CHECK in a new migrations/mssql file, "+
			"WITH NOCHECK and tolerating NULL where the PostgreSQL column is nullable -- see "+
			"migrations/mssql/037_json_column_checks.sql. If a column genuinely should not be "+
			"constrained, the fix is to stop declaring it JSONB on PostgreSQL, because that "+
			"declaration is the claim this guard enforces.",
			strings.Join(missing, "\n  "), checked)
	}
}

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// readMSSQLMigrations concatenates every SQL Server migration, because a check
// may be added by a later ALTER TABLE rather than in the original CREATE TABLE
// -- which is how plugin_deps (036) and the six in 037 arrive.
func readMSSQLMigrations(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no .sql files in %s", dir)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(readFileOrFail(t, filepath.Join(dir, n)))
		b.WriteString("\n")
	}
	return b.String()
}

var createTableRe = regexp.MustCompile(`CREATE TABLE(?:\s+IF NOT EXISTS)?\s+(?:dbo\.)?(\w+)`)

// splitCreateTableBlocks maps table name -> the text of its CREATE TABLE
// statement, up to the next CREATE TABLE.
func splitCreateTableBlocks(src string) map[string]string {
	out := map[string]string{}
	locs := createTableRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out[name] = src[loc[0]:end]
	}
	return out
}

var alterTableRe = regexp.MustCompile(`(?s)ALTER TABLE\s+(?:dbo\.)?(\w+)(.*?)(?:;|\z)`)

// mssqlHasISJSONCheck reports whether `table.col` is covered, looking both in
// the table's own CREATE TABLE block and in any ALTER TABLE targeting that
// table. Scoping to the table is the point: `ISJSON(result)` appears four times
// across the schema on four different tables.
func mssqlHasISJSONCheck(allMigrations, tableBlock, table, col string) bool {
	isjson := regexp.MustCompile(`ISJSON\(\s*` + regexp.QuoteMeta(col) + `\s*\)`)
	if isjson.MatchString(tableBlock) {
		return true
	}
	for _, m := range alterTableRe.FindAllStringSubmatch(allMigrations, -1) {
		if m[1] == table && isjson.MatchString(m[2]) {
			return true
		}
	}
	return false
}
