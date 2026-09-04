package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// The database a test runs against must hold the LATEST definition of every
// routine the migrations ship, not merely some definition of it.
//
// IMPROVEMENT-PLAN §3.300 row B5, the highest-ranked unguarded boundary in the
// C1 inventory.
//
// # The gap
//
// store_backends_procedures_test.go declares, per dialect, which migration
// files define the stored routines, and applies exactly those:
//
//	var postgresProcedureMigrations = []string{"003_procedures.sql",
//	    "004_fix_finalize_workflow_status_fence.sql"}
//
// Three hardcoded lists of two. Measured 2026-09-04, eight PostgreSQL
// migrations define a routine and three MySQL ones do. The omission that
// matters is 040_claim_terminating_workflows.sql, which DROPs and re-CREATEs
// admin.claim_workflows with an extra return column, superseding 023.
//
// That is §1.1's trap exactly -- for anything defined by CREATE OR REPLACE,
// find the highest-numbered migration that defines it -- with the list frozen
// two migrations after the schema stopped agreeing with it.
//
// # Why this is a guard and not a fix to that list
//
// The list is not the invariant. What matters is what is IN the database, and
// the database is built by an external migration run over the whole directory,
// not by that list. On this checkout the routine is correct; the list simply
// did not determine the answer. On a database that was not recreated after a
// migration landed, the same list installs the superseded routine and every
// test exercising it silently measures the wrong definition -- which is the
// failure CLAUDE.md already warns about ("when a schema migration lands,
// recreate your test databases") with nothing attached to it.
//
// So this asserts the property rather than the bookkeeping: parse both sides,
// declare nothing. Same shape as §2.71's mssql_policy_coverage_test.go.
//
// # Why SetupFullSchema does not already cover this
//
// It calls the real migration runner over the whole directory, which is why CI's
// database has every routine. But the runner records applied versions in
// schema_migrations and SKIPS anything already recorded -- so once a version is
// marked applied, no later run re-reads that file. A database carrying an older
// definition therefore keeps it forever, and calling SetupFullSchema against it
// is a no-op that reports success.
//
// That is not a hypothesis. Falsifying this test meant installing 023's
// admin.claim_workflows over 040's; SetupFullSchema ran, repaired nothing, and
// the assertions below were what noticed. A guard whose failure the setup path
// silently fixes would prove nothing, and this one demonstrably is not that.
//
// # How the check discriminates
//
// A text comparison between a .sql file and a database's rendering of the same
// routine cannot work -- every engine normalises. So for each routine defined
// more than once, the DISCRIMINATOR is derived: the identifiers that appear in
// the latest definition and in none of the earlier ones. If the database holds
// the latest definition those identifiers are present; if it holds a superseded
// one they are not.
//
// Measured 2026-09-04, comments stripped:
//
//	postgres admin.claim_workflows  023 -> 040:  pending_terminal_status, terminating
//	postgres finalize_workflow_status 003 -> 004: v_rows_updated, ROW_COUNT, DIAGNOSTICS
//	mysql    finalize_workflow_status 003 -> 004: fence_held, v_rows_updated, ROW_COUNT
//	mssql    finalize_workflow_status 003 -> 004: fence_held, rows_updated, ROWCOUNT
//
// # Stripping comments is load-bearing, not tidying
//
// The first version of the discriminator did not strip SQL comments, and the
// PostgreSQL 003->004 set came back as 60-odd English words -- "behalf",
// "actually", "regardless" -- because 004's rationale comment is prose and 003's
// is shorter. The check would then have asserted that the database's copy of the
// routine contains the word "behalf", which for PostgreSQL it does (comments
// inside a $$ body survive into pg_get_functiondef) and for MySQL it does not.
// A discriminator that is really a prose diff passes for a reason unrelated to
// the property, and it would have been the reason on one dialect out of three.
var routineDefRe = map[testutil.Dialect]*regexp.Regexp{
	testutil.DialectPostgres: regexp.MustCompile(
		`(?im)^CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+([A-Za-z_][\w.]*)\s*\(`),
	testutil.DialectMySQL: regexp.MustCompile(
		`(?im)^CREATE\s+(?:OR\s+REPLACE\s+)?PROCEDURE\s+([A-Za-z_][\w.]*)\s*\(`),
	testutil.DialectMSSQL: regexp.MustCompile(
		`(?im)^CREATE\s+(?:OR\s+ALTER\s+)?(?:FUNCTION|PROCEDURE)\s+([A-Za-z_][\w.]*)`),
}

// dropRoutineRe finds a routine a migration creates and then destroys again in
// the same file. mysql/034 does exactly this with cleat_drop_defs_fks(): a
// one-shot helper, created, called, dropped. It is legitimately absent from the
// built database, and asserting its presence would be a false failure.
//
// Checked rather than assumed before this exemption was written:
// information_schema.ROUTINES for the MySQL test database lists only
// finalize_workflow_status.
var dropRoutineRe = regexp.MustCompile(`(?im)^\s*DROP\s+(?:FUNCTION|PROCEDURE)\s+(?:IF\s+EXISTS\s+)?([A-Za-z_][\w.]*)`)

// Comment stripping reuses mssql_uuid_projection_test.go's stripSQLComments
// rather than adding a second one. That matters beyond tidiness: it blanks
// comments in place, preserving byte offsets AND newlines. The regex version
// written first here replaced a /* */ block with a single space, which collapses
// the newlines inside it -- and every routine-locating pattern below is anchored
// with (?im)^, so a collapsed block silently welds the next CREATE onto the end
// of a comment line and the anchor stops matching it.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)

func identsIn(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range identRe.FindAllString(stripSQLComments(s), -1) {
		out[m] = true
	}
	return out
}

// normalizedLines reduces a routine's text to comparable lines: comments
// stripped, lowercased, internal whitespace collapsed, blanks dropped.
//
// This is the SECOND discriminator, and it exists because the identifier one is
// structurally blind to a class of real change. cleat.assert_tenant_set goes
// from `IF tid IS NULL THEN` (001) to a version that also rejects the empty
// string, `IF tid IS NULL OR tid = <empty-string-literal> THEN` (034) -- the fix
// §3.300 names. (Spelled out rather than quoted: the two adjacent single quotes
// SQL uses for an empty string were silently rewritten to a Unicode right double
// quote here once already, which turned the sentence into a claim about a
// character SQL has no use for.) Every token in the new version appears in the
// old one, so an identifier-set difference is EMPTY for a change that alters
// what the function accepts. No threshold tuning rescues that; `tid` is not
// discriminating at any length because both versions contain it.
//
// Line comparison is confined to the body, never the signature: PostgreSQL
// stores a function's body verbatim (pg_proc.prosrc) but RECONSTRUCTS the header
// in pg_get_functiondef from the catalog, so signature lines legitimately differ
// in whitespace and type spelling and would produce false failures.
func normalizedLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(stripSQLComments(s), "\n") {
		ln = strings.ToLower(strings.Join(strings.Fields(ln), " "))
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// bodyStartRe finds where a routine's body begins, so line comparison can skip
// the reconstructed signature. See normalizedLines.
var bodyStartRe = regexp.MustCompile(`(?is)(\$[A-Za-z_]*\$|\bBEGIN\b|\bAS\b)`)

func routineBody(s string) string {
	if m := bodyStartRe.FindStringIndex(s); m != nil {
		return s[m[1]:]
	}
	return s
}

// routineDefinition is one CREATE of one routine, in one migration file.
type routineDefinition struct {
	migration string // e.g. "040_claim_terminating_workflows.sql"
	name      string // as written: "admin.claim_workflows", "finalize_workflow_status"
	body      string // the CREATE statement's text, up to the next top-level statement
}

// routineEndRe marks where a routine's definition text stops, per dialect, read
// off the migrations rather than assumed:
//
//	postgres  $$ LANGUAGE plpgsql;   (migrations/postgres/004:146)
//	mysql     END //                 (migrations/mysql/004:134)
//	mssql     END;                   (migrations/mssql/004:153)
//
// Scoping the body matters in both directions, and the first attempt got both
// wrong.
//
// Too WIDE and unrelated statements contaminate the diff: an unscoped whole-file
// comparison put 040's eight `CREATE INDEX` names into admin.claim_workflows'
// discriminator set, and index names never appear in pg_get_functiondef, so
// every one would have been a guaranteed false failure.
//
// Too NARROW and the body vanishes. The first version cut at a generic
// column-zero statement keyword whose list included `BEGIN` -- which is how a
// routine body STARTS. Every body was truncated to its signature, so the only
// supersession that still had a discriminator was admin.claim_workflows, whose
// return type changed; the other four reported "no identifier the earlier ones
// lack" and the test failed loudly. That is the anti-vacuity arm doing its job:
// the broken version could not have passed silently.
var routineEndRe = map[testutil.Dialect]*regexp.Regexp{
	// A closing dollar-quote, followed by either LANGUAGE or just `;`. Both
	// spellings ship: 004 ends `$$ LANGUAGE plpgsql;` while 040 ends `$$;`
	// because it declares `LANGUAGE sql` in the signature instead. Requiring
	// LANGUAGE after the quote missed 040 entirely, so its body ran on through
	// ALTER/REVOKE/GRANT into eight CREATE INDEX statements and the index names
	// became discriminators -- which pg_get_functiondef can never contain.
	testutil.DialectPostgres: regexp.MustCompile(`(?im)^\s*\$[A-Za-z_]*\$\s*(?:LANGUAGE\b[^\n]*|;)`),
	testutil.DialectMySQL:    regexp.MustCompile(`(?im)^END\s*//`),
	testutil.DialectMSSQL:    regexp.MustCompile(`(?im)^(?:END;?|GO)\s*$`),
}

// parseRoutineDefinitions reads migrations/<dialect>/*.sql in filename order and
// returns every routine definition it finds, in the order the migrations apply.
func parseRoutineDefinitions(t *testing.T, dialect testutil.Dialect, dir string) []routineDefinition {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	// Filename order is apply order: every migration here is zero-padded and
	// numerically prefixed, so a lexical sort is the numeric one.
	sort.Strings(names)

	re := routineDefRe[dialect]
	var defs []routineDefinition
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("reading %s: %v", n, err)
		}
		// Comments are stripped before locating CREATEs as well as before
		// diffing: migrations/mssql/001_schema.sql explains in a comment why
		// `CREATE OR ALTER FUNCTION on fn_tenant_filter` fails, and an
		// unstripped scan reads that sentence as a definition.
		src := stripSQLComments(string(b))
		locs := re.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			name := src[loc[2]:loc[3]]
			// The body runs to the next top-level statement AFTER this CREATE,
			// or to the next CREATE of a routine, or to end of file.
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			if m := routineEndRe[dialect].FindStringIndex(src[loc[1]:end]); m != nil {
				end = loc[1] + m[1] // through the terminator, not up to it
			}
			defs = append(defs, routineDefinition{migration: n, name: name, body: src[loc[0]:end]})
		}
	}
	return defs
}

// routinesDroppedInSameFile returns the routines a migration creates and then
// drops again, keyed by "migration\x00name".
func routinesDroppedInSameFile(t *testing.T, dir string, defs []routineDefinition) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	byFile := map[string][]routineDefinition{}
	for _, d := range defs {
		byFile[d.migration] = append(byFile[d.migration], d)
	}
	for file, fileDefs := range byFile {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		src := stripSQLComments(string(b))
		for _, d := range fileDefs {
			create := strings.Index(src, d.body[:min(len(d.body), 60)])
			for _, m := range dropRoutineRe.FindAllStringSubmatchIndex(src, -1) {
				dropped := src[m[2]:m[3]]
				// Only a DROP that comes AFTER the CREATE destroys it. A
				// drop-then-recreate (the shipped idiom for 040) is the
				// opposite case and must not be read as a teardown.
				if bareName(dropped) == bareName(d.name) && m[0] > create {
					out[file+"\x00"+d.name] = true
				}
			}
		}
	}
	return out
}

func bareName(qualified string) string {
	if i := strings.LastIndex(qualified, "."); i >= 0 {
		return qualified[i+1:]
	}
	return qualified
}

func schemaOf(qualified, def string) string {
	if i := strings.LastIndex(qualified, "."); i >= 0 {
		return qualified[:i]
	}
	return def
}

// databaseRoutineText returns the database's own rendering of a routine's
// definition, and whether it exists at all.
func databaseRoutineText(t *testing.T, db *sql.DB, dialect testutil.Dialect, qualified string) (string, bool) {
	t.Helper()
	var q, schema, name string
	name = bareName(qualified)
	switch dialect {
	case testutil.DialectPostgres:
		schema = schemaOf(qualified, "public")
		// string_agg over overloads: 023 and 040 differ in return type, and a
		// stale database can hold both. Concatenating means the discriminator
		// check below asks "is the latest definition present", not "is the only
		// definition the latest" -- the weaker question, and the right one,
		// because an extra overload is a separate finding this test does not own.
		q = `SELECT COALESCE(string_agg(pg_get_functiondef(p.oid), E'\n'), '')
		     FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		     WHERE n.nspname = $1 AND p.proname = $2`
	case testutil.DialectMySQL:
		// ROUTINE_DEFINITION is the body only; parameters are in
		// information_schema.PARAMETERS. Every MySQL discriminator measured
		// (fence_held, v_rows_updated, ROW_COUNT) is declared in the body, so
		// the body alone carries the signal -- but see the anti-vacuity guard,
		// which fails rather than passes if that ever stops being true.
		q = `SELECT COALESCE(ROUTINE_DEFINITION, '') FROM information_schema.ROUTINES
		     WHERE ROUTINE_SCHEMA = DATABASE() AND ROUTINE_NAME = ?`
	case testutil.DialectMSSQL:
		schema = schemaOf(qualified, "dbo")
		q = `SELECT COALESCE(m.definition, '') FROM sys.sql_modules m
		     JOIN sys.objects o ON o.object_id = m.object_id
		     JOIN sys.schemas s ON s.schema_id = o.schema_id
		     WHERE s.name = @p1 AND o.name = @p2`
	}

	var row *sql.Row
	if dialect == testutil.DialectMySQL {
		row = db.QueryRow(q, name)
	} else {
		row = db.QueryRow(q, schema, name)
	}
	var text string
	switch err := row.Scan(&text); {
	case err == sql.ErrNoRows:
		return "", false
	case err != nil:
		t.Fatalf("querying definition of %s: %v", qualified, err)
	}
	return text, strings.TrimSpace(text) != ""
}

func TestTheDatabaseHasTheLatestDefinitionOfEveryRoutineTheMigrationsShip(t *testing.T) {
	for _, d := range []struct {
		dialect testutil.Dialect
		dir     string
		// minRoutines is an anti-vacuity floor, not a target. Deliberately
		// loose so adding a routine does not fail here, deliberately non-zero
		// so losing the parse does. Measured 2026-09-04: postgres 9 distinct,
		// mysql 2, mssql 2.
		minRoutines int
	}{
		{testutil.DialectPostgres, filepath.Join("..", "migrations", "postgres"), 5},
		{testutil.DialectMySQL, filepath.Join("..", "migrations", "mysql"), 1},
		{testutil.DialectMSSQL, filepath.Join("..", "migrations", "mssql"), 2},
	} {
		t.Run(string(d.dialect), func(t *testing.T) {
			defs := parseRoutineDefinitions(t, d.dialect, d.dir)
			byName := map[string][]routineDefinition{}
			var order []string
			for _, def := range defs {
				if _, seen := byName[def.name]; !seen {
					order = append(order, def.name)
				}
				byName[def.name] = append(byName[def.name], def)
			}
			if len(byName) < d.minRoutines {
				t.Fatalf("parsed only %d routines out of %s; there were at least %d on "+
					"2026-09-04.\n\nA parse that matches almost nothing passes vacuously, "+
					"so this is a failure: re-point routineDefRe[%s] rather than lowering "+
					"this floor.", len(byName), d.dir, d.minRoutines, d.dialect)
			}

			// The supersession cases are the whole point of the test. If none
			// is found the discriminator arm below checks nothing at all, and
			// a test that checks nothing must say so rather than pass.
			superseded := 0
			for _, defList := range byName {
				if len(defList) > 1 {
					superseded++
				}
			}
			if superseded == 0 {
				t.Fatalf("no routine in %s is defined more than once, so the "+
					"latest-definition check has nothing to discriminate.\n\n"+
					"On 2026-09-04 every dialect had at least one "+
					"(finalize_workflow_status, 003 -> 004). Either the parse "+
					"broke or the migrations changed shape; do not delete this "+
					"guard to make it pass.", d.dir)
			}

			dropped := routinesDroppedInSameFile(t, d.dir, defs)
			db := testutil.TestDB(t, d.dialect)
			testutil.SetupFullSchema(t, db, d.dialect)

			checkedDiscriminators := 0
			for _, name := range order {
				defList := byName[name]
				latest := defList[len(defList)-1]
				if dropped[latest.migration+"\x00"+name] {
					continue // created and destroyed in the same migration
				}

				text, exists := databaseRoutineText(t, db, d.dialect, name)
				if !exists {
					t.Errorf("%s defines routine %s (latest in %s) and the built database "+
						"does not have it.\n\nThis is §3.300's B5 row firing: the "+
						"migrations and the database disagree about what exists.",
						d.dir, name, latest.migration)
					continue
				}
				if len(defList) == 1 {
					continue
				}

				// Identifiers the latest definition introduces and no earlier
				// one has. Derived, never declared -- a hardcoded token would
				// be a third copy of something the migrations already state.
				earlier := map[string]bool{}
				for _, prev := range defList[:len(defList)-1] {
					for id := range identsIn(prev.body) {
						earlier[id] = true
					}
				}
				var discriminators []string
				for id := range identsIn(latest.body) {
					if !earlier[id] {
						discriminators = append(discriminators, id)
					}
				}
				sort.Strings(discriminators)

				// The second discriminator: body lines the latest version has
				// and no earlier one does. Catches the changes that reuse every
				// existing token -- see normalizedLines.
				earlierLines := map[string]bool{}
				for _, prev := range defList[:len(defList)-1] {
					for _, ln := range normalizedLines(routineBody(prev.body)) {
						earlierLines[ln] = true
					}
				}
				var newLines []string
				for _, ln := range normalizedLines(routineBody(latest.body)) {
					if !earlierLines[ln] {
						newLines = append(newLines, ln)
					}
				}

				if len(discriminators) == 0 && len(newLines) == 0 {
					t.Errorf("%s is defined in %d migrations (latest %s) and the latest "+
						"introduces neither a new identifier nor a new body line, so "+
						"nothing here can tell the versions apart.\n\nThat is a hole in "+
						"this test, not a property of the schema: it means a stale "+
						"database would pass. Widen the discriminator rather than "+
						"skipping the routine.", name, len(defList), latest.migration)
					continue
				}

				dbLines := map[string]bool{}
				for _, ln := range normalizedLines(text) {
					dbLines[ln] = true
				}
				lower := strings.ToLower(text)

				var absent []string
				for _, id := range discriminators {
					if !strings.Contains(lower, strings.ToLower(id)) {
						absent = append(absent, id)
					}
				}
				// Line evidence is satisfied by ANY new line being present, not
				// all of them: the database's rendering may differ from the file
				// in ways that survive normalisation, and this arm exists to
				// distinguish two VERSIONS, not to diff two texts. One line only
				// the latest version has is sufficient to establish which one is
				// installed.
				lineEvidence := len(newLines) == 0
				for _, ln := range newLines {
					if dbLines[ln] {
						lineEvidence = true
						break
					}
				}

				checkedDiscriminators++
				if len(absent) > 0 || !lineEvidence {
					t.Errorf("the database's %s is not the definition %s ships.\n\n"+
						"%s is the latest of %d migrations defining it. New identifiers "+
						"%v (missing from the database: %v); new body lines %d, of which "+
						"the database has %v.\n\n"+
						"Almost certainly a test database that predates that migration: "+
						"CREATE TABLE IF NOT EXISTS never adds a column and a stale "+
						"routine is the same failure one layer down. Drop and recreate "+
						"the database rather than debugging the code. Note that "+
						"store_backends_procedures_test.go's hardcoded list will NOT "+
						"repair this -- it applies %v and nothing else.",
						name, d.dir, latest.migration, len(defList),
						discriminators, absent, len(newLines), lineEvidence,
						procedureMigrationsFor(d.dialect))
				}
			}

			if checkedDiscriminators == 0 {
				t.Errorf("no routine reached the discriminator comparison, so this "+
					"dialect's arm asserted only existence. %d routines parsed, %d "+
					"superseded -- if those disagree the drop-detection or the "+
					"database query is swallowing the cases.", len(byName), superseded)
			}
			if !t.Failed() {
				t.Logf("%s: %d routines, %d with a superseding definition, %d checked "+
					"against the database's own copy", d.dialect, len(byName), superseded,
					checkedDiscriminators)
			}
		})
	}
}

// procedureMigrationsFor names the hardcoded list in
// store_backends_procedures_test.go for this dialect, so a failure message can
// say why re-running that helper will not fix it.
func procedureMigrationsFor(d testutil.Dialect) []string {
	switch d {
	case testutil.DialectPostgres:
		return postgresProcedureMigrations
	case testutil.DialectMySQL:
		return mysqlProcedureMigrations
	case testutil.DialectMSSQL:
		return mssqlProcedureMigrations
	}
	return nil
}
