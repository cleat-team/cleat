package plugin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// castsOnWrite reports whether one SQL string both WRITES and casts a value to
// MySQL's JSON type.
//
// Both conditions, because the two CAST(... AS JSON) that legitimately remain
// in plugins/ are blobstore's JSON_CONTAINS READS -- they cast the search
// argument, not the stored value, and `tags` stays JSON precisely so that read
// keeps working. A matcher that flagged every cast would fail on those forever
// and be disabled within a week.
var (
	writesSQL = regexp.MustCompile(`(?is)\b(INSERT\s+(IGNORE\s+)?INTO|UPDATE\s+\w+\s+SET|REPLACE\s+INTO)\b`)
	castsJSON = regexp.MustCompile(`(?is)CAST\s*\([^)]*\bAS\s+JSON\s*\)`)
)

func castsOnWrite(sql string) bool { return writesSQL.MatchString(sql) && castsJSON.MatchString(sql) }

// TestTheMatcherSeesAWriteCastAndNotAReadCast is the known-positive, and the
// must-NOT-match half is the reason it is a table rather than a planted probe.
//
// A planted probe proves the scan below fires once, in the one direction
// already suspected. It cannot reach the other direction -- a matcher loose
// enough to catch every write cast can fire on a read, or on a comment
// describing one, and then the guard is useless in a way nothing surfaces.
func TestTheMatcherSeesAWriteCastAndNotAReadCast(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		// MUST match: the form cleat#1622 removed from eventstore.
		{"insert with cast", "INSERT INTO event_stream (event) VALUES (CAST($3 AS JSON))", true},
		{"insert ignore with cast", "INSERT IGNORE INTO t (c) VALUES (CAST($1 AS JSON))", true},
		{"update with cast", "UPDATE t SET c = CAST($1 AS JSON) WHERE id = $2", true},
		{"replace with cast", "REPLACE INTO t (c) VALUES (CAST($1 AS JSON))", true},
		{"lowercase", "insert into t (c) values (cast($1 as json))", true},
		{"cast spread over lines", "INSERT INTO t (c)\nVALUES (\n  CAST($1 AS JSON)\n)", true},

		// MUST NOT match: blobstore's real reads, which must keep working.
		{"json_contains predicate", "JSON_CONTAINS(i.tags, CAST($1 AS JSON))", false},
		{"select with cast in where", "SELECT id FROM blob_index WHERE JSON_CONTAINS(tags, CAST($1 AS JSON))", false},
		// MUST NOT match: a write with no cast, and a cast-free read.
		{"plain insert", "INSERT INTO t (c) VALUES ($1)", false},
		{"plain select", "SELECT c FROM t WHERE id = $1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := castsOnWrite(tc.sql); got != tc.want {
				t.Errorf("castsOnWrite = %v, want %v for:\n%s", got, tc.want, tc.sql)
			}
		})
	}
}

// TestNoPluginWriteCastsAValueToJSON is cleat#1622's second half, as a guard.
//
// eventstore's MySQL insert was `VALUES (..., CAST($3 AS JSON))`. CAST applies
// the JSON type's number narrowing to the value BEFORE it reaches the column,
// so it degrades even into LONGTEXT -- measured, both columns text, one INSERT:
//
//	via CAST   {"x": 1.2345678901234566e29}
//	direct     {"x":123456789012345678901234567890}
//
// So a cast re-introduced here would silently undo the migration and leave the
// characterization test green over a still-degrading path. The engine needed
// migrations/mysql/071 for exactly this.
func TestNoPluginWriteCastsAValueToJSON(t *testing.T) {
	root := repoRootForCastScan(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "plugins/*.go", "plugins/**/*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	// A scan that read nothing reports a clean tree, which is identical to
	// success. git ls-files is used rather than a walk so a stray worktree
	// cannot be attributed to this repo.
	if len(files) < 50 {
		t.Fatalf("git ls-files matched %d plugin Go files; the scan did not see the tree", len(files))
	}

	checked := 0
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(root, f))
		if rerr != nil {
			continue
		}
		checked++
		// Strip // comments first: this file's own prose quotes the offending
		// SQL, and so does the comment left where it was removed. A guard that
		// cannot tell a statement from a sentence about one reports the
		// retraction as the defect.
		// PER LITERAL, not per file. A file-wide match is wrong at exactly the
		// place that matters: plugins/blobstore/queries.go holds INSERT
		// statements AND the unrelated JSON_CONTAINS read cast, in different
		// literals, so scanning the whole file reports it as a write cast. The
		// first version of this guard did that and flagged blobstore twice.
		for _, lit := range backtickLiterals(stripLineComments(string(body))) {
			if castsOnWrite(lit) {
				bad = append(bad, f)
				break
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test plugin files were read; the scan measured nothing")
	}
	if len(bad) > 0 {
		t.Errorf("these plugin files cast a value to JSON on a write path: %v\n"+
			"CAST(... AS JSON) narrows the number BEFORE it reaches the column, so it "+
			"degrades even into LONGTEXT and undoes cleat#1622's migration. Pass the "+
			"value straight through; the column is text and its CHECK validates it.", bad)
	}
}

func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func repoRootForCastScan(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// backtickLiterals returns the contents of every raw string literal.
//
// Pairs FIRST and filters after. Applying any bound while pairing re-phases the
// rest of the file: a rejected literal leaves the scan mid-string, so the next
// closing backtick pairs with the following opening one and an unrelated
// statement is consumed as a delimiter. That does not merely drop a match, it
// fabricates one out of the Go source between them.
func backtickLiterals(src string) []string {
	var out []string
	for {
		i := strings.Index(src, "`")
		if i < 0 {
			return out
		}
		rest := src[i+1:]
		j := strings.Index(rest, "`")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		src = rest[j+1:]
	}
}
