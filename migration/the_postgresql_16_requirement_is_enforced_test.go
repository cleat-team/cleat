package migration

import (
	"os"
	"strings"
	"testing"
)

// cleat#1490's migration 077 made PostgreSQL 16 a REQUIREMENT rather than a
// verified claim: it grants the sweep role `WITH INHERIT FALSE`, which is 16
// syntax. These pin the threshold and the message.
//
// WHAT CI CANNOT COVER, said here rather than left as a gap somebody discovers:
// the refusal against a real old server. CI runs PostgreSQL 16 and there is no
// version matrix for any dialect (tiers.yaml, `dialect_versions`). Verified by
// hand against a real postgres:15.19 container instead -- the full migration
// set refused with this message and left the database with zero tables and no
// cleat_sweep role, and the same binary applied all 77 migrations on 16.
func TestPostgresBelowSixteenIsRefusedWithAnActionableMessage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		num     int
		shown   string
		refused bool
	}{
		{"15.19, the version measured by hand", 150019, "15.19 (Debian)", true},
		{"15.0, the boundary below", 150000, "15.0", true},
		{"16.0 exactly, the first allowed", 160000, "16.0", false},
		{"16.15, what CI runs", 160015, "16.15", false},
		{"17, newer than required", 170004, "17.4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := postgresVersionError(tc.num, tc.shown)
			if tc.refused && err == nil {
				t.Fatalf("server_version_num=%d was accepted; 077's GRANT ... WITH INHERIT "+
					"FALSE is 16 syntax and fails on it", tc.num)
			}
			if !tc.refused {
				if err != nil {
					t.Fatalf("server_version_num=%d was refused: %v", tc.num, err)
				}
				return
			}
			// The message is the whole point of the check -- the migration
			// already fails on its own without it, just unreadably. So the
			// message is asserted, not merely the refusal.
			msg := err.Error()
			for _, want := range []string{
				"requires PostgreSQL 16 or later",
				tc.shown,                   // which server this is
				"INHERIT FALSE",            // what actually needs it
				"Nothing has been applied", // what state the operator is in
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not mention %q, so an operator cannot act on it:\n%s", want, msg)
				}
			}
		})
	}
}

// The other half: something has to call it. A pre-flight with no caller is the
// shape cleat#1470 shipped once already (EvictIdle).
func TestTheVersionPreflightRunsBeforeAnyMigration(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatalf("read runner.go: %v", err)
	}
	var code []string
	for _, l := range strings.Split(string(src), "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		code = append(code, l)
	}
	body := strings.Join(code, "\n")

	call := strings.Index(body, "r.checkServerVersion(")
	if call < 0 {
		t.Fatalf("Run does not call checkServerVersion, so a too-old server still meets " +
			"migration 077 as a bare syntax error")
	}
	// Order matters as much as presence: after the tracking table or after
	// readMigrations would still be before 077, but the promise in the message
	// is "nothing has been applied", and only a check ahead of applyMigration
	// can keep it.
	apply := strings.Index(body, "r.applyMigration(")
	if apply >= 0 && call > apply {
		t.Errorf("checkServerVersion is called after applyMigration; the refusal claims " +
			"\"Nothing has been applied\" and that would no longer be true")
	}
}
