package migration

// The migration path depends on SESSION state surviving between statements,
// and two operations documents recommend a pooling mode that does not provide
// it. cleat#1310.
//
// docs/operations/postgresql-sizing.md and docs/operations/tuning.md now carry
// a note saying migrations must not go through `pool_mode = transaction`, and
// pointing at --migrate-db as the seam. That note is only true while these
// locks are SESSION-scoped. Swap `pg_advisory_lock` for `pg_advisory_xact_lock`
// and the constraint evaporates -- and the note becomes the opposite of the
// truth, telling an operator to keep a direct connection they no longer need
// and, worse, implying a hazard that has been fixed.
//
// A prose warning about a code property rots silently, because nothing
// re-reads the prose. This fails instead. It is the same move as
// engine/hostabi_runtime_parity_test.go's TestParityCoversEveryRegistered-
// HostFunction, which asserts that a filter has not come back.
//
// NO DATABASE REQUIRED, deliberately. This is a property of the SQL the
// packages contain, so it runs in every job rather than only where a DSN is
// configured -- and a guard that skips is a guard that reports success.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sessionScopedLockSites are the files whose advisory locks the documented
// constraint rests on. Both are on the worker's boot path.
var sessionScopedLockSites = []string{
	"migration/runner.go",
	"plugin/migration.go",
}

func TestMigrationLocksAreStillSessionScoped(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	root := strings.TrimSpace(string(out))

	for _, rel := range sessionScopedLockSites {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v -- the file moved, and the note in "+
				"docs/operations/postgresql-sizing.md cites it by name", rel, err)
		}
		body := string(src)

		// THE TRANSACTION-SCOPED CASE FIRST, and the order is not cosmetic.
		// A switch to pg_advisory_xact_lock also makes the presence check below
		// fail, because "pg_advisory_xact_lock(" does not contain
		// "pg_advisory_lock(" -- so checking presence first reported "the lock
		// was removed, migrations are no longer serialised", which is alarming
		// and wrong. Found by falsifying this test rather than by reading it.
		if strings.Contains(body, "pg_advisory_xact_lock") {
			t.Errorf("%s now uses pg_advisory_xact_lock, which is transaction-scoped.\n\n"+
				"That is a fine change and it invalidates a note this repo ships: "+
				"docs/operations/postgresql-sizing.md and docs/operations/tuning.md tell "+
				"operators that migrations must not run through `pool_mode = transaction` "+
				"and to use --migrate-db. Update both, then this assertion (cleat#1310).", rel)
			continue
		}

		// Present, and matched on the EXECUTED SQL rather than the bare name:
		// both files discuss pg_advisory_lock in comments, so a name-only
		// search is satisfied by prose about the call after the call is gone.
		if !strings.Contains(body, `"SELECT pg_advisory_lock(`) {
			t.Errorf("%s no longer executes SELECT pg_advisory_lock.\n\n"+
				"If the lock was removed, migrations are no longer serialised across "+
				"workers and BOTH operations documents are wrong in a new way. If the "+
				"statement was rephrased, update this assertion (cleat#1310).", rel)
		}
	}
}

// The plugin path additionally holds `SET search_path` across its run, which a
// transaction-mode pooler does not preserve either. Asserted separately because
// it is a different mechanism with the same consequence, and because the core
// runner is deliberately immune to it -- runner.go schema-qualifies
// public.schema_migrations for exactly this reason, so a reader who checks only
// the runner concludes the whole path is safe.
func TestThePluginMigrationPathStillHoldsSearchPathAcrossItsRun(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "plugin/migration.go"))
	if err != nil {
		t.Fatalf("read plugin/migration.go: %v", err)
	}
	body := string(src)

	// Not t.Skip. The precondition is always satisfiable in this repo -- the
	// file either sets search_path or it does not -- so a skip here is
	// scripts/check-skips.sh case (c): a guard reporting success on the one
	// tree it exists to check. If this stops being true it is a change someone
	// made, and they are the person who should hear about the note.
	if !strings.Contains(body, `"SET search_path`) {
		t.Fatal("plugin/migration.go no longer sets search_path.\n\n" +
			"That is a fine change, and it narrows a note this repo ships: the pooling " +
			"warning in docs/operations/postgresql-sizing.md and docs/operations/tuning.md " +
			"lists search_path alongside the advisory lock. Drop that row, then this " +
			"assertion (cleat#1310).")
	}
	if !strings.Contains(body, `"RESET search_path"`) {
		t.Error("plugin/migration.go sets search_path without resetting it. On a pooled " +
			"connection that leaks to whoever gets the connection next (cleat#1310).")
	}
}
