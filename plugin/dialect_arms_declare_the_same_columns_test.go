package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A plugin migration carries up to three hand-written definitions of the same
// table -- Up, UpMySQL, UpMSSQL -- and nothing checked that they describe the
// same columns. cleat#1291.
//
// They agree today. This exists because obtaining that answer took five
// attempts, four of which were wrong, and because every wrong one
// OVER-reported drift: a check that under-reported would have been
// indistinguishable from a correct one and nobody would have questioned it.
//
// WHY NOT A REGEX, which is the whole reason this guard is awkward.
// A Go raw string cannot contain a backtick, so MySQL DDL for a column named
// `key` is written as concatenation:
//
//	Up: `CREATE TABLE ... ` + "`key`" + ` VARCHAR(255) ...`
//
// A textual scan captures the first fragment and stops, reporting MySQL as
// missing nearly every column. go/parser evaluates the concatenation, which is
// the only reading that sees the whole statement.
//
// WHAT THIS DOES NOT CHECK. That any arm is CORRECT, or valid for its dialect
// -- #1171 guards that, anchored on where SQL is executed. And it is not
// covered by TestEveryDialectAgreesWhichColumnsAWriterMustSupply (#963), which
// compares which columns a writer MUST SUPPLY -- NOT NULL, no default -- after
// running the migrations against all three live databases. That cannot see a
// nullable or defaulted column present in one arm and absent from another, and
// it cannot run at all without three DSNs. This is static and needs no
// database, so it catches arm drift on any machine.
func TestPluginDialectArmsDeclareTheSameColumns(t *testing.T) {
	files := pluginMigrationFiles(t)
	if len(files) < 10 {
		t.Fatalf("found %d plugins/*/migrations.go; the scan is broken and this "+
			"guard would pass vacuously", len(files))
	}

	var compared int
	for _, file := range files {
		for _, m := range migrationsIn(t, file) {
			// table -> arm -> columns
			byTable := map[string]map[string][]string{}
			for arm, sql := range m.arms {
				tables := createTableColumns(sql, arm)

				// AN ARM THAT SAYS "CREATE TABLE" MUST YIELD A TABLE.
				//
				// Found by known-positive 3 and it is the reason that
				// control was worth running. Breaking evalStringExpr so it
				// takes only the first fragment -- what a regex effectively
				// does -- did NOT report drift. It truncated the MySQL arm
				// mid-statement, so the parenthesis never balanced, no
				// columns came out, the table appeared in one arm only, and
				// the comparison was silently SKIPPED.
				//
				// That is an under-report, and cleat#1291 says exactly why it
				// matters: all four historical wrong answers over-reported,
				// and an under-reporting check is indistinguishable from a
				// correct one. Without this the guard would have passed while
				// comparing nothing.
				//
				// A predicate rather than a count, so it cannot drift as
				// plugins are added.
				if len(tables) == 0 && createTableRe.MatchString(sql) {
					t.Errorf("%s v%d arm %s contains CREATE TABLE but no table was "+
						"extracted from it.\n\nThe arm parsed to %d characters. If that "+
						"is short, the Go string expression was not fully evaluated -- a "+
						"raw string cannot contain a backtick, so MySQL DDL is split "+
						"across concatenated fragments and anything that reads only the "+
						"first one truncates mid-statement.",
						m.plugin, m.version, arm, len(sql))
				}

				for table, cols := range tables {
					if byTable[table] == nil {
						byTable[table] = map[string][]string{}
					}
					byTable[table][arm] = cols
				}
			}

			for table, arms := range byTable {
				if len(arms) < 2 {
					continue // only one arm declares it; nothing to compare
				}
				compared++
				names := make([]string, 0, len(arms))
				for a := range arms {
					names = append(names, a)
				}
				sort.Strings(names)
				base := names[0]
				for _, other := range names[1:] {
					if missing, extra := diffCols(arms[base], arms[other]); len(missing)+len(extra) > 0 {
						t.Errorf("%s v%d table %q: the %s and %s arms declare different columns.\n"+
							"  in %s, not %s: %v\n"+
							"  in %s, not %s: %v\n\n"+
							"Both arms create the same table on different backends, so a column "+
							"present in one and absent from the other is a plugin that works on "+
							"one dialect and fails on another at query time.",
							m.plugin, m.version, table, base, other,
							base, other, missing, other, base, extra)
					}
				}
			}
		}
	}

	// A guard that compared nothing would report no differences. cleat#1291's
	// own history is four checks that each returned a confident wrong number.
	if compared == 0 {
		t.Fatal("no table was declared by two or more arms, so nothing was compared; " +
			"either the plugins stopped shipping dialect arms or this scan is broken")
	}
	t.Logf("compared %d (plugin, version, table) definitions across dialect arms", compared)
}

type pluginMigration struct {
	plugin  string
	version int
	arms    map[string]string // "Up" | "UpMySQL" | "UpMSSQL" -> SQL
}

// migrationsIn parses one plugins/<name>/migrations.go and returns each
// Migration literal's dialect arms, with concatenation evaluated.
func migrationsIn(t *testing.T, path string) []pluginMigration {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../"+path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	plugin := strings.Split(path, "/")[1]

	var out []pluginMigration
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		m := pluginMigration{plugin: plugin, version: -1, arms: map[string]string{}}
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Version":
				if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.INT {
					m.version, _ = strconv.Atoi(lit.Value)
				}
			case "Up", "UpMySQL", "UpMSSQL":
				if s, ok := evalStringExpr(kv.Value); ok && strings.TrimSpace(s) != "" {
					m.arms[key.Name] = s
				}
			}
		}
		if len(m.arms) > 0 {
			out = append(out, m)
		}
		return true
	})
	return out
}

// evalStringExpr evaluates a Go string expression built from literals and `+`.
//
// This is the step a regex cannot do, and skipping it is what made attempt 3
// in cleat#1291 report MySQL as missing nearly every column: a raw string
// cannot contain a backtick, so any MySQL identifier needing one splits the
// literal in two.
func evalStringExpr(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, okL := evalStringExpr(v.X)
		r, okR := evalStringExpr(v.Y)
		if !okL || !okR {
			return "", false
		}
		return l + r, true
	case *ast.ParenExpr:
		return evalStringExpr(v.X)
	}
	return "", false
}

var createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(]+)\s*\(`)

// createTableColumns maps each table a statement creates to its column names.
func createTableColumns(sql, arm string) map[string][]string {
	out := map[string][]string{}
	for _, loc := range createTableRe.FindAllStringSubmatchIndex(sql, -1) {
		table := normaliseIdent(sql[loc[2]:loc[3]])
		body, ok := balancedParenBody(sql, loc[1]-1)
		if !ok {
			continue
		}
		var cols []string
		for _, item := range splitTopLevel(body) {
			if name, ok := columnName(item, arm); ok {
				cols = append(cols, name)
			}
		}
		sort.Strings(cols)
		out[table] = cols
	}
	return out
}

// balancedParenBody returns the text between the parenthesis at open and its
// match. A naive scan to the first ")" truncates at the first CHAR(36) or
// DEFAULT (now()) -- attempt 2 in cleat#1291.
func balancedParenBody(s string, open int) (string, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

func splitTopLevel(body string) []string {
	var items []string
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				items = append(items, body[start:i])
				start = i + 1
			}
		}
	}
	return append(items, body[start:])
}

// universalConstraints begin a table constraint on every dialect.
var universalConstraints = map[string]bool{
	"PRIMARY": true, "UNIQUE": true, "CONSTRAINT": true,
	"FOREIGN": true, "CHECK": true,
}

// mysqlIndexKeywords begin an INLINE index, which only MySQL allows inside
// CREATE TABLE.
//
// THE DIALECT IS THE DISCRIMINATOR, and this is the trap cleat#1291 documents
// as attempt 4 -- which I then hit from the other side while implementing it.
//
// MySQL writes an index as `KEY idx_name (col)`. PostgreSQL writes a column
// named key as `key TEXT NOT NULL` -- unquoted, because key is not reserved
// there. Treating KEY as a keyword everywhere drops that column from the
// PostgreSQL arm and reports drift against MySQL and SQL Server, which quote
// it. Distinguishing on "is there a parenthesis" fails too: VARCHAR(255) has
// one.
//
// What actually separates them is that inline KEY/INDEX is MySQL syntax and
// is a syntax error in the other two, so the keyword can only mean an index in
// the MySQL arm. First measured here as "in UpMySQL, not Up: [key]".
var mysqlIndexKeywords = map[string]bool{
	"KEY": true, "INDEX": true, "FULLTEXT": true, "SPATIAL": true,
}

// columnName returns the column an item declares, or false if it is a
// constraint or an index. See mysqlIndexKeywords for why the arm matters.
func columnName(item, arm string) (string, bool) {
	item = strings.TrimSpace(item)
	if item == "" || strings.HasPrefix(item, "--") {
		return "", false
	}
	fields := strings.Fields(item)
	if len(fields) == 0 {
		return "", false
	}
	first := fields[0]
	quoted := strings.HasPrefix(first, "`") || strings.HasPrefix(first, "[") ||
		strings.HasPrefix(first, `"`)
	upper := strings.ToUpper(strings.TrimSuffix(first, "("))
	if !quoted && universalConstraints[upper] {
		return "", false
	}
	if !quoted && arm == "UpMySQL" && mysqlIndexKeywords[upper] {
		return "", false
	}
	return normaliseIdent(first), true
}

func normaliseIdent(s string) string {
	s = strings.TrimSpace(s)
	for _, pair := range [][2]string{{"`", "`"}, {"[", "]"}, {`"`, `"`}} {
		if strings.HasPrefix(s, pair[0]) && strings.HasSuffix(s, pair[1]) && len(s) > 1 {
			s = s[1 : len(s)-1]
		}
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:] // dbo.event_history -> event_history
	}
	return strings.ToLower(s)
}

func diffCols(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, c := range b {
		inB[c] = true
	}
	inA := map[string]bool{}
	for _, c := range a {
		inA[c] = true
	}
	for _, c := range a {
		if !inB[c] {
			onlyA = append(onlyA, c)
		}
	}
	for _, c := range b {
		if !inA[c] {
			onlyB = append(onlyB, c)
		}
	}
	return onlyA, onlyB
}

// pluginMigrationFiles lists plugins/*/migrations.go via git, not a walk:
// .claude/worktrees/ holds whole copies of this repository and a walk
// descends into them.
func pluginMigrationFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "plugins/*/migrations.go")
	cmd.Dir = ".."
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
