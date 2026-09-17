package engine

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#1702's LAST conversion: workflow_schedules.enabled became disabled_at
// (migration 089/077/081), and it is the only one of the four that INVERTED
// POLARITY -- `enabled = true` meant live, `disabled_at IS NULL` means live.
//
// WHY THAT NEEDS ITS OWN TESTS RATHER THAN RIDING ON THE OTHERS. The three
// earlier conversions could not change behaviour if the SQL was right, because
// `revoked_at`, `deprecated` and `disabled_at` all read "set means retired". An
// inversion is different: every predicate, every scan and every UI toggle that
// touched the old column has to flip WITH it, and a site that does not flip
// still compiles, still runs, and answers backwards. The failure is a schedule
// that fires while disabled, or never fires while live -- neither of which
// raises an error anywhere.
//
// scripts/check-entity-contract.py proves the COLUMN converted. It reads
// migration files, so it cannot say whether the predicates moved with it. That
// is what these tests are for.

// scheduleRetirementDialect is one dialect's worth of what these tests need: a
// store to drive the real paths, plus a raw connection that can see and read
// the column directly.
type scheduleRetirementDialect struct {
	name  string
	setup func(*testing.T) (WorkflowStore, *sql.DB)
	// readDisabledAt returns the column as the database holds it, which is the
	// only way to tell "the store reported nil" from "the row says NULL".
	readDisabledAt func(*testing.T, *sql.DB, string) *time.Time
	// columnExists answers information_schema for one column of
	// workflow_schedules, so a test can confirm the migration actually ran on
	// THIS database rather than assuming the file implies the state.
	columnExists func(*testing.T, *sql.DB, string) bool
}

func scheduleRetirementDialects() []scheduleRetirementDialect {
	readWith := func(q string) func(*testing.T, *sql.DB, string) *time.Time {
		return func(t *testing.T, db *sql.DB, name string) *time.Time {
			t.Helper()
			var at *time.Time
			if err := db.QueryRow(q, name).Scan(&at); err != nil {
				t.Fatalf("read disabled_at: %v", err)
			}
			return at
		}
	}
	existsWith := func(q string) func(*testing.T, *sql.DB, string) bool {
		return func(t *testing.T, db *sql.DB, col string) bool {
			t.Helper()
			var n int
			if err := db.QueryRow(q, col).Scan(&n); err != nil {
				t.Fatalf("information_schema lookup for %q: %v", col, err)
			}
			return n > 0
		}
	}
	return []scheduleRetirementDialect{
		{
			name: "postgres",
			setup: func(t *testing.T) (WorkflowStore, *sql.DB) {
				db := testutil.TestDB(t, testutil.DialectPostgres)
				testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
				testutil.CleanupPostgresTestData(t, db)
				return NewPostgresStore(db), db
			},
			readDisabledAt: readWith(`SELECT disabled_at FROM workflow_schedules WHERE name = $1`),
			columnExists: existsWith(`SELECT count(*) FROM information_schema.columns
			                           WHERE table_name = 'workflow_schedules' AND column_name = $1`),
		},
		{
			name: "mysql",
			setup: func(t *testing.T) (WorkflowStore, *sql.DB) {
				// Gate on whether the dialect was REQUESTED, not on whether it
				// answers: testutil.MySQLTestDB falls back to a default DSN and
				// then t.Fatalf's on the ping, so without this the test hard
				// fails in every job that does not provision MySQL. Same gate
				// as gc_eligibility_is_not_retirement_test.go, whose
				// requireDialectDSN this reuses.
				requireDialectDSN(t, "CLEAT_TEST_MYSQL")
				db := testutil.MySQLTestDB(t)
				testutil.SetupMySQLFullSchema(t, db)
				return NewMySQLStore(db), db
			},
			readDisabledAt: readWith(`SELECT disabled_at FROM workflow_schedules WHERE name = ?`),
			columnExists: existsWith(`SELECT count(*) FROM information_schema.COLUMNS
			                           WHERE TABLE_SCHEMA = DATABASE()
			                             AND TABLE_NAME = 'workflow_schedules'
			                             AND COLUMN_NAME = ?`),
		},
		{
			name: "mssql",
			setup: func(t *testing.T) (WorkflowStore, *sql.DB) {
				requireDialectDSN(t, "CLEAT_TEST_MSSQL")
				db := testutil.MSSQLTestDB(t)
				testutil.SetupMSSQLFullSchema(t, db)
				testutil.CleanupMSSQLTestData(t, db)
				// TWO CONNECTIONS, forced by dbo.TenantFilter_Schedules -- a
				// FILTER predicate on workflow_schedules that exempts nobody,
				// sysadmin included. The STORE needs SESSION_CONTEXT or the
				// filter hides the rows it just wrote (INSERT still succeeds:
				// there is no BLOCK predicate, so writing looks fine while
				// every later read matches nothing). The RAW handle must be the
				// admin one so it can read across the predicate. Migration 081
				// toggles this same policy off around its backfill for exactly
				// this reason.
				return openMSSQLTenantStore(t, DefaultTenantUUID),
					testutil.AdminDB(t, db, testutil.DialectMSSQL)
			},
			readDisabledAt: readWith(`SELECT disabled_at FROM workflow_schedules WHERE name = @p1`),
			columnExists: existsWith(`SELECT count(*) FROM information_schema.columns
			                           WHERE table_name = 'workflow_schedules' AND column_name = @p1`),
		},
	}
}

// pastDueSchedule is a schedule that is already owed a firing, so it appears in
// GetDueSchedules the moment it is live.
func pastDueSchedule(name string) Schedule {
	return Schedule{
		Name:           name,
		DefName:        "retirement-probe-def",
		EntryPoint:     "main",
		CronExpression: "*/5 * * * *",
		// Deliberately no retirement field: after cleat#1702 a live schedule
		// is the ZERO VALUE, and that property is asserted below.
		NextRunAt: time.Now().Add(-time.Hour),
		Timezone:  "UTC",
	}
}

func isDue(t *testing.T, store WorkflowStore, name string) bool {
	t.Helper()
	due, err := store.GetDueSchedules(context.Background())
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	for i := range due {
		if due[i].Name == name {
			return true
		}
	}
	return false
}

// TestTheScheduleColumnConvertedOnThisDatabase confirms the migration RAN here,
// with the control that makes the answer mean something.
//
// Without the control this is the emptiest kind of green: an
// information_schema query that is silently looking at the wrong database, or
// at a table that does not exist, reports "no `enabled` column" just as
// cheerfully as a converted schema does. So it also asks for a column that MUST
// be present. One absence and one presence, from the same query.
func TestTheScheduleColumnConvertedOnThisDatabase(t *testing.T) {
	for _, d := range scheduleRetirementDialects() {
		t.Run(d.name, func(t *testing.T) {
			_, db := d.setup(t)

			// CONTROL: the query can find a column that is there.
			if !d.columnExists(t, db, "disabled_at") {
				t.Fatalf("UNMEASURED on %s: workflow_schedules has no disabled_at, so this "+
					"lookup is not reading the table under test and its answer about "+
					"`enabled` means nothing", d.name)
			}
			// CONTROL: and does not find one that never existed, so a
			// non-zero count is not simply what this query always returns.
			if d.columnExists(t, db, "no_such_column_cleat1702") {
				t.Fatalf("UNMEASURED on %s: the lookup reports a column that cannot exist",
					d.name)
			}

			if d.columnExists(t, db, "enabled") {
				t.Errorf("workflow_schedules still carries `enabled` on %s. Migration "+
					"089/077/081 should have dropped it; a database where both columns "+
					"exist has two answers to \"should this fire\" and the code now reads "+
					"only one of them", d.name)
			}
		})
	}
}

// TestADisabledScheduleIsNotDueAndALiveOneIs is the behaviour the inversion
// could break without anything failing to compile.
func TestADisabledScheduleIsNotDueAndALiveOneIs(t *testing.T) {
	for _, d := range scheduleRetirementDialects() {
		t.Run(d.name, func(t *testing.T) {
			store, db := d.setup(t)
			ctx := context.Background()
			const name = "retirement-probe-due"
			if err := store.CreateSchedule(ctx, pastDueSchedule(name)); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}

			// A new schedule is LIVE, and that is the zero value of the field.
			if at := d.readDisabledAt(t, db, name); at != nil {
				t.Fatalf("a newly created schedule has disabled_at = %v, want NULL: the "+
					"zero value of the Go field must mean live, or every caller that "+
					"omits it creates a schedule that never fires", at)
			}

			// POSITIVE CONTROL. Everything below is about a schedule LEAVING
			// and RE-ENTERING this set, so a due-scan that returns nothing at
			// all would make the disabled case pass for the wrong reason --
			// and "not in the list" is exactly what a broken predicate also
			// produces.
			if !isDue(t, store, name) {
				t.Fatalf("UNMEASURED on %s: a live, past-due schedule is not in "+
					"GetDueSchedules, so this test cannot tell a working retirement "+
					"predicate from a due-scan that matches nothing", d.name)
			}

			if err := store.SetScheduleEnabled(ctx, name, false); err != nil {
				t.Fatalf("SetScheduleEnabled(false): %v", err)
			}
			if isDue(t, store, name) {
				t.Errorf("a disabled schedule is still due on %s: the due-schedule "+
					"predicate did not move from `enabled` to `disabled_at IS NULL`, so "+
					"disabling a schedule no longer stops it firing", d.name)
			}
			if at := d.readDisabledAt(t, db, name); at == nil {
				t.Errorf("disabling left disabled_at NULL on %s, so nothing recorded the "+
					"retirement even though the schedule stopped being due", d.name)
			}

			if err := store.SetScheduleEnabled(ctx, name, true); err != nil {
				t.Fatalf("SetScheduleEnabled(true): %v", err)
			}
			if at := d.readDisabledAt(t, db, name); at != nil {
				t.Errorf("enabling left disabled_at = %v on %s, want NULL: retirement has "+
					"to be reversible or /api/schedules/{name}/enable is a no-op", at, d.name)
			}
			if !isDue(t, store, name) {
				t.Errorf("a re-enabled schedule is not due again on %s", d.name)
			}
		})
	}
}

// TestRetiringAScheduleTwiceKeepsTheFirstInstant pins the COALESCE in the three
// stores' UPDATE.
//
// The column records WHEN, so a second disable that moves the timestamp is
// losing the only fact the boolean could not carry -- and it does it silently,
// because the schedule is disabled either way and no read of "is this live"
// can see the difference. This is the property that distinguishes the timestamp
// from the boolean it replaced; nothing else in the tree asserts it.
func TestRetiringAScheduleTwiceKeepsTheFirstInstant(t *testing.T) {
	for _, d := range scheduleRetirementDialects() {
		t.Run(d.name, func(t *testing.T) {
			store, db := d.setup(t)
			ctx := context.Background()
			const name = "retirement-probe-idempotent"
			if err := store.CreateSchedule(ctx, pastDueSchedule(name)); err != nil {
				t.Fatalf("CreateSchedule: %v", err)
			}
			if err := store.SetScheduleEnabled(ctx, name, false); err != nil {
				t.Fatalf("first disable: %v", err)
			}
			first := d.readDisabledAt(t, db, name)
			if first == nil {
				t.Fatalf("UNMEASURED on %s: the first disable recorded no instant, so there "+
					"is nothing for the second one to preserve or move", d.name)
			}

			// Enough separation that a re-stamp cannot be mistaken for clock
			// resolution: the assertion below is exact equality, so any
			// re-stamp at all fails it, and this only makes the failure legible.
			time.Sleep(10 * time.Millisecond)

			if err := store.SetScheduleEnabled(ctx, name, false); err != nil {
				t.Fatalf("second disable: %v", err)
			}
			second := d.readDisabledAt(t, db, name)
			if second == nil {
				t.Fatalf("the second disable cleared disabled_at on %s", d.name)
			}
			if !second.Equal(*first) {
				t.Errorf("disabling twice moved disabled_at on %s: %v then %v.\n"+
					"The store's UPDATE should be COALESCE(disabled_at, <now>), which keeps "+
					"the instant the schedule was actually retired. Re-stamping it means the "+
					"column answers \"when did someone last call disable\" instead, and the "+
					"real answer is gone with no error anywhere.",
					d.name, first, second)
			}
		})
	}
}
