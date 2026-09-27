package engine

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestEveryEventWriterThatStoresAPayloadRecordsItsEncoding asserts a property of
// the SOURCE, because the alternative -- a behavioural test per writer -- needs
// one fixture per dialect per path and still says nothing about the writer added
// tomorrow.
//
// The predicate is not "every INSERT names payload_encoding". Two writers insert
// a child-start event and touch no request/response column at all
// (store_children.go, mssql_signals_promises.go); for those, NULL is the honest
// value and naming the column would be a claim about bytes that do not exist.
// The predicate is the conditional one:
//
//	an INSERT into event_history that stores request or response
//	must also store payload_encoding.
//
// This exists because that is exactly the gap this test's own change closed.
// cleat#1380 gave five writers one encoding, and this column shipped wired to
// three of them: two Postgres writers in store_event_write.go and the MSSQL
// writer in mssql_events.go stored base64 and recorded nothing, so the read path
// fell back to guessing on their rows -- which is the behaviour cleat#1319
// exists to end. Nothing failed, because a NULL here is indistinguishable from a
// legacy row.
func TestEveryEventWriterThatStoresAPayloadRecordsItsEncoding(t *testing.T) {
	for _, f := range engineGoFiles(t) {
		src := readEngineFile(t, f)
		for _, stmt := range eventHistoryInserts(src) {
			storesPayload := reqOrRespColumn.MatchString(stmt)
			recordsEncoding := strings.Contains(stmt, "payload_encoding")
			if storesPayload && !recordsEncoding {
				t.Errorf("%s: an INSERT into event_history stores request/response but not "+
					"payload_encoding, so the read path must guess at its rows (cleat#1319).\n"+
					"Pass stored.Encoding, or payloadEncodingFor(rec) where the writer does not "+
					"call encodeEventForStorage.\nstatement:\n%s", f, truncate(stmt))
			}
		}
	}
}

// reqOrRespColumn matches the column names in an INSERT's column list. The word
// boundaries matter: "defer_description" contains neither, but a substring
// search for "request" would also match "request_b64" in a payload JSON literal,
// which is not a column.
var reqOrRespColumn = regexp.MustCompile(`(^|[ \t(,])(request|response)[ \t,)\n]`)

// eventHistoryInserts returns the column list of each INSERT INTO event_history
// in src -- from the statement keyword to the first VALUES or SELECT, which is
// where every column name in an INSERT appears and where no payload JSON literal
// or WHERE clause can reach.
//
// Bounding the slice at the VALUES/SELECT rather than by a character count is
// deliberate: a length bound applied while scanning is how CLAUDE.md's
// "pair first, filter after" rule was learned, and an over-long slice here would
// reach into the argument list where the word "response" appears in ordinary Go.
func eventHistoryInserts(src string) []string {
	var out []string
	for _, idx := range insertKeyword.FindAllStringIndex(src, -1) {
		rest := src[idx[0]:]
		if end := valuesOrSelect.FindStringIndex(rest); end != nil {
			out = append(out, rest[:end[1]])
		}
	}
	return out
}

var (
	insertKeyword  = regexp.MustCompile(`INSERT INTO event_history`)
	valuesOrSelect = regexp.MustCompile(`(?i)\b(VALUES|SELECT)\b`)
)

// engineGoFiles lists the package's tracked non-test sources.
//
// git ls-files rather than a filesystem walk, so a scratch checkout under
// .claude/worktrees/ cannot contribute a file -- and DEDUPED, because a file
// with an unresolved merge is listed once per stage, which silently triples a
// per-file scan's population.
//
// The pathspec is "." and not "engine/": go test runs with the package
// directory as the working directory, so an "engine/" pathspec matches nothing
// and the scan reports a clean tree over zero files. That is the exact failure
// this file exists to prevent, and it happened here first -- which is what the
// empty-population Fatal below is for.
func engineGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", ".").Output()
	if err != nil {
		// Fatal and not Skip. git is always available where this suite runs,
		// so there is no "the resource is optional" case to honour here -- and
		// a skip would silently disable the scan, which is the exact failure
		// the empty-population Fatal below exists to prevent. A guard that can
		// opt itself out is not a guard (scripts/check-skips.sh case (c)).
		t.Fatalf("git ls-files: %v", err)
	}
	seen := map[string]bool{}
	var files []string
	for _, f := range strings.Fields(string(out)) {
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") || seen[f] {
			continue
		}
		seen[f] = true
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no engine sources found: the scan would pass vacuously")
	}
	return files
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

func readEngineFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
