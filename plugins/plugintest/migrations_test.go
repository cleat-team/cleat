package plugintest

import (
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// The guard's own known-positive.
//
// "It passes on a tree that is fine" is satisfied by every broken version of a
// guard. What separates a working one is that it REPORTS a case already known
// to be bad -- so each case here declares a migration that is wrong in a
// specific way and requires the predicate to say so.
func TestMigrationProblemsReportsWhatItShould(t *testing.T) {
	for _, tc := range []struct {
		name        string
		migrations  []plugin.Migration
		wantProblem string // empty means: must report nothing
	}{
		{
			name:       "ordinary SQL migration",
			migrations: []plugin.Migration{{Version: 1, Up: "CREATE TABLE t (id int)", Down: "DROP TABLE t"}},
		},
		{
			// THE CASE THE OLD PREDICATE REJECTED. A TenantScoped migration
			// carries no SQL in either direction: the runtime emits the policy
			// from the field, and a Down would drop a policy the runtime owns.
			name:       "TenantScoped migration, no SQL either way",
			migrations: []plugin.Migration{{Version: 2, TenantScoped: []string{"audit_events"}}},
		},
		{
			// The defect the guard exists for, and it is NOT "has no SQL" --
			// it is "has no effect".
			name:        "neither SQL nor TenantScoped",
			migrations:  []plugin.Migration{{Version: 3}},
			wantProblem: "so applying it does nothing",
		},
		{
			name:        "SQL with no way back",
			migrations:  []plugin.Migration{{Version: 4, Up: "CREATE TABLE t (id int)"}},
			wantProblem: "has no Down SQL",
		},
		{
			name:        "version zero",
			migrations:  []plugin.Migration{{Up: "CREATE TABLE t (id int)", Down: "DROP TABLE t"}},
			wantProblem: "version must be non-zero",
		},
		{
			// A TenantScoped migration is still required to have a version --
			// the exemption is about SQL, not about identity.
			name:        "TenantScoped but unversioned",
			migrations:  []plugin.Migration{{TenantScoped: []string{"t"}}},
			wantProblem: "version must be non-zero",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := migrationProblems(tc.migrations)
			if tc.wantProblem == "" {
				if len(got) != 0 {
					t.Errorf("reported %d problem(s) for a migration that is fine:\n  %s",
						len(got), strings.Join(got, "\n  "))
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("reported nothing. This case is KNOWN to be bad (%q), so a guard "+
					"that stays silent here passes every tree, including broken ones.",
					tc.wantProblem)
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, tc.wantProblem) {
				t.Errorf("reported a problem, but not the expected one.\ngot:\n  %s\nwant it to mention: %q",
					strings.Join(got, "\n  "), tc.wantProblem)
			}
		})
	}
}

// The index in the message has to be the index, so a plugin with several
// migrations can tell which one is at fault.
func TestMigrationProblemsNamesTheOffendingIndex(t *testing.T) {
	got := migrationProblems([]plugin.Migration{
		{Version: 1, Up: "CREATE TABLE a (id int)", Down: "DROP TABLE a"},
		{Version: 2, Up: "CREATE TABLE b (id int)", Down: "DROP TABLE b"},
		{Version: 3}, // the bad one
	})
	if len(got) == 0 {
		t.Fatal("reported nothing for a slice containing a migration that does nothing")
	}
	if !strings.Contains(got[0], "migration 2") {
		t.Errorf("problem names the wrong migration: %q\n\nwant it to name index 2, the only "+
			"bad one. An index that is always 0, or the length, is worse than none -- it "+
			"sends the reader to a migration that is fine.", got[0])
	}
	if !strings.Contains(got[0], "version 3") {
		t.Errorf("problem does not name the VERSION: %q. The index is a slice position and "+
			"the version is what the author wrote; a reader needs the second to find it.", got[0])
	}
}
