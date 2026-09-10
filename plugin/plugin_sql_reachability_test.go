package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// No plugin may execute a raw statement carrying a construct the dialects it
// reaches cannot parse.
//
// cleat#1133. Parts 1-3 of that issue made plugin.Rebind literal-aware, gave it
// boolean literals, and moved it to the adapter -- so $N, now() and TRUE/FALSE
// are handled centrally for every statement. What Rebind deliberately does NOT
// do is anything that changes the SHAPE of a statement: LIMIT/TOP,
// ON CONFLICT/MERGE, RETURNING/OUTPUT, PostgreSQL interval literals. Those are
// written per dialect in a plugin.Query, and this guard exists so the next raw
// literal carrying one does not ship.
//
// WHY THIS ANCHORS ON db.Query/db.Exec AND NOT ON plugin.Query.
// plugin/dialect_sql_test.go checks that each ARM of a plugin.Query is valid
// for the dialect it names. That is necessary and it is not sufficient, and the
// gap cost two rounds of fixing: jobqueue's pollPending is a raw literal with
// `LIMIT 10` sitting EIGHT LINES above a plugin.Query that cleat#1134 and
// cleat#1141 both edited. Neither touched it, because neither was looking at
// call sites. After both repairs the reaper was correct and had nothing to reap
// on SQL Server, since no job could reach `running` there.
//
//	plugin.Query was never the boundary of the defect, only the boundary of
//	the fix.
//
// So the unit here is the EXECUTION site.
//
// REACHABILITY IS THE WHOLE QUESTION, AND GETTING IT WRONG INFLATES.
// A construct is a defect only if it can reach a dialect that rejects it. I
// published "5 bare-boolean sites" for this issue and the answer was 3: the
// scan flagged every `NOT <col>` without asking which dialect arm it sat in,
// and `NOT processed` in a Default or MySQL arm is correct for that dialect. It
// also counted `WHEN NOT MATCHED` from MERGE, which is not a boolean at all.
//
// THE EXEMPTION IS DERIVED, NOT LISTED, which is the part worth keeping.
// A plugin that declares migrations and ships no UpMySQL/UpMSSQL arm has said
// it is PostgreSQL-only -- plugin/migration.go's own doc names pgvector as the
// case -- so its tables never exist on another backend and its SQL is only
// required to be valid PostgreSQL. That predicate lives in the code rather than
// in a list here, so it cannot go stale: add a MySQL arm to pgvector and this
// guard starts requiring portable SQL of it on the same commit.
//
// (Whether such a plugin should be LOADED at all on a backend where its tables
// cannot exist is cleat#1157. It currently is: the declaration is recorded by
// the migration runner and never read.)
func TestNoPluginSQLReachesADialectThatRejectsIt(t *testing.T) {
	root := pluginsDir(t)
	pkgs := map[string][]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		d := filepath.Dir(p)
		pkgs[d] = append(pkgs[d], p)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(pkgs) < 15 {
		t.Fatalf("only %d plugin packages found under %s; the walk is broken and this "+
			"guard asserts nothing", len(pkgs), root)
	}

	var offenders []string
	checked, exempt := 0, []string{}
	for dir, files := range pkgs {
		name := filepath.Base(dir)
		if pgOnly(t, files) {
			exempt = append(exempt, name)
			continue
		}
		for _, f := range files {
			for _, s := range rawExecutedSQL(t, f) {
				checked++
				if bad := nonPortable(s.sql); len(bad) > 0 {
					offenders = append(offenders,
						s.pos+"  "+strings.Join(bad, ",")+
							"  -- raw literal, so it reaches every dialect")
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no raw executed SQL at all; the AST match is broken and a pass " +
			"here means nothing")
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d raw statement(s) carry a construct some reachable dialect rejects.\n"+
			"plugin.Rebind does not rewrite these -- they change the SHAPE of the\n"+
			"statement, not a token in it -- so they need a plugin.Query with an arm\n"+
			"per dialect, verified by plugins/plugintest.RunEveryArm:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	sort.Strings(exempt)
	t.Logf("checked %d raw executed statements; PostgreSQL-only by declaration (no "+
		"UpMySQL/UpMSSQL migration arm): %v", checked, exempt)
}

// pgOnly reports whether this package declares migrations and provides no
// dialect override for any of them.
func pgOnly(t *testing.T, files []string) bool {
	t.Helper()
	declares, override := false, false
	for _, f := range files {
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			continue
		}
		ast.Inspect(af, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isMigrationLit(lit.Type) {
				return true
			}
			declares = true
			// The dialect keys live in the ELEMENT literals of
			// []plugin.Migration{{...}, {...}}, and those carry no type of
			// their own -- lit.Type is nil for them, so a scan keyed on the
			// type name never sees them. Reading only the outer literal made
			// every plugin look PostgreSQL-only, which the checked==0 guard
			// below caught.
			for _, el := range lit.Elts {
				inner, ok := el.(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, f := range inner.Elts {
					kv, ok := f.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok &&
						(id.Name == "UpMySQL" || id.Name == "UpMSSQL") {
						override = true
					}
				}
			}
			return true
		})
	}
	return declares && !override
}

func isMigrationLit(e ast.Expr) bool {
	switch tt := e.(type) {
	case *ast.SelectorExpr:
		return tt.Sel.Name == "Migration"
	case *ast.ArrayType:
		return isMigrationLit(tt.Elt)
	}
	return false
}

type execSite struct{ pos, sql string }

var reIsSQLStmt = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|WITH)\b`)

// rawExecutedSQL returns the string LITERALS handed to a db execution method.
// A call whose SQL comes from a plugin.Query (`q.For(d)`) or any other
// expression is skipped: this guard is about statements with no dialect
// variants at all.
func rawExecutedSQL(t *testing.T, path string) []execSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil
	}
	var out []execSite
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Query", "Exec", "QueryRow", "QueryContext", "ExecContext", "QueryRowContext":
		default:
			return true
		}
		for _, a := range call.Args {
			lit := stringLit(a)
			if lit == "" || !reIsSQLStmt.MatchString(lit) {
				continue
			}
			out = append(out, execSite{fset.Position(a.Pos()).String(), lit})
		}
		return true
	})
	return out
}

// stringLit unwraps a bare literal, and one wrapped in plugin.Rebind(...) --
// Rebind does not change any of the constructs this guard looks for, so a
// rebound literal is exactly as exposed as a bare one.
func stringLit(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.BasicLit:
		if t.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(t.Value)
		if err != nil {
			return ""
		}
		return s
	case *ast.CallExpr:
		if sel, ok := t.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Rebind" &&
			len(t.Args) > 0 {
			return stringLit(t.Args[0])
		}
	}
	return ""
}

var portabilityChecks = []struct {
	name string
	re   *regexp.Regexp
}{
	// T-SQL spells row limits TOP / OFFSET..FETCH.
	{"LIMIT", regexp.MustCompile(`(?i)\bLIMIT\s`)},
	// PostgreSQL upsert; MySQL wants INSERT IGNORE / ON DUPLICATE KEY, T-SQL
	// an explicit NOT EXISTS or MERGE.
	{"ON CONFLICT", regexp.MustCompile(`(?i)\bON\s+CONFLICT\b`)},
	// T-SQL spells it OUTPUT INSERTED.
	{"RETURNING", regexp.MustCompile(`(?i)\bRETURNING\b`)},
	// PostgreSQL's quoted plural interval literal. MySQL answers Error 1064.
	{"INTERVAL '...'", regexp.MustCompile(`(?i)\bINTERVAL\s+'`)},
	// PostgreSQL-only case-insensitive LIKE.
	{"ILIKE", regexp.MustCompile(`(?i)\bILIKE\b`)},
}

// A bare boolean COLUMN is not a condition in T-SQL: Msg 4145. It is a BINDING
// error rather than a syntax error, which is why SET PARSEONLY ON accepts it
// and only SET NOEXEC ON rejects it.
//
// This is a function and not another entry in the table above because Go's
// regexp is RE2 and has no lookahead: the obvious
// `NOT\s+(?!EXISTS|IN|LIKE|...)` does not compile, and MustCompile panics
// rather than failing a vet. The words below are OPERATORS -- `NOT EXISTS`,
// `NOT IN`, `NOT LIKE`, `NOT NULL`, and `WHEN NOT MATCHED` from MERGE -- so a
// scan that does not exclude them reports every MERGE statement as a boolean
// defect. Mine did, which is part of how "5 sites" turned out to be 3.
var reNotWord = regexp.MustCompile(`(?i)\bNOT\s+([A-Za-z_][A-Za-z0-9_.]*)`)

var notOperators = map[string]bool{
	"exists": true, "in": true, "like": true, "null": true, "matched": true,
	"between": true, "and": true, "or": true, "not": true,
}

func hasBareBooleanColumn(sql string) bool {
	for _, m := range reNotWord.FindAllStringSubmatch(sql, -1) {
		if !notOperators[strings.ToLower(m[1])] {
			return true
		}
	}
	return false
}

func nonPortable(sql string) []string {
	var out []string
	for _, c := range portabilityChecks {
		if c.re.MatchString(sql) {
			out = append(out, c.name)
		}
	}
	if hasBareBooleanColumn(sql) {
		out = append(out, "NOT <column>")
	}
	return out
}

// The guard must be able to see each construct it claims to check, and must not
// flag the operators that merely look like one.
//
// A guard that passes on a green tree is satisfied by every broken version of
// itself. This is the in-repo control: it does not depend on mutating a real
// file, so it keeps working after the tree is clean.
func TestTheReachabilityGuardSeesEachConstruct(t *testing.T) {
	mustFlag := map[string]string{
		"LIMIT":          `SELECT a FROM t ORDER BY a LIMIT 10`,
		"ON CONFLICT":    `INSERT INTO t (a) VALUES ($1) ON CONFLICT DO NOTHING`,
		"RETURNING":      `INSERT INTO t (a) VALUES ($1) RETURNING id`,
		"INTERVAL '...'": `SELECT a FROM t WHERE at < NOW() - INTERVAL '10 seconds'`,
		"ILIKE":          `SELECT a FROM t WHERE name ILIKE 'x%'`,
		"NOT <column>":   `SELECT a FROM t WHERE NOT processed`,
	}
	for want, sql := range mustFlag {
		got := nonPortable(sql)
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the guard does not flag %s\n  sql:  %s\n  got:  %v", want, sql, got)
		}
	}

	// These must NOT be flagged. Every one is a construct that resembles
	// something above, and each has cost someone a false finding: NOT EXISTS
	// and WHEN NOT MATCHED are operators rather than boolean columns, MySQL's
	// unquoted INTERVAL is correct for MySQL, and TOP/OFFSET are the T-SQL
	// spellings this guard exists to steer people towards.
	mustNotFlag := []string{
		`SELECT a FROM t WHERE NOT EXISTS (SELECT 1 FROM u)`,
		`SELECT a FROM t WHERE b NOT IN (1, 2)`,
		`SELECT a FROM t WHERE name NOT LIKE 'x%'`,
		`SELECT a FROM t WHERE a IS NOT NULL`,
		`MERGE t USING s ON t.id = s.id WHEN NOT MATCHED THEN INSERT (a) VALUES (s.a)`,
		`SELECT a FROM t WHERE at < NOW() - INTERVAL 10 SECOND`,
		`SELECT TOP 10 a FROM t ORDER BY a`,
		`SELECT a FROM t ORDER BY a OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY`,
		`UPDATE t SET processed = 1 WHERE processed = 0`,
	}
	for _, sql := range mustNotFlag {
		if got := nonPortable(sql); len(got) > 0 {
			t.Errorf("the guard flags portable/dialect-correct SQL as a defect\n"+
				"  sql: %s\n  got: %v", sql, got)
		}
	}
}
