package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This is the guard for a defect that has now shipped twice in two days, both
// times severe, both times invisible to CI.
//
//	#438  claims returned tenant_id as 16 raw bytes. The worker routes
//	      execution on that string, so every workflow on SQL Server failed
//	      with "no store for tenant".
//	this  GetDueSchedules/ListSchedules did the same, and the scheduler binds
//	      the value straight back to a UNIQUEIDENTIFIER parameter -- so no
//	      schedule fired on SQL Server at all.
//
// The cause is one property of the driver: go-mssqldb scans UNIQUEIDENTIFIER
// into a Go string as the column's 16 raw storage bytes, not the canonical
// hyphenated text. Nothing about the Go code looks wrong at the call site, and
// the value is only detectably broken once it reaches a caller that parses or
// re-binds it -- which is why both instances were found by accident rather
// than by a test.
//
// A sweep would fix the sites that exist today. This fails at authoring time
// instead, in every job, with no SQL Server required: it reads the shipped
// migrations to learn which columns are UNIQUEIDENTIFIER, then refuses any
// SELECT or OUTPUT in the MSSQL store that projects one without CONVERT or
// CAST. Deriving the column list from the migrations rather than hardcoding it
// is the point -- a UUID column added later is covered without anyone
// remembering to update this test.
func TestMSSQLUUIDColumnsAreConvertedInProjections(t *testing.T) {
	byTable := mssqlUUIDColumns(t)
	if len(byTable) == 0 {
		t.Fatal("no UNIQUEIDENTIFIER columns found in migrations/mssql -- this guard is " +
			"reading the wrong files and would pass no matter what the store did")
	}
	// Sanity anchors. If the parse silently stops finding these, every
	// assertion below becomes vacuous.
	if !byTable["workflow_schedules"]["tenant_id"] {
		t.Fatalf("workflow_schedules.tenant_id is not among the parsed UNIQUEIDENTIFIER "+
			"columns %v -- the migration parse is broken", sortedKeys(byTable))
	}
	// The guard has to be per-table, not per-column name: `id` is
	// UNIQUEIDENTIFIER on workflow_routing and ordinary text on
	// workflow_instances, so a name-only rule flags every instance query.
	if byTable["workflow_instances"]["id"] {
		t.Fatal("workflow_instances.id parsed as UNIQUEIDENTIFIER; it is not, and a guard " +
			"that thinks so will flag correct code until someone disables it")
	}

	files := goFilesCarryingSQL(t)
	var scanned int
	for _, f := range files {
		scanned++
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, v := range findRawUUIDProjections(f, string(src), byTable) {
			t.Errorf("%s:%d projects UUID column %q without CONVERT/CAST:\n    %s\n\n"+
				"go-mssqldb scans UNIQUEIDENTIFIER into a Go string as 16 raw bytes, not "+
				"canonical UUID text. Wrap it: CONVERT(NVARCHAR(36), %s) AS %s",
				f, v.line, v.column, v.context, v.column, v.column)
		}
	}
	// A floor rather than an exact count: the set grows as the repo does. It
	// exists so a glob that silently stops matching fails loudly instead of
	// reporting a clean scan of nothing.
	if scanned < 20 {
		t.Fatalf("only %d Go files scanned; the discovery walk is broken and this guard "+
			"is asserting almost nothing", scanned)
	}
	// The two files carrying the defects this guard was written for must be in
	// the set, whatever else is.
	var sawLifecycle, sawSchedules bool
	for _, f := range files {
		switch filepath.Base(f) {
		case "mssql_lifecycle.go":
			sawLifecycle = true
		case "mssql_schedules.go":
			sawSchedules = true
		}
	}
	if !sawLifecycle || !sawSchedules {
		t.Errorf("scan missed mssql_lifecycle.go (%v) or mssql_schedules.go (%v) -- "+
			"the two files whose raw projections shipped as bugs", sawLifecycle, sawSchedules)
	}
}

// goFilesCarryingSQL returns every non-test Go file in the packages that talk
// to SQL Server.
//
// The original version of this guard globbed engine/mssql_*.go only. That was
// where both known defects lived, but it is not where all the SQL Server SQL
// lives: engine/store_intent.go and engine/store_admin.go carry @pN statements
// under dialect switches, and so do plugin/ and tests/plugin-harness. A guard
// whose coverage is narrower than the class it names is worse than none,
// because its existence implies the rest was checked.
func goFilesCarryingSQL(t *testing.T) []string {
	t.Helper()
	roots := []string{".", filepath.Join("..", "auth"), filepath.Join("..", "plugin"),
		filepath.Join("..", "cmd"), filepath.Join("..", "tests")}
	var out []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a root that does not exist is not a failure
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "testdata" || d.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(out)
	return out
}

// mssqlDialectMarkers identify a SQL literal as SQL Server's.
//
// Necessary now that the scan is not confined to mssql_*.go: PostgreSQL and
// MySQL return UUID columns as canonical text, so applying this rule to their
// SQL would demand a CONVERT that does not exist in those dialects. The
// filename is still honoured for the mssql_ files, which is what covers a
// parameterless SQL Server query like GetDueSchedules.
var mssqlDialectMarkers = []*regexp.Regexp{
	regexp.MustCompile(`@p\d`),
	regexp.MustCompile(`(?i)SYSUTCDATETIME`),
	regexp.MustCompile(`(?i)NVARCHAR`),
	regexp.MustCompile(`(?i)READPAST`),
	regexp.MustCompile(`(?i)OUTPUT\s+INSERTED`),
	regexp.MustCompile(`(?i)STRING_SPLIT`),
	regexp.MustCompile(`(?i)UNIQUEIDENTIFIER`),
}

func looksLikeMSSQL(file, sql string) bool {
	if strings.HasPrefix(filepath.Base(file), "mssql_") {
		return true
	}
	for _, re := range mssqlDialectMarkers {
		if re.MatchString(sql) {
			return true
		}
	}
	return false
}

// mssqlUUIDColumns reads the shipped SQL Server migrations and returns, per
// table, every column declared UNIQUEIDENTIFIER.
func mssqlUUIDColumns(t *testing.T) map[string]map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "migrations", "mssql", "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	// `col UNIQUEIDENTIFIER` at the start of a column definition. Deliberately
	// not matching `@param UNIQUEIDENTIFIER` (function arguments) or CAST(...
	// AS UNIQUEIDENTIFIER).
	decl := regexp.MustCompile(`(?im)^\s*\[?(\w+)\]?\s+UNIQUEIDENTIFIER\b`)
	// CREATE TABLE, and the ALTER TABLE ... ADD form migrations use to add a
	// column to an existing table.
	tableRe := regexp.MustCompile(`(?i)\b(?:CREATE\s+TABLE|ALTER\s+TABLE)\s+(?:\[?\w+\]?\.)?\[?(\w+)\]?`)
	out := map[string]map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		src := string(b)
		// Walk the file once, tracking the table each declaration belongs to.
		type mark struct {
			pos   int
			table string
		}
		var marks []mark
		for _, m := range tableRe.FindAllStringSubmatchIndex(src, -1) {
			marks = append(marks, mark{pos: m[0], table: strings.ToLower(src[m[2]:m[3]])})
		}
		for _, m := range decl.FindAllStringSubmatchIndex(src, -1) {
			name := strings.ToLower(src[m[2]:m[3]])
			switch name {
			case "add", "alter", "column", "as", "cast", "convert", "table":
				continue
			}
			table := ""
			for _, mk := range marks {
				if mk.pos < m[0] {
					table = mk.table
				} else {
					break
				}
			}
			if table == "" {
				continue
			}
			if out[table] == nil {
				out[table] = map[string]bool{}
			}
			out[table][name] = true
		}
	}
	return out
}

type rawUUIDProjection struct {
	line    int
	column  string
	context string
}

var (
	// Backtick-quoted Go string literals, where this package keeps its SQL.
	sqlLiteralRe = regexp.MustCompile("(?s)`([^`]*)`")
	// A projection list: everything between SELECT/OUTPUT and the clause that
	// ends it.
	projectionRe = regexp.MustCompile(`(?is)\b(SELECT|OUTPUT)\b(.*?)(?:\bFROM\b|\bWHERE\b|\bWHEN\b|\bINTO\b|$)`)
	// Every table a statement names, so the guard can ask "is this column a
	// UUID on a table this query actually touches".
	tableRefRe = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|UPDATE|INTO|MERGE)\s+(?:\[?\w+\]?\.)?\[?(\w+)\]?`)
)

// findRawUUIDProjections returns each place a UUID column appears in a SELECT
// or OUTPUT projection without a surrounding CONVERT or CAST.
func findRawUUIDProjections(file, src string, byTable map[string]map[string]bool) []rawUUIDProjection {
	var out []rawUUIDProjection
	for _, lit := range sqlLiteralRe.FindAllStringSubmatchIndex(src, -1) {
		// Comments are stripped before anything else looks at this. They are
		// not decoration here: the claim queries carry a long -- comment
		// explaining this very conversion, and it contains the words "into"
		// and "from", either of which terminates a projection match. An
		// earlier version of this guard silently found nothing in
		// mssql_lifecycle.go for exactly that reason -- it passed while the
		// bug it was written for was reverted in front of it.
		sql := stripSQLComments(src[lit[2]:lit[3]])
		base := strings.Count(src[:lit[2]], "\n") + 1

		// PostgreSQL and MySQL hand back UUID columns as canonical text, so
		// this rule applies to SQL Server's SQL and nothing else.
		if !looksLikeMSSQL(file, sql) {
			continue
		}

		// Only the columns that are UUIDs on a table THIS statement touches.
		uuidCols := map[string]bool{}
		for _, tm := range tableRefRe.FindAllStringSubmatch(sql, -1) {
			for c := range byTable[strings.ToLower(tm[1])] {
				uuidCols[c] = true
			}
		}
		if len(uuidCols) == 0 {
			continue
		}

		for _, pm := range projectionRe.FindAllStringSubmatchIndex(sql, -1) {
			// An INSERT (...) column list is not a projection. Those follow
			// the word INSERT, which SELECT/OUTPUT never does.
			head := sql[:pm[2]]
			if trimmedEndsWith(head, "INSERT") {
				continue
			}
			proj := sql[pm[4]:pm[5]]
			for col := range uuidCols {
				colRe := regexp.MustCompile(`(?i)(?:\w+\.)?\b` + regexp.QuoteMeta(col) + `\b`)
				for _, cm := range colRe.FindAllStringIndex(proj, -1) {
					// `CONVERT(..., tenant_id) AS tenant_id` mentions the name
					// twice; the second is an alias being defined, not a column
					// being read, and it necessarily sits outside the call.
					if precededByAS(proj, cm[0]) {
						continue
					}
					if wrappedInConversion(proj, cm[0]) {
						continue
					}
					line := base + strings.Count(sql[:pm[4]+cm[0]], "\n")
					out = append(out, rawUUIDProjection{
						line:    line,
						column:  col,
						context: strings.TrimSpace(collapse(proj[maxInt(0, cm[0]-50):minInt(len(proj), cm[1]+20)])),
					})
				}
			}
		}
	}
	return out
}

// stripSQLComments blanks out -- line comments and /* */ block comments,
// preserving byte offsets and newlines so reported line numbers stay true.
func stripSQLComments(sql string) string {
	b := []byte(sql)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '-' && i+1 < len(b) && b[i+1] == '-':
			for i < len(b) && b[i] != '\n' {
				b[i] = ' '
				i++
			}
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			for i < len(b) && !(b[i] == '*' && i+1 < len(b) && b[i+1] == '/') {
				if b[i] != '\n' {
					b[i] = ' '
				}
				i++
			}
			for j := i; j < len(b) && j < i+2; j++ {
				b[j] = ' '
			}
			i++
		}
	}
	return string(b)
}

// precededByAS reports whether the token at idx is an alias (`... AS name`)
// rather than a column reference.
func precededByAS(proj string, idx int) bool {
	i := idx - 1
	for i >= 0 && (proj[i] == ' ' || proj[i] == '\t' || proj[i] == '\n') {
		i--
	}
	if i < 1 {
		return false
	}
	if !strings.EqualFold(proj[i-1:i+1], "AS") {
		return false
	}
	// Must be the whole word AS, not the tail of an identifier.
	return i-2 < 0 || !isIdentByte(proj[i-2])
}

// wrappedInConversion reports whether the token at idx sits inside a
// CONVERT(...) or CAST(...) call.
//
// Comma-based lookback does not work here: CONVERT(NVARCHAR(36), tenant_id)
// contains a comma between the function name and the column, so scanning back
// to the nearest comma lands inside the call and finds no CONVERT. This walks
// outward through balanced parentheses instead, checking the name of each
// enclosing call, which is the only way to answer the question correctly for
// nested expressions like LOWER(CONVERT(NVARCHAR(36), tenant_id)).
func wrappedInConversion(proj string, idx int) bool {
	depth := 0
	for i := idx - 1; i >= 0; i-- {
		switch proj[i] {
		case ')':
			depth++
		case '(':
			if depth > 0 {
				depth--
				continue
			}
			// Unmatched '(' -- we are inside this call. Read its name.
			j := i - 1
			for j >= 0 && (proj[j] == ' ' || proj[j] == '\t' || proj[j] == '\n') {
				j--
			}
			end := j + 1
			for j >= 0 && (isIdentByte(proj[j])) {
				j--
			}
			name := strings.ToUpper(proj[j+1 : end])
			if name == "CONVERT" || name == "CAST" {
				return true
			}
			// Some other call (LOWER, ISNULL, COALESCE): keep looking outward.
		}
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func trimmedEndsWith(s, word string) bool {
	s = strings.TrimRight(s, " \t\n(")
	if len(s) < len(word) {
		return false
	}
	return strings.EqualFold(s[len(s)-len(word):], word)
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func sortedKeys(m map[string]map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
