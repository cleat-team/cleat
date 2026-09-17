package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1702 split workflow_defs.deprecated into two columns: disabled_at
// (admission control) and gc_eligible (collection eligibility). These tests are
// the ONLY thing in the tree that exercises the difference.
//
// WHY THAT IS NOT AN OVERSTATEMENT, AND WHY IT MATTERS. Every shipped path that
// writes either column writes BOTH -- the three stores' MarkVersionDeprecated
// and WorkflowLoader.Deprecate. (An earlier draft of this comment said
// `cleatctl versions deprecate` was the only one; WorkflowLoader.Deprecate is a
// second, and finding it is why the both-move-together property is asserted per
// writer below rather than argued once from a single call site.)
// So in every deployment the two columns agree in every row, forever. A reader
// who measures production will find them identical and conclude one is
// redundant -- and they will be right about the data and wrong about the
// design. The equality is INCIDENTAL, NOT INVARIANT.
//
// That is IMPROVEMENT-PLAN 3.211's third state, "bound but never executed": a
// difference that is permitted but never demonstrated is indistinguishable from
// a difference that does not exist. So the demonstration lives here rather than
// in any production row. If these tests are deleted, nothing stops the columns
// being merged, and merging them restores the hazard the split removed -- a
// generic write to disabled_at arming a permanent deletion.

// gcSplitDialect is one dialect's worth of what these tests need: a store to
// exercise the real paths, plus a connection that can write ONE column at a
// time, which no exported API can do by design.
type gcSplitDialect struct {
	name    string
	dialect testutil.Dialect
	setup   func(*testing.T) (WorkflowStore, *sql.DB)
	// setOneColumn writes exactly one of the two columns, leaving the other
	// as it is. Dialect-specific because the placeholders are.
	disableOnly func(*testing.T, *sql.DB, string, int)
	armOnly     func(*testing.T, *sql.DB, string, int)
	read        func(*testing.T, *sql.DB, string, int) (disabled bool, eligible bool)
}

func gcSplitDialects() []gcSplitDialect {
	pgSet := func(col, val string) func(*testing.T, *sql.DB, string, int) {
		return func(t *testing.T, db *sql.DB, name string, version int) {
			t.Helper()
			if _, err := db.Exec(
				`UPDATE workflow_defs SET `+col+` = `+val+` WHERE name = $1 AND version = $2`,
				name, version); err != nil {
				t.Fatalf("set %s: %v", col, err)
			}
		}
	}
	mySet := func(col, val string) func(*testing.T, *sql.DB, string, int) {
		return func(t *testing.T, db *sql.DB, name string, version int) {
			t.Helper()
			if _, err := db.Exec(
				`UPDATE workflow_defs SET `+col+` = `+val+` WHERE name = ? AND version = ?`,
				name, version); err != nil {
				t.Fatalf("set %s: %v", col, err)
			}
		}
	}
	msSet := func(col, val string) func(*testing.T, *sql.DB, string, int) {
		return func(t *testing.T, db *sql.DB, name string, version int) {
			t.Helper()
			if _, err := db.Exec(
				`UPDATE workflow_defs SET `+col+` = `+val+` WHERE name = @p1 AND version = @p2`,
				name, version); err != nil {
				t.Fatalf("set %s: %v", col, err)
			}
		}
	}
	readWith := func(q string) func(*testing.T, *sql.DB, string, int) (bool, bool) {
		return func(t *testing.T, db *sql.DB, name string, version int) (bool, bool) {
			t.Helper()
			var disabledAt *time.Time
			var eligible bool
			if err := db.QueryRow(q, name, version).Scan(&disabledAt, &eligible); err != nil {
				t.Fatalf("read columns: %v", err)
			}
			return disabledAt != nil, eligible
		}
	}
	return []gcSplitDialect{
		{
			name:    "postgres",
			dialect: testutil.DialectPostgres,
			setup: func(t *testing.T) (WorkflowStore, *sql.DB) {
				db := testutil.TestDB(t, testutil.DialectPostgres)
				testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
				return NewPostgresStore(db), db
			},
			disableOnly: pgSet("disabled_at", "now()"),
			armOnly:     pgSet("gc_eligible", "true"),
			read: readWith(`SELECT disabled_at, gc_eligible FROM workflow_defs
			                 WHERE name = $1 AND version = $2`),
		},
		{
			name:    "mysql",
			dialect: testutil.DialectMySQL,
			setup: func(t *testing.T) (WorkflowStore, *sql.DB) {
				db := testutil.MySQLTestDB(t)
				testutil.SetupMySQLFullSchema(t, db)
				return NewMySQLStore(db), db
			},
			disableOnly: mySet("disabled_at", "NOW(6)"),
			armOnly:     mySet("gc_eligible", "1"),
			read: readWith(`SELECT disabled_at, gc_eligible FROM workflow_defs
			                 WHERE name = ? AND version = ?`),
		},
		{
			name:    "mssql",
			dialect: testutil.DialectMSSQL,
			setup: func(t *testing.T) (WorkflowStore, *sql.DB) {
				db := testutil.MSSQLTestDB(t)
				testutil.SetupMSSQLFullSchema(t, db)
				testutil.CleanupMSSQLTestData(t, db)
				// TWO DIFFERENT CONNECTIONS, and both choices are forced by
				// dbo.TenantFilter_Defs, a FILTER predicate on workflow_defs
				// that exempts nobody -- not even sysadmin.
				//
				// The STORE must be openMSSQLTenantStore, not
				// NewMSSQLStore(db): the latter has no SESSION_CONTEXT, so the
				// filter hides the rows from the connection that wrote them.
				// INSERT still succeeds (there is no BLOCK predicate), so
				// deploying looked fine while every subsequent UPDATE matched
				// zero rows and reported success. Measured: 6 rows physically
				// present, 0 visible.
				//
				// The RAW handle must be the ADMIN one, for the same reason in
				// the other direction -- it has to read across the predicate to
				// see what the store wrote.
				return openMSSQLTenantStore(t, DefaultTenantUUID),
					testutil.AdminDB(t, db, testutil.DialectMSSQL)
			},
			disableOnly: msSet("disabled_at", "SYSUTCDATETIME()"),
			armOnly:     msSet("gc_eligible", "1"),
			read: readWith(`SELECT disabled_at, gc_eligible FROM workflow_defs
			                 WHERE name = @p1 AND version = @p2`),
		},
	}
}

// gcSplitOpts is a GC configuration that WOULD collect version 1 of a
// two-version definition, if that version were eligible.
//
// MinVersionsToKeep is 1 so that the newest version is protected and the older
// one is a candidate; Now is a year ahead so the age test passes whatever the
// row's created_at is. Everything except eligibility is therefore satisfied,
// which is what makes a result of "not collected" attributable to eligibility
// alone rather than to any of GC's other three gates.
func gcSplitOpts() GCOptions {
	return GCOptions{
		MinVersionsToKeep: 1,
		MaxVersionAge:     30 * 24 * time.Hour,
		Now:               time.Now().Add(365 * 24 * time.Hour),
	}
}

func deployGCSplitProbe(t *testing.T, store WorkflowStore, name string) {
	t.Helper()
	ctx := context.Background()
	for _, v := range []int{1, 2} {
		if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
			Name: name, Version: v, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
			ABIVersion: 1, MinVersion: 1,
		}); err != nil {
			t.Fatalf("deploy %s v%d: %v", name, v, err)
		}
	}
}

// TestDisablingAVersionDoesNotMakeItCollectable is the hazard, as a regression
// test rather than an argument.
//
// Before the split, retiring a version and arming its permanent deletion were
// the same bit. A generic entity helper writing the contract's `disabled_at` --
// the column nine other members carry with no deletion attached -- would have
// armed a GC that deletes a definition an in-flight run may still need to
// replay. This asserts that writing that column alone does nothing to
// collection.
func TestDisablingAVersionDoesNotMakeItCollectable(t *testing.T) {
	for _, d := range gcSplitDialects() {
		t.Run(d.name, func(t *testing.T) {
			store, db := d.setup(t)
			// NOT closed here: on SQL Server the raw handle is the SHARED admin
			// connection, and closing it leaves every later subtest reading
			// "sql: database is closed". testutil owns these handles' lifetime.
			ctx := context.Background()
			const name = "gc-split-disabled-only"

			deployGCSplitProbe(t, store, name)
			d.disableOnly(t, db, name, 1)

			// PRECONDITION: exactly the state under test. Without this the
			// assertion below passes for a row that was never disabled, and a
			// GC that collects nothing because the write silently failed reads
			// identically to a GC that correctly refused.
			disabled, eligible := d.read(t, db, name, 1)
			if !disabled || eligible {
				t.Fatalf("UNMEASURED on %s: wanted disabled_at set and gc_eligible false, "+
					"got disabled=%v eligible=%v. Nothing below is a statement about the "+
					"split unless the row is in that exact state.", d.name, disabled, eligible)
			}

			res, err := GarbageCollectVersions(ctx, store, gcSplitOpts())
			if err != nil {
				t.Fatalf("GarbageCollectVersions: %v", err)
			}
			if res.VersionsRemoved != 0 {
				t.Errorf("on %s, a version with disabled_at set and gc_eligible false was "+
					"COLLECTED (%d removed).\n\n"+
					"That is cleat#1702's hazard: retirement must not arm a permanent "+
					"deletion. If GC has been changed to key on disabled_at, the two columns "+
					"are one again and a generic entity write can delete a definition an "+
					"in-flight run still needs. engine/version_gc.go must test GCEligible.",
					d.name, res.VersionsRemoved)
			}

			// And admission control DID take effect, so the write was not inert.
			if def, err := store.GetWorkflowDef(ctx, name, 1); err != nil {
				t.Fatalf("GetWorkflowDef: %v", err)
			} else if def == nil || !def.Disabled() {
				t.Errorf("on %s, disabled_at was set but the loaded definition does not "+
					"report Disabled(): the column is not reaching admission control, so "+
					"this test's 'not collected' result proves nothing.", d.name)
			}
		})
	}
}

// TestArmingCollectionDoesNotDisableAVersion is the other direction, and it is
// also the POSITIVE CONTROL for the test above.
//
// Without it, a GC that collected NOTHING EVER would pass
// TestDisablingAVersionDoesNotMakeItCollectable perfectly. This proves the
// harness can observe a collection at all -- and, separately, that arming one
// does not retire the version.
func TestArmingCollectionDoesNotDisableAVersion(t *testing.T) {
	for _, d := range gcSplitDialects() {
		t.Run(d.name, func(t *testing.T) {
			store, db := d.setup(t)
			// NOT closed here: on SQL Server the raw handle is the SHARED admin
			// connection, and closing it leaves every later subtest reading
			// "sql: database is closed". testutil owns these handles' lifetime.
			ctx := context.Background()
			const name = "gc-split-armed-only"

			deployGCSplitProbe(t, store, name)
			d.armOnly(t, db, name, 1)

			disabled, eligible := d.read(t, db, name, 1)
			if disabled || !eligible {
				t.Fatalf("UNMEASURED on %s: wanted gc_eligible true and disabled_at NULL, "+
					"got disabled=%v eligible=%v", d.name, disabled, eligible)
			}

			// ADMISSION CONTROL IS UNAFFECTED: still startable, still routable.
			if def, err := store.GetWorkflowDef(ctx, name, 1); err != nil {
				t.Fatalf("GetWorkflowDef: %v", err)
			} else if def == nil || def.Disabled() {
				t.Errorf("on %s, arming collection reported the version as disabled. "+
					"gc_eligible must not reach admission control -- a version awaiting "+
					"collection is still usable until it is collected.", d.name)
			}

			// THE CONTROL: with eligibility armed, GC does collect.
			res, err := GarbageCollectVersions(ctx, store, gcSplitOpts())
			if err != nil {
				t.Fatalf("GarbageCollectVersions: %v", err)
			}
			if res.VersionsRemoved != 1 {
				t.Errorf("on %s, gc_eligible was true and every other GC gate satisfied, "+
					"yet %d versions were removed (want 1).\n\n"+
					"This is the control for TestDisablingAVersionDoesNotMakeItCollectable: "+
					"if collection cannot happen here, that test's zero proves nothing about "+
					"the split and would pass against a GC that never collects anything.",
					d.name, res.VersionsRemoved)
			}
		})
	}
}

// TestDeprecatingAVersionMovesBothColumnsTogether covers the failure mode the
// split CREATED, which is the one no reviewer of the old column would look for.
//
// MarkVersionDeprecated also UN-deprecates. Clear retirement without clearing
// eligibility and the row is LIVE AND COLLECTABLE -- startable, routable, and
// queued for permanent deletion -- a state the old single boolean could not
// express and neither new column can express alone. Both writes must move
// together, and this asserts it rather than trusting three store
// implementations to stay in step.
func TestDeprecatingAVersionMovesBothColumnsTogether(t *testing.T) {
	for _, d := range gcSplitDialects() {
		t.Run(d.name, func(t *testing.T) {
			store, db := d.setup(t)
			// NOT closed here: on SQL Server the raw handle is the SHARED admin
			// connection, and closing it leaves every later subtest reading
			// "sql: database is closed". testutil owns these handles' lifetime.
			ctx := context.Background()
			const name = "gc-split-both"

			deployGCSplitProbe(t, store, name)

			if disabled, eligible := d.read(t, db, name, 1); disabled || eligible {
				t.Fatalf("UNMEASURED on %s: a freshly deployed version should be neither "+
					"disabled nor armed, got disabled=%v eligible=%v", d.name, disabled, eligible)
			}

			if err := store.MarkVersionDeprecated(ctx, name, 1, true); err != nil {
				t.Fatalf("MarkVersionDeprecated(true): %v", err)
			}
			if disabled, eligible := d.read(t, db, name, 1); !disabled || !eligible {
				t.Errorf("on %s, deprecating set disabled=%v eligible=%v; want both true.\n"+
					"Deprecating means retire AND allow collection -- that is what the "+
					"command has always meant, and keeping it is why the split needed no "+
					"change to the operator workflow.", d.name, disabled, eligible)
			}

			// Re-deprecating must not move the instant it was disabled: the
			// stores use COALESCE for exactly this, and a bare now() here would
			// silently reset the age of every repeated call.
			firstDisabled := readDisabledAt(t, db, d, name, 1)
			time.Sleep(10 * time.Millisecond)
			if err := store.MarkVersionDeprecated(ctx, name, 1, true); err != nil {
				t.Fatalf("MarkVersionDeprecated(true) again: %v", err)
			}
			if again := readDisabledAt(t, db, d, name, 1); !again.Equal(firstDisabled) {
				t.Errorf("on %s, re-deprecating moved disabled_at from %v to %v. "+
					"COALESCE(disabled_at, now()) is what stops that.",
					d.name, firstDisabled, again)
			}

			if err := store.MarkVersionDeprecated(ctx, name, 1, false); err != nil {
				t.Fatalf("MarkVersionDeprecated(false): %v", err)
			}
			disabled, eligible := d.read(t, db, name, 1)
			if disabled || eligible {
				t.Errorf("on %s, restoring left disabled=%v eligible=%v; want both false.\n\n"+
					"THE DANGEROUS HALF IS eligible=true WITH disabled=false: that is a "+
					"version which is live, startable, and queued for permanent deletion. "+
					"Neither the old `deprecated` boolean nor either new column can express "+
					"it alone -- it exists only if the two writes come apart.",
					d.name, disabled, eligible)
			}
		})
	}
}

func readDisabledAt(t *testing.T, db *sql.DB, d gcSplitDialect, name string, version int) time.Time {
	t.Helper()
	q := map[testutil.Dialect]string{
		testutil.DialectPostgres: `SELECT disabled_at FROM workflow_defs WHERE name = $1 AND version = $2`,
		testutil.DialectMySQL:    `SELECT disabled_at FROM workflow_defs WHERE name = ? AND version = ?`,
		testutil.DialectMSSQL:    `SELECT disabled_at FROM workflow_defs WHERE name = @p1 AND version = @p2`,
	}[d.dialect]
	var at *time.Time
	if err := db.QueryRow(q, name, version).Scan(&at); err != nil {
		t.Fatalf("read disabled_at: %v", err)
	}
	if at == nil {
		t.Fatalf("disabled_at is NULL where it should be set")
	}
	return *at
}
