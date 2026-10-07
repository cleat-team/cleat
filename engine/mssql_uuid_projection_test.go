package engine

import (
	"fmt"
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
// AND THE CASE IS NOT OBSERVABLE FROM BEHAVIOUR, which is why this is a text
// guard and not a test that exercises the store. Measured 2026-10-02: reverting
// the LOWER at the SQL Server schedule projection -- a site whose tenant is
// bound straight back to a UNIQUEIDENTIFIER parameter and composed into the
// cron:<tenant>:<name>:<instant> idempotency key -- left every message,
// schedule and claim test green, including 112 MSSQL subtests run against a
// real SQL Server 2022. So a green suite says nothing about a projection's case,
// and cleat#2993's nine sites shipped through exactly that blind spot.
//
// A sweep would fix the sites that exist today. This fails at authoring time
// instead, in every job, with no SQL Server required: it reads the shipped
// migrations to learn which columns are UNIQUEIDENTIFIER, then refuses any
// SELECT or OUTPUT in the MSSQL store that projects one without BOTH a CONVERT
// or CAST and a LOWER. Deriving the column list from the migrations rather than
// hardcoding it is the point -- a UUID column added later is covered without
// anyone remembering to update this test.
//
// WHAT IT CANNOT SEE, so that a green run is not read as more than it is: the
// derivation needs a table NAME. A statement whose table arrives as a
// runtime-concatenated identifier -- `FROM ` + someVar -- matches no table
// reference, contributes no UUID columns, and is skipped without comment. It
// also misses tables no migration declares, because there is nothing to derive.
//
// Both together describe exactly one site in the tree today:
// engine/testutil/mssql_row_disappearance.go, whose audit table is created at
// runtime. That site's LOWER is wrapped BY HAND and is NOT enforced -- an edit
// that drops it would leave this guard green. Found on 2026-10-02 by
// enumerating every non-LOWERed CONVERT(NVARCHAR(36), ...) in the tree and
// diffing against what this guard reported: three of the four non-test hits
// were prose, this was the fourth. That enumeration is the control for a claim
// of coverage, and it is cheap; re-run it when a UUID column moves somewhere
// this cannot follow.
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
			// The guard now REQUIRES LOWER, so the message is advice rather than
			// the only line standing between an author and the defect -- which is
			// the change cleat#2993 exists to make. Until it landed, this guard
			// accepted a bare CONVERT and its message prescribed one, so an
			// author who obeyed it wrote `CONVERT(NVARCHAR(36), x)`, passed, and
			// shipped an id that is UPPERCASE on SQL Server where the value the
			// application wrote is lowercase: a by-id lookup misses, and a
			// command that prints an id and accepts it back fails to match its
			// own output (cleat#2983). The message still names the canonical form,
			// and TestTheProjectionAdvicePrescribesTheCanonicalForm still asserts
			// its text -- the repair for a message that was a vector is a message
			// that is not, not the removal of the test that noticed.
			t.Errorf("%s", uuidProjectionMessage(f, v))
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
	line   int
	column string
	// converted distinguishes the two ways a projection can be wrong, because
	// they are different defects with different repairs and the message owes the
	// reader the right one. false: no CONVERT at all, so the driver hands back
	// 16 raw bytes. true: CONVERTed but not LOWERed, so the text is canonical in
	// the wrong case -- cleat#2993.
	converted bool
	context   string
}

var (
	// Backtick-quoted Go string literals, where this package keeps its SQL.
	sqlLiteralRe = regexp.MustCompile("(?s)`([^`]*)`")
	// A projection list: everything between SELECT/OUTPUT and the clause that
	// ends it.
	//
	// ON is in the alternation for MERGE, and was added by cleat#2434. A MERGE
	// reads
	//
	//	MERGE workflow_memory_stats AS target
	//	USING (SELECT @p1 AS def_name, ..., @p3 AS tenant_id) AS source
	//	ON target.def_name = source.def_name AND target.tenant_id = @p3
	//	WHEN MATCHED THEN UPDATE ...
	//
	// and the SELECT inside USING has no FROM, so the only terminator ahead of
	// it was WHEN -- which put the whole ON clause inside the "projection". A
	// join or match CONDITION is not a projection and nothing in it is scanned
	// into Go, so the guard reported mssql_schedules.go's memory-sample upsert
	// as a raw UUID projection.
	//
	// It did not fire until the migrations were compacted, because
	// workflow_memory_stats.tenant_id was not among the columns the old chain's
	// text let this parser find. Better column coverage exposed a latent
	// false positive; the fix is here rather than in an allowlist, since the
	// next MERGE would have hit it too.
	//
	// Adding ON cannot mask a real violation: a genuine projection lists its
	// columns BEFORE any FROM, and ON only appears after one in a query that
	// has a FROM at all.
	projectionRe = regexp.MustCompile(`(?is)\b(SELECT|OUTPUT)\b(.*?)(?:\bFROM\b|\bWHERE\b|\bWHEN\b|\bINTO\b|\bON\b|$)`)
	// A MERGE with no ON at all still ends at its first WHEN, which the
	// alternation above already covers.
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
			// Nor is the SELECT that FEEDS an INSERT. Its values go straight
			// into another column of the same type and are never scanned into
			// Go, so the driver's raw-bytes behaviour -- the entire subject of
			// this guard -- cannot apply to them. Converting such a projection
			// to text would be the wrong fix: it would round-trip a UUID
			// through NVARCHAR for no reader.
			//
			// cleat#1186 added the first one, acquiring concurrency keys with
			// INSERT INTO concurrency_keys ... SELECT ... FROM workflow_instances.
			// Detected by looking back for an unterminated INSERT INTO rather
			// than by allowlisting a line, so the whole class is covered and an
			// exemption cannot rot onto a different statement.
			if feedsAnInsert(head) {
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
					converted, lowered := projectionWrapping(proj, cm[0])
					if converted && lowered {
						continue
					}
					line := base + strings.Count(sql[:pm[4]+cm[0]], "\n")
					out = append(out, rawUUIDProjection{
						line:      line,
						column:    col,
						converted: converted,
						context:   strings.TrimSpace(collapse(proj[maxInt(0, cm[0]-50):minInt(len(proj), cm[1]+20)])),
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

// projectionWrapping reports, for the token at idx, whether it sits inside a
// CONVERT(...)/CAST(...) call and whether it sits inside a LOWER(...) call.
//
// BOTH are required, and they are two requirements rather than one restated:
// CONVERT supplies canonical UUID TEXT where go-mssqldb would otherwise hand
// back 16 raw storage bytes, and LOWER supplies the CASE, because
// CONVERT(NVARCHAR(36), x) returns UPPERCASE on SQL Server while the value the
// application wrote is lowercase. Either alone is a defect -- the first is the
// raw-bytes bug this guard was written for, the second is cleat#2983, which
// shipped through this guard because CONVERT alone satisfied it.
//
// Comma-based lookback does not work here: CONVERT(NVARCHAR(36), tenant_id)
// contains a comma between the function name and the column, so scanning back
// to the nearest comma lands inside the call and finds no CONVERT. This walks
// outward through balanced parentheses instead, checking the name of each
// enclosing call, which is the only way to answer the question correctly for
// nested expressions like LOWER(CONVERT(NVARCHAR(36), tenant_id)).
//
// A single outward walk collecting both facts, rather than two walks: two would
// be the same code twice, and the second would have to be trusted not to differ.
//
// WHAT THE ORDER OF THE TWO DOES NOT MATTER FOR, measured rather than assumed on
// SQL Server 2022 on 2026-10-02. Both nestings lower the result correctly, so the
// walk accepts either and this is not an oversight:
//
//	LOWER(CONVERT(NVARCHAR(36), id))  -> a0533f82-3acc-4ffb-897e-b04d7fd395af
//	CONVERT(NVARCHAR(36), LOWER(id))  -> a0533f82-3acc-4ffb-897e-b04d7fd395af
//
// A bare LOWER(id) with no conversion at all ALSO returns that same lowercase
// text, so the rule refuses a form that works. That is deliberate and is the one
// place this predicate is stricter than the database: it relies on SQL Server
// implicitly choosing the character type for LOWER's argument, and a guard whose
// subject is "say explicitly what the text conversion is" should not accept an
// implicit one. The cost of refusing it is a wrap that is also correct.
func projectionWrapping(proj string, idx int) (converted, lowered bool) {
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
			switch strings.ToUpper(proj[j+1 : end]) {
			case "CONVERT", "CAST":
				converted = true
			case "LOWER":
				lowered = true
			}
			// Some other call (ISNULL, COALESCE): keep looking outward.
		}
	}
	return converted, lowered
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

// feedsAnInsert reports whether the projection starting after head is the
// source of an INSERT INTO ... SELECT.
//
// "Unterminated" is the whole test: an INSERT earlier in the same statement
// means this SELECT supplies its rows, while one in a PREVIOUS statement --
// separated by a semicolon -- has nothing to do with it. Without the semicolon
// check, any SELECT following any INSERT in a multi-statement string would be
// excused.
func feedsAnInsert(head string) bool {
	i := strings.LastIndex(strings.ToUpper(head), "INSERT INTO")
	if i < 0 {
		return false
	}
	return !strings.Contains(head[i:], ";")
}

// uuidProjectionMessage is what an author who trips this guard is told. It is a
// function rather than an inline Errorf argument so that its TEXT can be
// asserted on -- which is the whole point of the change that introduced it.
//
// THE MESSAGE WAS THE VECTOR, and that is why the text is asserted on rather
// than trusted. Until cleat#2993 this guard required CONVERT and NOT LOWER, so
// its advice was the only place the prescribed form was written down: an author
// who obeyed the message wrote `CONVERT(NVARCHAR(36), x)`, passed the check, and
// shipped an id that is UPPERCASE on SQL Server where the value the application
// wrote is lowercase. That is not hypothetical -- it is what happened in
// cleat#2982, where a minted credential could not be recognised as its own row
// and `cleatctl operator-key revoke --key-id <what list printed>` matched nothing.
//
// The guard NOW enforces the canonical form, so the message is advice rather
// than the last line of defence. It still distinguishes the two defects, because
// they have different causes and a reader who is told the wrong one looks in the
// wrong place: "without CONVERT/CAST" means the driver is handing back raw bytes,
// while "CONVERTed but not LOWERed" means the text is right and only the case is
// wrong.
func uuidProjectionMessage(file string, v rawUUIDProjection) string {
	// The per-mode sentence names the DEFECT, and the tail explains the
	// PRESCRIPTION. They are separate because both modes prescribe the same wrap:
	// whoever is told to write LOWER(CONVERT(...)) needs to know why the LOWER is
	// there even when their own defect was the missing CONVERT, and an author who
	// does not know why removes it as noise -- which is how this came back the
	// first time.
	missing := "without CONVERT/CAST"
	defect := "go-mssqldb scans UNIQUEIDENTIFIER into a Go string as 16 raw bytes, " +
		"not canonical UUID text."
	if v.converted {
		missing = "CONVERTed but not LOWERed"
		defect = "the text is canonical but in the wrong case, so the projected id " +
			"would not equal the id that created the row: a by-id lookup misses, and a " +
			"command that prints an id and accepts it back fails to match its own output."
	}
	return fmt.Sprintf("%s:%d projects UUID column %q %s:\n    %s\n\n"+
		"%s Wrap it: LOWER(CONVERT(NVARCHAR(36), %s)) AS %s -- the LOWER is not "+
		"decoration: CONVERT alone returns UPPERCASE on SQL Server while the value the "+
		"application wrote is lowercase (cleat#2983, cleat#2993).",
		file, v.line, v.column, missing, v.context, defect, v.column, v.column)
}

// TestTheProjectionAdvicePrescribesTheCanonicalForm pins the artefact this issue
// is about, and it asserts on the TEXT because the text is what failed.
//
// A status-only test could not have caught this: the guard's verdict was correct
// in every instance -- it refused exactly what it said it refused. What was
// wrong was the repair it named, and only an assertion over the message can see
// that.
func TestTheProjectionAdvicePrescribesTheCanonicalForm(t *testing.T) {
	// Both modes, because the guard can now be tripped two ways and a message
	// that only tells the reader about one of them sends the other one looking
	// in the wrong place.
	for _, converted := range []bool{false, true} {
		msg := uuidProjectionMessage("engine/x.go", rawUUIDProjection{
			line: 12, column: "tenant_id", context: "&wf.TenantID", converted: converted,
		})

		if !strings.Contains(msg, "LOWER(CONVERT(NVARCHAR(36), tenant_id))") {
			t.Errorf("the prescribed wrap is not the canonical one, so an author who obeys "+
				"this message writes an id that is UPPERCASE on SQL Server:\n%s", msg)
		}

		// THE CONTROL, and it needs care: `CONVERT(NVARCHAR(36), tenant_id)` is a
		// SUBSTRING of the corrected text, so asserting only that the message
		// mentions CONVERT would pass on both the broken and the fixed message and
		// measure nothing. The assertion has to be that the OLD PRESCRIPTION -- the
		// whole "Wrap it: ... AS ..." clause, un-LOWERed -- is absent.
		if strings.Contains(msg, "Wrap it: CONVERT(NVARCHAR(36), tenant_id) AS tenant_id") {
			t.Errorf("the message still prescribes the un-LOWERed wrap, which passed this "+
				"guard before cleat#2993 and ships the bug:\n%s", msg)
		}

		// And it must say WHY. A prescription with no reason is one a later reader
		// removes as noise, which is how the un-LOWERed form came back.
		if !strings.Contains(msg, "UPPERCASE") {
			t.Errorf("the message prescribes LOWER without saying why, so the next reader "+
				"has no reason to keep it:\n%s", msg)
		}
	}

	// The two modes must not read the same. Asserting each mode separately above
	// cannot see a message that ignores its argument and prints one fixed text,
	// which would leave the "CONVERTed but not LOWERed" reader told to add a
	// CONVERT they already have.
	raw := uuidProjectionMessage("engine/x.go", rawUUIDProjection{
		line: 12, column: "tenant_id", context: "&wf.TenantID", converted: false,
	})
	done := uuidProjectionMessage("engine/x.go", rawUUIDProjection{
		line: 12, column: "tenant_id", context: "&wf.TenantID", converted: true,
	})
	if raw == done {
		t.Errorf("the message does not distinguish a raw projection from an un-LOWERed "+
			"one, so it names the wrong defect for one of them:\n%s", done)
	}
	if !strings.Contains(done, "not LOWERed") || !strings.Contains(raw, "without CONVERT/CAST") {
		t.Errorf("the two modes are different but not for the right reason; a "+
			"CONVERTed-but-not-LOWERed site must be told about the case, not about "+
			"raw bytes\nraw:  %s\ndone: %s", raw, done)
	}
	// And each must name the CAUSE, not only the label: a reader told "without
	// CONVERT/CAST" who is not told what the driver actually returns has no way
	// to judge whether the wrap is the right repair or a ritual.
	if !strings.Contains(raw, "16 raw bytes") {
		t.Errorf("the raw-projection message does not say what the driver returns "+
			"instead of text:\n%s", raw)
	}
	if !strings.Contains(done, "wrong case") {
		t.Errorf("the un-LOWERed message does not name the case as the defect, so a reader "+
			"sees an id that looks canonical and cannot tell what is wrong with it:\n%s", done)
	}
}

// TestTheProjectionWrappingPredicateSeparatesTheForms drives the predicate
// directly, which the tree-scanning guard cannot do: the scan only ever reports
// the sites that FAIL, so a predicate that accepted everything and one that
// refused everything would both be invisible to it on a tree that happens to
// have no bad sites. This is the check that can disagree.
//
// The column is written as X and located by strings.Index, so each case reads as
// the SQL it is.
func TestTheProjectionWrappingPredicateSeparatesTheForms(t *testing.T) {
	cases := []struct {
		proj                       string
		wantConverted, wantLowered bool
		name                       string
	}{
		{"LOWER(CONVERT(NVARCHAR(36), X)) AS id", true, true,
			"the canonical form"},
		{"CONVERT(NVARCHAR(36), X) AS id", true, false,
			"cleat#2993: converted, uppercase, must be reported"},
		{"X", false, false,
			"cleat#2983: raw bytes, must be reported"},
		{"CAST(X AS NVARCHAR(36)) AS id", true, false,
			"CAST is the other conversion and needs LOWER too"},
		{"LOWER(CAST(X AS NVARCHAR(36))) AS id", true, true,
			"and LOWER wrapping a CAST is accepted"},
		{"CONVERT(NVARCHAR(36), LOWER(X)) AS id", true, true,
			"the inverted nesting, measured valid on SQL Server 2022"},
		{"LOWER(ISNULL(CONVERT(NVARCHAR(36), X), '')) AS id", true, true,
			"a LOWER further out still encloses the token"},
		{"ISNULL(CONVERT(NVARCHAR(36), X), '') AS id", true, false,
			"and a non-LOWERing wrapper around the conversion does not"},
		{"LOWER(X) AS id", false, true,
			"bare LOWER: measured to WORK on the server, refused here because the " +
				"character type would be chosen implicitly"},
		{"LEFT(CONVERT(NVARCHAR(36), X), 8) AS id", true, false,
			"a conversion used as an argument is still the projection's conversion"},
		{"LOWER(tenant_id), CONVERT(NVARCHAR(36), X) AS id", true, false,
			"a SIBLING LOWER does not lower this token, and its closing paren must " +
				"not be mistaken for an enclosing one"},
		{"INSERTED.X", false, false,
			"a qualified column, as OUTPUT clauses use"},
	}
	for _, tc := range cases {
		idx := strings.Index(tc.proj, "X")
		if idx < 0 {
			t.Fatalf("case %q has no X to locate", tc.name)
		}
		gotConverted, gotLowered := projectionWrapping(tc.proj, idx)
		if gotConverted != tc.wantConverted || gotLowered != tc.wantLowered {
			t.Errorf("%s: projectionWrapping(%q) = converted=%v lowered=%v, want %v/%v",
				tc.name, tc.proj, gotConverted, gotLowered, tc.wantConverted, tc.wantLowered)
		}
	}
}
