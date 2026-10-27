package catalogdiff

// cleat#2882. snapshotMySQL's own header comment resolves the roles
// question as: no migration in migrations/mysql/*.sql issues CREATE ROLE,
// and even if one did, MySQL has no catalog-level marker distinguishing a
// role from a locked ordinary user account (measured against mysql:8.4.11,
// the version this repo pins: both CREATE ROLE and CREATE USER ... ACCOUNT
// LOCK produce an identical mysql.user row, account_locked='Y', with no
// other column telling them apart).
//
// That first half is a fact about the CHAIN, and facts about the chain
// decay silently -- a future migration could add CREATE ROLE without
// anyone revisiting mysql.go's comment, and the gap would be back to
// "checked, none exist" by assumption rather than by measurement. This
// test is the tripwire: it fails the day that happens, forcing the capture
// decision mysql.go's header already names, rather than letting the claim
// rot the way this repo's own CLAUDE.md warns every unchecked claim does.
//
// A regex over migration TEXT, not a live database -- the claim is about
// what the chain contains, not what a database built from it looks like,
// so this needs no CLEAT_TEST_MYSQL and runs on every job unconditionally
// rather than earning a skip-ledger line for a dialect gate it does not
// need. Comments are stripped before matching: a future contributor
// explaining why they are NOT adding CREATE ROLE, in prose, should not trip
// this test -- the exact "a text search cannot tell a thing from a sentence
// about the thing" trap this repo's CLAUDE.md names for this class of scan.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mysqlMigrationCreateRole = regexp.MustCompile(`(?i)\bCREATE\s+ROLE\b`)

// stripSQLComments removes /* ... */ blocks and -- line comments before any
// keyword scan, matching the technique this repo's CLAUDE.md gives for the
// same hazard: a header quoting a CREATE statement, or a comment naming the
// keyword while explaining its absence, would otherwise count as a match.
func stripSQLComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = regexp.MustCompile(`--.*$`).ReplaceAllString(l, "")
	}
	return strings.Join(lines, "\n")
}

func TestNoMySQLMigrationCreatesARoleObject(t *testing.T) {
	root := "../../migrations/mysql"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		if mysqlMigrationCreateRole.MatchString(stripSQLComments(string(b))) {
			t.Errorf("%s issues CREATE ROLE -- snapshotMySQL's header comment "+
				"(migration/catalogdiff/mysql.go, cleat#2882) resolved the roles "+
				"gap on the premise that nothing in this chain creates one; a new "+
				"CREATE ROLE here means that premise no longer holds and the "+
				"capture decision mysql.go's comment describes is now due, not "+
				"the comment alone", e.Name())
		}
	}
	if checked == 0 {
		t.Fatalf("%s contained no .sql files -- this test checked nothing", root)
	}
}
