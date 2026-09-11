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
		"SELECT id FROM plugin_registry WHERE name = $1":                                                                     "cleat#1216",
		"UPDATE plugin_registry SET wasm_bytes = $1, updated_at = now() WHERE name = $2":                                     "cleat#1216",
		"INSERT INTO plugin_registry (name, wasm_bytes, metadata, created_at, updated_at) VALUES ($1, $2, $3, now(), now())": "cleat#1216",
		"SELECT COUNT(*) FROM %s": "a format string: the table name is substituted at the call site, so there is no statement here to parse",
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
