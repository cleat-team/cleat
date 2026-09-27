package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// The report's two verdicts are the whole instrument, so each is proved able to
// come out BOTH ways. A verdict that can only say one thing is the fault
// cleat#982 has already paid for eleven times: ForeignSessions reported "none"
// on runs where the interference was known to be real, and nothing in its
// output distinguished that from a run where it could not have said otherwise.

func TestABlanketDeleteIsToldApartFromATargetedOne(t *testing.T) {
	// Both statement texts are what the trigger actually captured on
	// 2026-09-16, copied from a measured run rather than invented: a
	// parameterized targeted delete arrives with go-mssqldb's sp_executesql
	// preamble, which a naive "does it contain WHERE" would also have to cope
	// with.
	cases := []struct {
		name    string
		stmt    string
		blanket bool
	}{
		{"CleanupMSSQLTestData's shape", `DELETE FROM dbo.workflow_instances`, true},
		{"schema-qualified and bracketed", `DELETE FROM [dbo].[workflow_instances]`, true},
		{"a targeted delete", `(@p1 nvarchar(4))DELETE FROM dbo.rows_t WHERE id = @p1`, false},
		{"multi-line, as captured", "DELETE FROM dbo.workflow_instances\n\tWHERE id = @p1", false},
		{"not a delete at all", `UPDATE dbo.workflow_instances SET status='ready'`, false},
		{"nothing captured", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MSSQLDeletion{Stmt: c.stmt}.Blanket()
			if got != c.blanket {
				t.Errorf("Blanket() = %v, want %v, for %q", got, c.blanket, c.stmt)
			}
		})
	}

	// The floor. Without it every case above could be passing because Blanket
	// returns a constant, and a report that can never say BLANKET is a report
	// that will never name CleanupMSSQLTestData -- the one suspect this whole
	// issue keeps circling.
	var sawTrue, sawFalse bool
	for _, c := range cases {
		if c.blanket {
			sawTrue = true
		} else {
			sawFalse = true
		}
	}
	if !sawTrue || !sawFalse {
		t.Fatalf("this table must contain at least one case of each verdict; "+
			"blanket=%v targeted=%v", sawTrue, sawFalse)
	}
}

func TestADeletionByAnotherProcessIsReportedAsForeign(t *testing.T) {
	// selfPID is the seam foreign_sessions.go already uses for exactly this:
	// a test claims to be a different process, so this process's own
	// activity must then be reported as foreign. Without the swap, "not
	// foreign" is what a comparison against the wrong field returns too.
	realPID := selfPID()

	ours := MSSQLDeletion{HostPID: realPID}
	if ours.Foreign() {
		t.Errorf("a deletion by pid %d is reported as foreign to pid %d", realPID, realPID)
	}

	orig := selfPID
	selfPID = func() int { return realPID + 1 }
	defer func() { selfPID = orig }()

	if !ours.Foreign() {
		t.Errorf("with this process claiming pid %d, a deletion by pid %d is still not "+
			"reported as foreign -- the report cannot produce the verdict it exists for",
			realPID+1, realPID)
	}

	// An unrecorded pid is not a foreign one. SQL Server reports
	// host_process_id as NULL for some client stacks, and reading that zero as
	// "a different process" would manufacture an interloper out of a missing
	// field -- an over-report, in an investigation where four sessions have
	// already chased mechanisms that were not there.
	if (MSSQLDeletion{HostPID: 0}).Foreign() {
		t.Error("a deletion with no recorded host_process_id is reported as foreign")
	}
}

// TestTheAuditedCleanupStillIssuesADelete guards the instrument's blind spot.
//
// An AFTER DELETE trigger does not fire for TRUNCATE TABLE. Measured
// 2026-09-16: two rows removed by TRUNCATE produced zero audit rows, with no
// error and no empty-result signal to notice.
//
// CleanupMSSQLTestData issues DELETE FROM today, so the suspect this
// instrument was built to catch is visible -- and it is ONE EDIT away from not
// being. Somebody converting that loop to TRUNCATE for speed would be making a
// reasonable change that silently blinds the instrument, and the failure would
// look exactly like "nothing deleted anything", which is the answer cleat#982
// has been given too many times already.
func TestTheAuditedCleanupStillIssuesADelete(t *testing.T) {
	const file = "mssql_schema.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.FuncDecl); ok && d.Name.Name == "CleanupMSSQLTestData" {
			fn = d
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatalf("CleanupMSSQLTestData is not in %s any more. It is what the cleat#982 "+
			"deletion audit is aimed at; find where it went and re-point this guard.", file)
	}

	// THE LITERALS, NOT THE TEXT. This function's comments say "DELETE" five
	// times and describe the unqualified DELETE at length, so a guard reading
	// its source text is satisfied by prose about deleting and would be
	// satisfied by a comment saying "do not use TRUNCATE here" in the other
	// direction -- failing on a comment that AGREES with it. A string literal
	// is where the executed statement lives.
	var literals []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil {
			literals = append(literals, strings.ToUpper(v))
		}
		return true
	})

	// The known-positive for this guard's own parse. "No TRUNCATE in the
	// literals" is also what a guard that extracted no literals at all
	// reports, and that is the more likely way this goes wrong as the
	// function is refactored -- a statement moved into a helper or a package
	// var leaves this body with nothing in it and the guard silently green.
	var sawDelete bool
	for _, l := range literals {
		if strings.Contains(l, "DELETE FROM") {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Fatalf("this guard extracted %d string literal(s) from CleanupMSSQLTestData and "+
			"none of them is a DELETE FROM.\n\n"+
			"Either the cleanup no longer deletes -- in which case the cleat#982 audit is "+
			"blind and this is the failure that says so -- or the statement moved out of "+
			"this function body and the guard is now reading nothing. Re-point it before "+
			"believing a green run.", len(literals))
	}

	for _, l := range literals {
		if strings.Contains(l, "TRUNCATE") {
			t.Errorf("CleanupMSSQLTestData now issues a TRUNCATE.\n\n" +
				"An AFTER DELETE trigger does not fire for TRUNCATE TABLE (measured " +
				"2026-09-16: two truncated rows, zero audit rows, no error). The " +
				"cleat#982 deletion audit in mssql_row_disappearance.go would go on " +
				"reporting an empty audit, which is indistinguishable from nothing " +
				"having deleted anything.\n\n" +
				"If the change is wanted, the audit needs another mechanism -- an " +
				"extended events session, say -- before this guard is relaxed.")
		}
	}
}
