package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A plugin.Query arm must be valid for the dialect it names.
//
// cleat#1133. Plugin queries failed at runtime on all three dialects, in
// nightly runs that passed, because background loops error without failing an
// assertion and the worker log is not in the CI console.
//
// TWO SHAPES, AND THE SECOND IS THE ONE A SQL-SERVER-ONLY READING MISSES:
//
//   - plugins/scheduler and plugins/scheduledbackup had MSSQL arms carrying
//     `enabled = true` and `now()`, neither valid in T-SQL. Both doc comments
//     say the Query provides "dialect-specific FOR UPDATE SKIP LOCKED
//     equivalents" -- and the locking hint IS what was translated. A comment
//     naming a variant's purpose narrows the reviewer to that purpose, and the
//     rest of the literal inherits the primary dialect unexamined.
//   - plugins/jobqueue's Default arm was `UPDATE ... LIMIT 1000`, which
//     PostgreSQL rejects. Query.For() returns Default for every dialect that is
//     not MySQL or MSSQL, so PostgreSQL ran it, and the reaper had never
//     executed on the PRIMARY dialect. Default differed from the MySQL arm only
//     in the interval literal -- it WAS the MySQL statement.
//
// So this is not "SQL Server was forgotten". Each dialect is broken by whichever
// arm nobody exercised.
//
// WHY THIS IS LEXICAL AND NOT A PARSE, WHICH IS A MEASURED CHOICE.
// The obvious closure is to run each arm through its dialect's parser. Measured
// 2026-09-10 against SQL Server 2022:
//
//	SET PARSEONLY ON; ... WHERE enabled = true AND next_run_at <= now()
//	  -> Msg 195  'now' is not a recognized built-in function name.
//	SET PARSEONLY ON; ... WHERE enabled = true AND next_run_at <= SYSUTCDATETIME()
//	  -> NO ERROR
//
// PARSEONLY is blind to `enabled = true`, because that is a COLUMN BINDING
// error rather than a syntax error -- and binding is 30 of the 74 faults in
// #1133. `SET NOEXEC ON` does catch it, but only because it binds, which means
// it needs the schema present and therefore a live database per dialect. A
// guard that needs three DSNs skips where they are absent, and a skipped guard
// is a false green.
//
// So: a token scan that runs in every job with no database, and a stronger
// compile-based check named here as its successor rather than implied.
//
// WHAT THIS DOES NOT CATCH, said plainly so a pass is not read as more than it
// is: raw SQL that uses no plugin.Query at all. There were 204 such strings at
// the time of writing and 69 of them carry a token invalid for some dialect
// they can reach. That is a larger, separate tranche -- this guard covers the
// statements whose dialect intent is EXPLICIT.
func TestEveryPluginQueryArmIsValidForItsOwnDialect(t *testing.T) {
	root := pluginsDir(t)
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) < 20 {
		t.Fatalf("only %d plugin source files found under %s; the walk is broken and this "+
			"guard asserts nothing", len(files), root)
	}

	var checked int
	for _, f := range files {
		for _, a := range queryArms(t, f) {
			checked++
			for _, bad := range invalidFor(a.dialect, a.sql) {
				t.Errorf("%s:%d: the %s arm contains %s, which %s rejects:\n    %s\n\n"+
					"Query.For() hands this literal to %s and nothing else translates it. "+
					"A variant that names a dialect owns EVERY dialect-specific token in "+
					"the statement, not just the one it was written for -- see cleat#1133.",
					a.file, a.line, a.dialect, bad.what, a.dialect, excerpt(a.sql), a.dialect)
			}
		}
	}
	// A scan that examined nothing is not a pass.
	if checked < 20 {
		t.Fatalf("only %d plugin.Query arms found; the extraction is broken", checked)
	}
	t.Logf("checked %d plugin.Query arms across %d files", checked, len(files))
}

type armT struct {
	file, dialect, sql string
	line               int
}

type fault struct{ what string }

// invalidFor reports tokens that the named dialect cannot accept.
//
// Deliberately narrow: every entry is a construct observed failing in #1133's
// nightly logs or measured against a live server, not a general portability
// opinion. A wider net here would flag correct SQL and train people to add
// exemptions, which is how a guard stops being read.
func invalidFor(dialect, sql string) []fault {
	var out []fault
	add := func(re *regexp.Regexp, what string) {
		if re.MatchString(sql) {
			out = append(out, fault{what})
		}
	}
	// NOT CHECKED HERE: `now()` in an MSSQL arm. plugin.Rebind rewrites it to
	// SYSUTCDATETIME() on that dialect (cleat#1138), so flagging it would reject
	// working code. This guard's first draft did flag it -- written before #1138
	// landed, against a tree where it really was a fault. Verified on the live
	// path rather than from Rebind's definition: plugins/scheduler calls
	// `plugin.Rebind(dueSchedulesQuery.For(p.dialect), p.dialect)`.
	//
	// That leaves a gap this guard does not close: a call site that uses
	// Query.For() WITHOUT Rebind gets no rewrite, and `now()` is a fault again.
	// Asserting every For() is Rebound is a separate check and a better one.
	switch dialect {
	case "MSSQL":
		add(regexp.MustCompile(`(?i)=\s*(true|false)\b`), "a boolean literal (T-SQL has none; use 1/0)")
		add(regexp.MustCompile(`(?i)\bLIMIT\b`), "`LIMIT` (use OFFSET ... FETCH NEXT)")
		add(regexp.MustCompile(`(?i)\bRETURNING\b`), "`RETURNING` (use OUTPUT)")
		add(regexp.MustCompile(`(?i)\bON\s+CONFLICT\b`), "`ON CONFLICT` (use MERGE)")
		add(regexp.MustCompile(`(?i)\bINTERVAL\s+'`), "a PostgreSQL INTERVAL literal (use DATEADD)")
		add(regexp.MustCompile(`::`), "a `::` cast (use CAST/CONVERT)")
	case "MySQL":
		add(regexp.MustCompile(`(?i)\bRETURNING\b`), "`RETURNING` (PostgreSQL-only)")
		add(regexp.MustCompile(`(?i)\bON\s+CONFLICT\b`), "`ON CONFLICT` (use ON DUPLICATE KEY UPDATE)")
		add(regexp.MustCompile(`(?i)\bINTERVAL\s+'`), "a PostgreSQL INTERVAL literal (use INTERVAL n UNIT)")
		add(regexp.MustCompile(`::`), "a `::` cast")
	case "Default":
		// Default is what Query.For() returns for PostgreSQL, so it must be
		// valid PostgreSQL -- not "whatever the author last wrote".
		//
		// TOP-LEVEL only. `UPDATE ... WHERE id IN (SELECT ... LIMIT n)` is
		// valid PostgreSQL and is the correct FIX for this fault, so a check
		// that matched LIMIT anywhere would flag the repair as the defect --
		// this one did, on its first run, against the fix in this same commit.
		// Stripping balanced parentheses leaves only clauses of the UPDATE
		// itself.
		outer := stripParens(sql)
		if regexp.MustCompile(`(?is)^\s*(UPDATE|DELETE)\b`).MatchString(outer) &&
			regexp.MustCompile(`(?i)\bLIMIT\b`).MatchString(outer) {
			out = append(out, fault{"a top-level `LIMIT`, which PostgreSQL does not accept on UPDATE/DELETE (bound the rows in a subquery instead)"})
		}
		add(regexp.MustCompile(`(?i)\bINTERVAL\s+\d+\s+(MINUTE|HOUR|DAY|SECOND)\b`), "a MySQL INTERVAL literal (PostgreSQL wants INTERVAL '5 minutes')")
		add(regexp.MustCompile(`(?i)\bON\s+DUPLICATE\s+KEY\b`), "`ON DUPLICATE KEY UPDATE` (MySQL-only)")
	}
	return out
}

// queryArms extracts `<Dialect>: ` + "`sql`" + ` fields of plugin.Query
// composite literals, via the Go parser rather than a regex.
//
// Not a regex on purpose: the first version of this scan used one anchored on a
// newline before the closing brace, and plugins/jobqueue/background.go writes
// its arms on single lines -- so the one Query with the PostgreSQL fault was
// invisible to the scan looking for it.
func queryArms(t *testing.T, path string) []armT {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []armT
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if !isPluginQuery(cl.Type) {
			return true
		}
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			lit, ok := kv.Value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			out = append(out, armT{
				file:    path,
				line:    fset.Position(lit.Pos()).Line,
				dialect: key.Name,
				sql:     strings.Trim(lit.Value, "`\""),
			})
		}
		return true
	})
	return out
}

func isPluginQuery(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "plugin" && sel.Sel.Name == "Query"
}

func pluginsDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs(filepath.Join("..", "plugins"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatalf("plugins dir not found at %s: %v", d, err)
	}
	return d
}

// stripParens removes balanced parenthesised groups, leaving only the tokens
// belonging to the outermost statement. Subquery clauses disappear with them,
// which is the point: a LIMIT inside a subquery is not a clause of the UPDATE.
func stripParens(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		return s[:140] + " ..."
	}
	return s
}
