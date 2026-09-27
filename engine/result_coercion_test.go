package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryResultWriteIsCoerced is the guard for the defect that made
// continue-as-new fail outright on PostgreSQL.
//
// The result column is jsonb on PostgreSQL and JSON on MySQL, and the string a
// store is handed is not guaranteed to be either. coerceResultJSON exists for
// exactly that: it turns "" into {} and replaces anything unparseable, loudly.
//
// It was called from one path out of three. FinalizeWorkflowSegment coerced;
// ContinueAsNew and CompleteWorkflow wrote the raw string. A workflow that
// continues as new never returned a value, so its result is "", and every such
// run died with
//
//	pq: invalid input syntax for type json (22P02)
//
// Continue-as-new therefore did not work at all, and the failure named the
// database rather than the missing call.
//
// The check is source-derived rather than a list of function names, because a
// list of function names is what the three call sites already were.
func TestEveryResultWriteIsCoerced(t *testing.T) {
	// An UPDATE of workflow_instances that assigns the result column, in any
	// dialect's placeholder style: result = $3, result = ?, result = @p3.
	//
	// Scoped to workflow_instances on purpose. workflow_promises and
	// workflow_update_requests have their own `result` columns carrying a value
	// the guest supplied and already validated; coercing those would silently
	// rewrite a caller's data, which is a different decision from making a
	// workflow's own result storable.
	writesResult := regexp.MustCompile(`(?is)UPDATE\s+(?:dbo\.)?workflow_instances\b.{0,400}?\bresult\s*=\s*[$?@]`)

	var offenders []string
	var checked int

	for _, path := range lifecycleSourcesForTest(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, fn := range splitTopLevelFuncs(string(src)) {
			// Comments are stripped before anything is matched. The first
			// version of this test did not, and its own explanatory comment
			// -- which names coerceResultJSON while describing the defect --
			// counted as a call. Backing the fix out left the test green.
			//
			// That is the fourth time in this repository that a scanner has
			// read a sentence ABOUT a thing as the thing: a shellcheck path
			// list, an import-name count, an SDK table, and now this. A text
			// search cannot tell a use from a mention, so the mentions have
			// to go before the search runs.
			code := stripComments(fn.body)
			if !writesResult.MatchString(code) {
				continue
			}
			checked++
			// Covered either by coercing here, or by receiving an
			// already-coerced value. The second is not a loophole: the
			// coerced value travels under the name resultJSON precisely so
			// that a helper taking one is declaring where its value came
			// from. The MSSQL store splits every write into an outer method
			// that retries and an inner *Once that runs the statement, and
			// the outer one is the right place to coerce.
			if !strings.Contains(code, "coerceResultJSON(") &&
				!strings.Contains(code, "resultJSON string") {
				offenders = append(offenders,
					filepath.Base(path)+":"+fn.name)
			}
		}
	}

	// Input assertion. A scan that finds no result writes reports perfect
	// coverage, which is the failure this test is about.
	if checked < 3 {
		t.Fatalf("found only %d function(s) assigning the result column; expected at "+
			"least 3 (one per dialect). The queries moved or changed shape and this "+
			"test is checking a set it never found.", checked)
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d function(s) write the result column without coercing it: %s\n\n"+
			"result is jsonb on PostgreSQL and JSON on MySQL. The string handed to a "+
			"store is not guaranteed to be valid JSON -- a workflow that continues as "+
			"new never returned a value, so its result is the empty string -- and "+
			"writing it raw fails the whole run with a syntax error that names the "+
			"database rather than the missing call. Pass it through coerceResultJSON.",
			len(offenders), strings.Join(offenders, ", "))
	}
}

type topLevelFunc struct{ name, body string }

// stripComments removes // and /* */ comments, leaving string literals alone.
// SQL lives in raw string literals here, so those must survive intact -- and a
// // inside one is not a comment.
func stripComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	var inLine, inBlock, inStr, inRaw bool
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out.WriteByte(c)
			}
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				i++
			}
		case inRaw:
			out.WriteByte(c)
			if c == '`' {
				inRaw = false
			}
		case inStr:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				out.WriteByte(src[i])
			} else if c == '"' {
				inStr = false
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			i++
		default:
			if c == '`' {
				inRaw = true
			} else if c == '"' {
				inStr = true
			}
			out.WriteByte(c)
		}
	}
	return out.String()
}

// splitTopLevelFuncs carves a Go source file into top-level functions by their
// declaration lines. Deliberately textual rather than AST-based: the subject is
// SQL inside string literals, which no Go AST walk reaches any more directly,
// and the declaration line is the only anchor needed.
func splitTopLevelFuncs(src string) []topLevelFunc {
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)\(`)
	locs := decl.FindAllStringSubmatchIndex(src, -1)
	out := make([]topLevelFunc, 0, len(locs))
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, topLevelFunc{
			name: src[loc[2]:loc[3]],
			body: src[loc[0]:end],
		})
	}
	return out
}

func lifecycleSourcesForTest(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading engine sources: %v", err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		t.Fatal("no engine sources found")
	}
	return out
}
