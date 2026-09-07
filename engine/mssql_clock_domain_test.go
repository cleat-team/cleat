package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoMSSQLStatementUsesTheServersLocalClock is a SOURCE guard, and it is a
// source guard on purpose.
//
// # The defect
//
// The engine's MSSQL statements wrote `completed_at` with two different
// clocks. Every ordinary terminal write used `SYSUTCDATETIME()`; three did
// not, and all three were on the terminate and defer-phase paths:
//
//	engine/mssql_defer_phase.go:34   FinalizeDeferPhase
//	engine/mssql_defer_phase.go:90   ExpireDeferPhases
//	engine/mssql_operations.go:284   TerminateWorkflow
//
// So on SQL Server a workflow that finished normally got a UTC `completed_at`
// and one that was terminated got the server's LOCAL time. Not a skew of
// milliseconds -- the server's UTC offset, potentially hours, and signed.
//
// `completed_at` is `DATETIMEOFFSET` (migrations/mssql/001_schema.sql:187) and
// both functions land in it carrying `+00:00`. That is what makes `GETDATE()`
// worse here than an ordinary local-time column: the value is not merely local,
// it is local time **labelled as UTC**, so nothing downstream can detect or
// correct it. The column's own DEFAULTs in that same file use
// `SYSUTCDATETIME()`, which is a third witness to which one was meant.
//
// # Why this is not a behavioural test
//
// No behavioural test in this repo can distinguish the two functions, and none
// ever will while the test databases are containers. Measured 2026-09-06
// against the SQL Server on 1433:
//
//	SELECT DATEDIFF(minute, SYSUTCDATETIME(), GETDATE())  ->  0
//	SELECT CAST(SYSDATETIMEOFFSET() AS NVARCHAR(50))      ->  ... +00:00
//
// The container runs UTC, so the two functions are the same function there.
// Every test database in the project is such a container. A test asserting
// "these timestamps agree" would pass identically before and after the fix --
// it would be a check that cannot see the state it looks for, which CLAUDE.md's
// "Is this result real?" section is entirely about. The honest guard is over
// the text, so this is a guard over the text.
//
// # Comments are stripped, and this file excludes itself
//
// Both are needed, and the second was found the hard way: with comments
// stripped this guard still reported three offenders on its first run, all of
// them in its own source. Not in its prose -- in its STRING LITERALS. The token
// it searches for, the token in its failure message, and the token in its
// success log are all real code, so stripping comments cannot reach them.
//
// That is the trap CLAUDE.md records as "a text search cannot tell a thing from
// a sentence about the thing", one level further in than the version usually
// met: a sentence about the thing is not only prose, it can be a string
// literal, and a scan over source cannot distinguish a statement it should
// reject from the description of one. So this file is excluded by name, which
// is safe because it contains no SQL -- and the exclusion is asserted to have
// matched, so renaming the file cannot silently turn the guard on itself again.
func TestNoMSSQLStatementUsesTheServersLocalClock(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing engine/*.go: %v", err)
	}
	if len(files) < 50 {
		t.Fatalf("found only %d .go files in engine/; there were 174 on 2026-09-06. "+
			"A glob that matches almost nothing passes vacuously.", len(files))
	}

	// This file's own string literals contain the token; see the doc comment.
	const selfName = "mssql_clock_domain_test.go"

	var offenders []string
	scanned, skippedSelf := 0, 0
	for _, f := range files {
		if f == selfName {
			skippedSelf++
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		scanned++
		for i, line := range strings.Split(string(b), "\n") {
			code := stripGoLineComment(line)
			if strings.Contains(code, "GETDATE()") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", f, i+1, strings.TrimSpace(line)))
			}
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("%d SQL Server statement(s) read the server's LOCAL clock:\n\n  %s\n\n"+
			"Use SYSUTCDATETIME(). completed_at and created_at are DATETIMEOFFSET and every\n"+
			"other writer -- including the column DEFAULTs in migrations/mssql/001_schema.sql --\n"+
			"is UTC, so a GETDATE() value lands beside them carrying +00:00 while actually\n"+
			"being local time. On a UTC server the two are identical, which is why no\n"+
			"behavioural test can catch this and why the guard is here instead.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// If this file were renamed, the exclusion above would stop matching and
	// the guard would report itself -- confusingly, since the report would name
	// real code. Fail on the rename instead, at the point that explains it.
	if skippedSelf != 1 {
		t.Fatalf("expected to exclude exactly one file (%s), excluded %d. "+
			"If this file was renamed, update selfName; the exclusion exists because "+
			"this file's own string literals contain the token being searched for.",
			selfName, skippedSelf)
	}
	t.Logf("%d files in engine/ scanned (1 excluded: itself), no local-clock call outside comments", scanned)
}

// stripGoLineComment removes a // comment from a line, leaving string literals
// alone. It is not a Go parser and does not need to be: it exists so that prose
// ABOUT a SQL function is not read as a use of it, and the only way a `//` can
// appear before the comment on these lines is inside a string, which is checked
// by counting quotes and backticks to its left.
func stripGoLineComment(line string) string {
	inQuote, inBacktick := false, false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '`':
			if !inQuote {
				inBacktick = !inBacktick
			}
		case '"':
			if !inBacktick && (i == 0 || line[i-1] != '\\') {
				inQuote = !inQuote
			}
		case '/':
			if !inQuote && !inBacktick && i+1 < len(line) && line[i+1] == '/' {
				return line[:i]
			}
		}
	}
	return line
}
