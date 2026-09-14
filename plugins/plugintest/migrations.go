package plugintest

import (
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// AssertMigrationsDoSomething checks that every migration a plugin declares has
// an effect.
//
// WHY THIS IS SHARED RATHER THAN WRITTEN THIRTEEN TIMES. Thirteen plugins each
// carried their own copy of this loop, and they had already drifted before
// anyone needed to change them -- three checked Up and not Down, the wording
// differed in every one, and some used t.Error where others used t.Errorf with
// an index. Re-derive the population with
//
//	git grep -ln 'Up == ""' -- 'plugins/*/*_test.go'
//
// The drift was harmless while the predicate was "Up and Down are non-empty
// strings". It stopped being harmless when plugin.Migration grew a second way
// to have an effect.
//
// A TenantScoped MIGRATION HAS NO SQL, AND THAT IS THE POINT. It declares
// tables and the runtime emits their row-level policies from the field
// (plugin.applyTenantScoping); there is no statement for an author to write, and
// a Down would have to drop a policy the runtime owns. So the old predicate
// rejected the mechanism by construction -- in BOTH halves, and the Down half
// is the one that bites first: auditlog's v2 failed on "migration Down SQL must
// be non-empty" before it ever reached the Up check.
//
// kvstore, the only plugin to adopt TenantScoped before cleat#1278, happens to
// carry no such test. That is the whole reason the field shipped and sat unused
// for weeks without anyone discovering that thirteen plugins would reject it:
// the one plugin that used it was the one plugin that could not have noticed.
//
// WHAT THIS DELIBERATELY DOES NOT CHECK. Some plugins assert more than the
// common predicate -- eventtriggers requires versions to be sequential from 1,
// ratelimiter requires migration 0's Up to mention its table. Those stay where
// they are. Folding them in here would impose one plugin's rule on twelve
// others and turn this change from an extraction into a behaviour change,
// bundling unrelated failures into a refactor.
//
// Nor does it replace the assertions that name a migration by INDEX
// (kafkaconnect_test.go, scheduledbackup_test.go check migrations[0] and
// migrations[1] specifically). Those are a different predicate -- brittle in
// their own way, since they couple an index to a version -- and pretending they
// are this one would silently drop what they check.
func AssertMigrationsDoSomething(t *testing.T, migrations []plugin.Migration) {
	t.Helper()

	// NON-VACUITY. Every check below is satisfied by an empty slice, which reads
	// exactly like success -- and a plugin whose Migrations() returned nothing
	// is precisely the case worth catching.
	if len(migrations) == 0 {
		t.Fatal("no migrations declared: every assertion here is vacuous rather than passing")
	}
	for _, problem := range migrationProblems(migrations) {
		t.Error(problem)
	}
}

// migrationProblems is the predicate, separated from the reporting so it can be
// tested directly.
//
// A guard that can only be exercised through *testing.T can only be checked by
// running it on a tree that is already fine, which answers "does it pass when
// nothing is wrong" -- the question every broken version of a guard also passes.
// testing.TB cannot be implemented outside the testing package (it has an
// unexported method), so the alternative was no known-positive at all. Returning
// the problems instead makes the guard's own failure cases assertable; see
// migrations_test.go, which declares a migration that does nothing and requires
// this to say so.
func migrationProblems(migrations []plugin.Migration) []string {
	var problems []string
	for i, m := range migrations {
		if m.Version == 0 {
			problems = append(problems, fmt.Sprintf("migration %d: version must be non-zero", i))
		}
		// "Does something" is the predicate, not "has SQL". A migration with
		// no SQL and no declaration is recorded as applied and changes
		// nothing, which is the actual defect worth failing on.
		//
		// SweepTables counts as doing something for the same reason
		// TenantScoped does: the runtime emits the statement, so the author
		// writes a declaration rather than SQL. It makes the runtime GRANT
		// cleat_sweep on a table a cross-tenant sweep touches but which
		// carries no tenant column (cleat#1490) -- without it the sweep gets
		// "permission denied for table X (42501)".
		declares := len(m.TenantScoped) > 0 || len(m.SweepTables) > 0
		if m.Up == "" && !declares {
			problems = append(problems, fmt.Sprintf(
				"migration %d (version %d): has neither Up SQL nor a TenantScoped/"+
					"SweepTables declaration, so applying it does nothing", i, m.Version))
		}
		if m.Down == "" && !declares {
			problems = append(problems, fmt.Sprintf(
				"migration %d (version %d): has no Down SQL and declares no "+
					"TenantScoped/SweepTables entries. A migration that writes SQL must "+
					"be able to undo it; one that only declares scoping or sweep grants "+
					"has nothing to undo, because the runtime owns the policy and the "+
					"grant it emits.", i, m.Version))
		}
	}
	return problems
}
