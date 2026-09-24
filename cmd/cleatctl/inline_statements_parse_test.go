package main

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// Every SQL statement written inline in this package must PARSE against a real
// PostgreSQL with the schema applied.
//
// cleat#1208. loadWorkflowInstance selected min_version and error from
// workflow_instances. min_version belongs to workflow_defs and the column is
// error_msg, so `cleatctl replay` and `cleatctl debug` both failed at their
// first query -- on every role, every database, since the statement was
// written. Nothing was broken by a schema change; the names had never been
// right.
//
// The package's tests could not see it. replay_test.go drives a hand-written
// driver.Conn whose Prepare DISCARDS the sql:
//
//	func (c *mockReplayConn) Prepare(_ string) (driver.Stmt, error) {
//	        return &mockReplayStmt{notFound: c.notFound}, nil
//	}
//
// so it returns the same canned row for any statement, including one naming
// columns that do not exist. Same shape as the fake-driver problem
// plugins/*_dialect_arms_multidb_test.go was built for: the fake pattern-matches
// the query string, so it accepts SQL no database would. cmd/ had no equivalent.
//
// PREPARE is the right check and not a compromise. It resolves every table and
// column name and type-checks the expression tree WITHOUT executing anything, so
// it needs no fixtures, no rows and no tenant context -- which matters here
// because the tables it names are RLS-scoped and a SELECT against them would
// raise before it could tell you anything about the SQL.
func TestEveryInlineStatementParsesOnPostgres(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	ctx := context.Background()

	stmts := inlineStatements(t)
	if len(stmts) < 15 {
		t.Fatalf("found only %d inline statements in this package; there were 25 on "+
			"2026-09-11.\n\n"+
			"A floor, not a count -- the population grows. This fires when the EXTRACTION "+
			"breaks, which is the failure that would leave this test passing while checking "+
			"nothing.", len(stmts))
	}

	// pinned maps a NORMALIZED statement to why it is not expected to parse.
	//
	// Keyed on the statement rather than on file:line, because a line number is
	// invalidated by any edit above it -- including the edits this test exists
	// to provoke -- and a stale key silently stops matching, which lands on the
	// permissive side.
	//
	// It fails in BOTH directions: a statement that fails and is not pinned,
	// and a pin that no longer corresponds to anything. That is the tier-2 rule
	// from tiers.yaml -- a tracked list of known failures that can only shrink
	// -- applied to one package. A list that is not required to shrink is an
	// allowlist, and an allowlist is how a guard stops guarding.
	pinned := map[string]string{
		"SELECT COUNT(*) FROM %s": "a format string: the table name is substituted at the call site, so there is no statement here to parse",

		// check-db's two RUNTIME counts, assembled because the database they
		// read is not known until the tenant is (cleat#1956's audit). On MySQL
		// a tenant's per-tenant tables live in cleat_<tenant-id>, a different
		// physical database from the one --db names, so the qualifier is built
		// by dialect.tenantRuntimeQualifier and prefixed here; off MySQL it is
		// empty and these are exactly the statements they were before.
		//
		// The coverage lost to the pin is bought back where it means something
		// rather than waived: TestCheckDBCountsTheTenantsDatabaseOnMySQL issues
		// both of these ASSEMBLED, against a real MySQL with the schema applied
		// and a row in the tenant database, and fails if the count comes back
		// from the base one. A PREPARE here could only have told us the
		// PostgreSQL form parses, which is the form that never had the bug.
		"SELECT status, COUNT(*) AS cnt FROM %sworkflow_instances GROUP BY status ORDER BY status": "a format string: the database qualifier is substituted at the call site (empty off MySQL), so there is no statement here to parse. Exercised assembled by TestCheckDBCountsTheTenantsDatabaseOnMySQL",
		"SELECT COUNT(*) FROM %sevent_history":                                                     "a format string, as the statement above; same substitution and same assembled coverage",

		// reseal-payloads (cleat#1794) builds both of its statements by
		// concatenating engine.EncryptedEventColumns, so what is in the source
		// is a PREFIX and not a statement. Pinned for the same reason as the
		// format string above: there is nothing here to parse.
		//
		// The coverage is not lost, which is the part that matters --
		// TestResealBindsLegacyCiphertextAndPreservesThePlaintext issues both
		// of these, fully assembled, against a real database with the schema
		// applied, so a wrong column name fails there instead of here. Writing
		// the eleven columns out as a literal to satisfy this test would
		// duplicate the list that TestEncryptedEventColumnsIsComplete exists to
		// keep single.
		"UPDATE event_history SET":                   "a prefix: the SET list is built from engine.EncryptedEventColumns, so there is no statement here to parse. Exercised assembled by TestResealBindsLegacyCiphertextAndPreservesThePlaintext",
		"SELECT workflow_id, step, tenant_id::text,": "a prefix: the column list is built from engine.EncryptedEventColumns. Same coverage note as the UPDATE above",

		// Both are SQL Server-only, from drop-tenant's port in cleat#1635.
		//
		// Pinned rather than written as a plugin.Query MSSQL arm -- which is
		// what the pruning above would recognise -- because Query.Default is
		// required and is PostgreSQL. There is no PostgreSQL statement these
		// are the SQL Server arm OF: the whole point of the first one is that
		// it asks sys.columns, and PostgreSQL's answer to the same question
		// comes from a different catalogue with different column names. A
		// Default written only to satisfy the struct would be dead code that
		// reads like a supported path.
		"SELECT s.name, t.name FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id WHERE EXISTS (SELECT 1 FROM sys.columns c WHERE c.object_id = t.object_id AND c.name = 'tenant_id') ORDER BY s.name, t.name": "SQL Server catalogue views; cleat#1635. PostgreSQL has no sys.tables",
		"SELECT count(*) FROM %s.%s WHERE tenant_id = @p1": "a format string, and SQL Server-only: the schema and table come from the row above, so there is no statement here to parse. cleat#1635",

		// T-SQL, and it is REACHED only on SQL Server: rlsPostureOf dispatches
		// on the dialect before this runs, so PostgreSQL never issues it.
		// Pinned rather than rewritten because there is nothing to rewrite --
		// IS_ROLEMEMBER has no PostgreSQL equivalent, which is the whole
		// reason cleat#1646 exists. The PostgreSQL arm answers the same
		// question with pg_roles and is checked by this test as before.
		"SELECT IS_ROLEMEMBER('cleat_admin')": "SQL Server built-in: reached only on the mssql arm of rlsPostureOf, and there is no PostgreSQL equivalent to write instead (cleat#1646)",

		// cleat#1918. queue.go:100 is `case "update":` in runQueue's dispatch
		// switch, naming the new `queue update` subcommand. The verb regex
		// matches on content, not on syntactic position, so a bare case label
		// that happens to start with a SQL verb word is indistinguishable from
		// a statement to the AST walk -- there is no SQL here at all, just a
		// five-letter subcommand name that collides with the UPDATE keyword.
		"update": "cmd/cleatctl/queue.go's `case \"update\":` switch label for the `queue update` subcommand; matches the verb regex by coincidence of spelling, not because it is SQL",

		// cleat#2046. tenant_quota is a PLUGIN table (plugins/tenantquota/
		// migrations.go), not part of the core schema testutil.TestDB applies
		// here -- unlike tenant_settings, tenant_egress_allow and
		// tenant_secrets, which this package also writes inline SQL against
		// and which ARE core tables, so their statements are checked by this
		// test as written. PREPAREing any of the five below against this
		// test's db fails with "relation tenant_quota does not exist", which
		// is a property of this test's fixture, not of the SQL.
		//
		// The coverage is not lost: TestQuotaCommandWorksOnEveryDialect runs
		// every one of these five, unmodified, through plugin.RunMigrations
		// (the same call cmd/cleat-worker uses at boot) against real
		// Postgres, MySQL and SQL Server databases -- a stronger check than
		// this test's PREPARE-only one, and the only one of the three
		// dialects this test could have run against anyway.
		"SELECT limit_count, window_seconds, enforce, updated_at FROM tenant_quota WHERE tenant_id = $1 AND resource = $2":                                        "tenant_quota is a plugin table, not in this test's core schema; checked live by TestQuotaCommandWorksOnEveryDialect (cleat#2046)",
		"INSERT INTO tenant_quota (tenant_id, resource, limit_count, window_seconds, enforce, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7)":        "tenant_quota is a plugin table, not in this test's core schema; checked live by TestQuotaCommandWorksOnEveryDialect (cleat#2046)",
		"UPDATE tenant_quota SET limit_count = $1, window_seconds = $2, enforce = $3, updated_at = $4 WHERE tenant_id = $5 AND resource = $6 AND updated_at = $7": "tenant_quota is a plugin table, not in this test's core schema; checked live by TestQuotaCommandWorksOnEveryDialect (cleat#2046)",
		"SELECT tenant_id, resource, limit_count, window_seconds, enforce, updated_at FROM tenant_quota WHERE tenant_id = $1 ORDER BY resource":                   "tenant_quota is a plugin table, not in this test's core schema; checked live by TestQuotaCommandWorksOnEveryDialect (cleat#2046)",
		"SELECT tenant_id, resource, limit_count, window_seconds, enforce, updated_at FROM tenant_quota ORDER BY tenant_id, resource":                             "tenant_quota is a plugin table, not in this test's core schema; checked live by TestQuotaCommandWorksOnEveryDialect (cleat#2046)",
	}

	// A template is not checkable as written, and saying so out loud is the
	// point: silently skipping it would shrink the population this test covers
	// without shrinking the number it reports, which is how a check comes to
	// measure less than it claims.
	template := regexp.MustCompile(`%[a-zA-Z]`)

	seen := map[string]bool{}
	var unexpected []string
	for _, st := range stmts {
		norm := normalizeSQL(st.sql)
		if template.MatchString(norm) {
			if _, ok := pinned[norm]; !ok {
				unexpected = append(unexpected, fmt.Sprintf(
					"%s\n      not checkable: contains a format verb, and is not pinned\n      %q",
					st.where, norm))
				continue
			}
			seen[norm] = true
			continue
		}
		err := preparses(ctx, db, st.sql)
		if err == nil {
			if _, ok := pinned[norm]; ok {
				seen[norm] = true // recorded; reported as a fixed pin below
			}
			continue
		}
		if _, ok := pinned[norm]; ok {
			seen[norm] = true
			continue
		}
		unexpected = append(unexpected, fmt.Sprintf("%s\n      %v\n      %q", st.where, err, norm))
	}

	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Errorf("%d inline statement(s) do not parse against the schema:\n\n  %s\n\n"+
			"PREPARE resolves every table and column name without executing, so this is a "+
			"statement that CANNOT RUN -- not a missing fixture, not a permissions problem, "+
			"and not a tenant-context error. The mock drivers in this package accept it "+
			"happily; a database will not.\n\n"+
			"If it is genuinely expected to fail, add the quoted normalized form to `pinned` "+
			"with the issue tracking it.",
			len(unexpected), strings.Join(unexpected, "\n  "))
	}

	// Now the other direction.
	var stale []string
	for norm, why := range pinned {
		if !seen[norm] {
			stale = append(stale, fmt.Sprintf("%q\n      pinned as %s, and no statement in the "+
				"package matches it", norm, why))
			continue
		}
		if !template.MatchString(norm) && preparses(ctx, db, norm) == nil {
			stale = append(stale, fmt.Sprintf("%q\n      pinned as %s, and it now parses", norm, why))
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%d pin(s) no longer describe the tree:\n\n  %s\n\n"+
			"Delete them, and close the issue if that was the last one. A pin that outlives "+
			"its defect is an exclusion, and the next real failure at the same statement "+
			"would be waved through by it.",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// normalizeSQL collapses whitespace so a pin survives reindentation. It does
// nothing else -- a pin should stop matching when the STATEMENT changes, which
// is the moment to re-derive whether it still fails.
func normalizeSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

// preparses asks PostgreSQL to parse and plan the statement without running it.
//
// Pinned to a single connection, and the DEALLOCATE is not tidiness: a PREPARE
// name lives for the SESSION, and this runs against a pooled *sql.DB, so a
// reused connection would fail the next check with "prepared statement already
// exists" -- a failure of this test's bookkeeping, reported as a failure of the
// SQL under test.
func preparses(ctx context.Context, db *sql.DB, stmt string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	stmtSeq++
	name := fmt.Sprintf("cleatctl_parse_check_%d", stmtSeq)
	if _, err := conn.ExecContext(ctx, "PREPARE "+name+" AS "+stmt); err != nil {
		// 42P18, indeterminate_datatype. Every table and column name resolved;
		// PostgreSQL simply cannot infer what type a bare $1 should be in this
		// position. That is a property of preparing a statement outside its
		// call site, not a defect in the statement -- `SELECT admin.drop_tenant($1)`
		// is the case in this package -- so it is not counted as a failure.
		//
		// Deliberately narrow: it matches this one message rather than the
		// SQLSTATE class, because widening it is how a check stops checking.
		if strings.Contains(err.Error(), "could not determine data type of parameter") {
			return nil
		}
		return err
	}
	_, _ = conn.ExecContext(ctx, "DEALLOCATE "+name)
	return nil
}

var stmtSeq int

// inlineStatement is one backtick literal that looks like SQL.
type inlineStatement struct {
	where string // path:line
	sql   string
}

// inlineStatements extracts them with go/ast rather than a regex.
//
// The regex version of this is where the repo's most expensive scanning bug
// lives, and it is worth naming because it looks harmless: a LENGTH BOUND
// applied while pairing delimiters re-phases the scan. A pattern that pairs
// backticks and bounds the contents to 10-600 characters in the SAME expression
// rejects a long literal and resumes INSIDE it, so its closing backtick pairs
// with the next literal's opening one and the Go source between them is
// returned as string data. Measured on this very file's neighbour, cmd/cleatctl/replay.go:
// one match either way, 225 characters of `)\n}\n\n// loadWorkflowInstance ...`
// instead of the 303-character SELECT. Same count, unrelated contents.
//
// go/ast does not have the problem to avoid: a BasicLit is a token, not a
// region between delimiters. The SQL filter is applied AFTER extraction, which
// is the same "pair first, filter after" rule one level up.
func inlineStatements(t *testing.T) []inlineStatement {
	t.Helper()

	root := cleatctlRepoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard", "cmd/cleatctl/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	verb := regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|WITH)\b`)
	fset := token.NewFileSet()

	var stmts []inlineStatement
	for _, rel := range strings.Fields(string(out)) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// A plugin.Query's MySQL and MSSQL arms are never sent to
			// PostgreSQL, so PREPAREing them here asks the wrong database a
			// question it cannot answer: `SELECT TOP 1` is a syntax error on
			// PostgreSQL and correct on SQL Server, and CONVERT(NVARCHAR(36),
			// ...) likewise. Four such arms arrived with cleat#1316.
			//
			// Pruned by KEY rather than by recognising the SQL, because the
			// alternative is a heuristic that has to know every construct the
			// other two dialects have -- a list that can only be incomplete,
			// and whose incompleteness shows up as a confident failure about a
			// statement that is correct.
			//
			// This narrows what the guard covers and the narrowing is stated
			// rather than hidden: the Default arm is still PREPAREd, so the
			// statements this package actually sends to PostgreSQL are still
			// checked, and the other arms are covered structurally by
			// TestEveryDialectArmBindsItsOwnPlaceholders. Nothing yet PREPAREs
			// them against a MySQL or SQL Server instance; that is a real gap
			// and it is smaller than the one it replaces.
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok &&
					(key.Name == "MySQL" || key.Name == "MSSQL") {
					return false
				}
			}

			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			body, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if !verb.MatchString(body) {
				return true
			}
			stmts = append(stmts, inlineStatement{
				where: fmt.Sprintf("%s:%d", rel, fset.Position(lit.Pos()).Line),
				sql:   body,
			})
			return true
		})
	}
	return stmts
}

func cleatctlRepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}
