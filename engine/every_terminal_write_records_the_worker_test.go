package engine

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Every terminal write must record which worker ran the run. cleat#1118.
//
// WHY A SOURCE GUARD AND NOT A BEHAVIOUR TEST. The behaviour tests cover the
// paths a test can reach; this covers the ones it cannot. There are 27 terminal
// UPDATE statements across three dialects -- done, failed, terminated and
// dead_lettered, in the normal, admin and dead-letter paths -- and a behaviour
// test per path would still miss the twenty-eighth somebody adds. A missed site
// is a SILENT BLANK: the column reads NULL, which is indistinguishable from "no
// worker ever held this run", which is a real and expected state.
//
// THE ORDER IS WHAT IS ASSERTED, NOT THE PRESENCE, and that is the whole point
// of the test. `completed_by = assigned_to` has to come BEFORE
// `assigned_to = NULL` in the same SET clause, because the dialects disagree
// about whether it matters:
//
//	SET c = a, a = NULL   ->  postgres w1   mssql w1   mysql w1
//	SET a = NULL, c = a   ->  postgres w2   mssql w2   mysql <null>
//
// PostgreSQL and SQL Server evaluate every right-hand side against the OLD row,
// so order is irrelevant there. MySQL assigns left to right and later
// assignments see earlier ones. So a reordering -- which no reviewer would flag,
// and which two of three dialects forgive -- silently blanks the column on
// MySQL alone. A guard checking only that `completed_by` appears would pass.
//
// PAIR THE BACKTICKS, THEN FILTER. A length bound or a content test applied
// DURING the pairing re-phases the scan: a rejected literal leaves it
// mid-string and the next opening backtick pairs with the wrong one, which
// fabricates literals out of the Go source between them.
func TestEveryTerminalWriteRecordsTheWorker(t *testing.T) {
	files := trackedEngineSources(t)
	if len(files) == 0 {
		t.Fatal("UNMEASURED: no engine sources found, so this checked nothing")
	}

	// A terminal write sets the status COLUMN to a terminal literal. It is not
	// the same as a statement that merely mentions one: the defer-phase
	// transitions write `pending_terminal_status = 'terminated'` while moving
	// status to 'terminating', and they are correctly excluded -- the run has
	// not finished and a later write finalises it.
	realTerminal := regexp.MustCompile(`(?i)(?:^|[^_a-z])status\s*=\s*'(done|failed|terminated|dead_lettered)'`)
	literal := regexp.MustCompile("`([^`]*)`")

	checked, bad := 0, 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		src := string(raw)
		for _, m := range literal.FindAllStringSubmatchIndex(src, -1) {
			lit := src[m[2]:m[3]]
			low := strings.ToLower(lit)
			if !strings.Contains(low, "workflow_instances") || !strings.Contains(low, "set") {
				continue
			}
			if !realTerminal.MatchString(lit) {
				continue
			}
			if !strings.Contains(lit, "assigned_to = NULL") {
				// A terminal write that does not surrender the lease is a
				// different shape and this guard has nothing to say about it.
				continue
			}
			checked++
			line := strings.Count(src[:m[0]], "\n") + 1

			rec := strings.Index(lit, "completed_by = assigned_to")
			clr := strings.Index(lit, "assigned_to = NULL")
			switch {
			case rec < 0:
				bad++
				t.Errorf("%s:%d terminal write does not record the worker.\n\n"+
					"Add `completed_by = assigned_to,` before `assigned_to = NULL`. "+
					"The lease is the only place the identity exists at that moment, "+
					"and this statement erases it (cleat#1118). Leaving it out is not "+
					"a visible failure -- the column reads NULL, which is also what a "+
					"run that was never claimed reads.\n\n%s", f, line, terminalWriteOneLine(lit))
			case rec > clr:
				bad++
				t.Errorf("%s:%d records the worker AFTER clearing the lease.\n\n"+
					"On MySQL that stores NULL: it assigns left to right and later "+
					"assignments see earlier ones. PostgreSQL and SQL Server evaluate "+
					"both against the old row, so this passes on two dialects of three "+
					"and blanks the column on the third, silently.\n\n%s",
					f, line, terminalWriteOneLine(lit))
			}
		}
	}

	// Vacuity. Every assertion above is satisfied by finding nothing, and
	// "nothing" is what a broken extractor returns.
	if checked == 0 {
		t.Fatal("UNMEASURED: matched no terminal writes at all. The scan is broken, " +
			"or the statements moved out of backtick literals -- either way this " +
			"guard passed without checking anything.")
	}
	t.Logf("terminal writes checked: %d, failing: %d", checked, bad)
}

func trackedEngineSources(t *testing.T) []string {
	t.Helper()
	// git ls-files, not a filesystem walk: a walk descends into
	// .claude/worktrees/ and attributes a second copy of the repo to this one.
	out, err := exec.Command("git", "ls-files", ".").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	return files
}

func terminalWriteOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
