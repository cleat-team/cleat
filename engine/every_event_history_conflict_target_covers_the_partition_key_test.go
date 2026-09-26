package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryEventHistoryConflictTargetCoversThePartitionKey asserts that every
// ON CONFLICT arbiter against event_history names tenant_id.
//
// WHY THIS IS NOT STYLE. event_history is hash-partitioned on tenant_id
// (cleat#2059), and PostgreSQL requires a conflict target to match a unique
// index -- which, on a partitioned table, must cover the partition key. An
// arbiter of (workflow_id, step) therefore fails AT RUNTIME:
//
//	ERROR: there is no unique or exclusion constraint matching the
//	       ON CONFLICT specification (42P10)
//
// Nothing upstream of the database can catch that. The SQL type-checks, the
// schema applies cleanly, and `go build` has no opinion about a string
// literal. It surfaces as a batch flush that cannot write, which on the event
// path is a lost event rather than a compile error -- and only on the
// PostgreSQL dialect with the partitioned baseline, so a MySQL or MSSQL run
// reports nothing.
//
// WHY ONE TEST AND NOT TWO. The arbiter is written in two independent places:
// Go statements that build the SQL, and the routine bodies the PostgreSQL
// baseline ships (003_procedures.sql, whose two procedures are themselves
// event writers). A guard over either half alone is satisfied by a tree where
// the other half still names the old target -- and the halves are edited by
// different people for different reasons, so "they will move together" is not
// a property. The coverage assertion at the end is the mechanical form of
// that: it fails if either half stops contributing clauses, which is what a
// one-sided guard looks like from the inside.
//
// WHAT IT CANNOT SEE: an arbiter with no column list at all
// (`ON CONFLICT DO NOTHING`), which has no target for this to inspect. The
// keyword reconciliation fails loudly rather than skipping such a form, so
// one this check does not model cannot pass as one it does.

// conflictSrc is one statement-shaped string: a Go string literal, or a whole
// shipped SQL file. `line` is 0 for the SQL half, which is scanned as a file.
type conflictSrc struct {
	half string // "go" or "sql"
	name string
	line int
	text string
}

func (s conflictSrc) where(off int) string {
	if s.line == 0 {
		return s.name
	}
	return s.name + ":" + strconv.Itoa(s.line+strings.Count(s.text[:off], "\n"))
}

func TestEveryEventHistoryConflictTargetCoversThePartitionKey(t *testing.T) {
	var srcs []conflictSrc

	// The Go half. Parsed rather than scanned: the parser pairs delimiters
	// and drops comments by construction, so a statement is reported as a
	// statement and prose about one is not -- and a bound applied while
	// pairing cannot re-phase the rest of the file, which is how CLAUDE.md's
	// backtick survey swallowed an unrelated SELECT.
	fset := token.NewFileSet()
	for _, f := range engineGoFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		file, err := parser.ParseFile(fset, f, b, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			srcs = append(srcs, conflictSrc{"go", f, fset.Position(lit.Pos()).Line, s})
			return true
		})
	}

	// The SQL half: the shipped baseline, which is what a deployment applies.
	sqlFiles, err := exec.Command("git", "ls-files", "../migrations/postgres").Output()
	if err != nil {
		t.Fatalf("git ls-files ../migrations/postgres: %v", err)
	}
	for _, f := range strings.Fields(string(sqlFiles)) {
		if !strings.HasSuffix(f, ".sql") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		srcs = append(srcs, conflictSrc{"sql", f, 0, string(b)})
	}

	// An INSERT names its table here; the arbiter that follows belongs to the
	// nearest one. Searching BACKWARD from the clause rather than forward
	// from a statement is SQL's own scoping rule, and it is what keeps a
	// file-level scan (003 is routines, not statements) from attributing one
	// procedure's arbiter to another's INSERT.
	insertRe := regexp.MustCompile(`(?is)INSERT\s+INTO\s+([A-Za-z_][A-Za-z0-9_$.]*)`)
	arbiterRe := regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(([^)]*)\)`)
	targetlessRe := regexp.MustCompile(`(?is)ON\s+CONFLICT\s+DO\b`)
	colRe := regexp.MustCompile(`(?i)\btenant_id\b`)

	checked := map[string]int{}
	var violations []string
	for _, s := range srcs {
		inserts := insertRe.FindAllStringSubmatchIndex(s.text, -1)
		for _, m := range arbiterRe.FindAllStringSubmatchIndex(s.text, -1) {
			tbl := ""
			for _, ins := range inserts {
				if ins[0] < m[0] {
					tbl = tableName(s.text[ins[2]:ins[3]])
				}
			}
			if tbl != "event_history" {
				continue
			}
			checked[s.half]++
			cols := s.text[m[2]:m[3]]
			if !colRe.MatchString(cols) {
				violations = append(violations, s.where(m[0])+
					": ON CONFLICT ("+strings.Join(strings.Fields(cols), " ")+
					") omits tenant_id, which a partitioned event_history "+
					"refuses with 42P10")
			}
		}
	}

	// Reconcile the KEYWORD against the forms modelled above, in the same text
	// and at the same instant. Without this, a clause this check does not
	// model -- `ON CONFLICT ON CONSTRAINT <name>` -- is indistinguishable
	// from a file with no clause at all, and the guard prints the clean answer
	// on a tree that has one.
	for _, s := range srcs {
		kw := strings.Count(strings.ToUpper(s.text), "ON CONFLICT")
		modelled := len(arbiterRe.FindAllString(s.text, -1)) +
			len(targetlessRe.FindAllString(s.text, -1))
		if kw != modelled {
			violations = append(violations, s.name+": "+strconv.Itoa(kw)+
				" 'ON CONFLICT' keyword(s) but "+strconv.Itoa(modelled)+
				" clause(s) this check can read -- a form it does not model "+
				"(ON CONFLICT ON CONSTRAINT carries no column list)")
		}
	}

	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}

	// Coverage of BOTH halves, asserted rather than assumed. A guard that
	// silently stopped finding one half's clauses reports the same clean
	// result as a correct one; this is where that can disagree.
	if checked["go"] == 0 {
		t.Error("no ON CONFLICT clause against event_history was found in any " +
			"Go statement: this half of the check measured nothing")
	}
	if checked["sql"] == 0 {
		t.Error("no ON CONFLICT clause against event_history was found in " +
			"migrations/postgres: this half of the check measured nothing")
	}
	t.Logf("conflict targets checked: %d in Go, %d in the shipped SQL; none "+
		"may omit tenant_id", checked["go"], checked["sql"])
}

// tableName strips a schema qualifier and any quoting, so event_history,
// "event_history" and public.event_history are one table to this check.
func tableName(ident string) string {
	ident = strings.Trim(strings.TrimSpace(ident), `"`)
	if i := strings.LastIndex(ident, "."); i >= 0 {
		ident = ident[i+1:]
	}
	return strings.ToLower(strings.Trim(ident, `"`))
}
