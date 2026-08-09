package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// A schedule read back from SQL Server has to carry its tenant as canonical
// UUID text, because the scheduler loop feeds that string straight back into
// the database.
//
// cmd/cleat-worker's schedule loop does exactly two things with
// Schedule.TenantID: it builds the cron:<tenant>:<name>:<instant> idempotency
// key that carries the at-least-once delivery guarantee, and it passes the
// value to StartNewRun, which binds it to a UNIQUEIDENTIFIER parameter.
//
// go-mssqldb scans UNIQUEIDENTIFIER into a Go string as the 16 raw storage
// bytes rather than the hyphenated form. Those bytes are not empty, so the
// loop's `if tenantID == ""` fallback does not fire; they are not valid UUID
// text either, so the bind fails with "Conversion failed when converting from
// a character string to uniqueidentifier" -- and NO schedule fires on SQL
// Server at all.
//
// The round trip is the assertion rather than the string format, because the
// round trip is what actually broke. A test that only checked for hyphens
// would pass on any plausible-looking string.
func TestMSSQLSchedules_TenantIDSurvivesTheRoundTripToStartNewRun(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)

	const tid = "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()

	seed := NewMSSQLStore(db)
	seed.tenantID = tid
	setupTestData(t, seed)
	if err := seed.CreateSchedule(ctx, Schedule{
		Name:           "tenant-id-round-trip",
		DefName:        "test-workflow",
		EntryPoint:     "run",
		CronExpression: "* * * * *",
		Input:          json.RawMessage(`{}`),
		Enabled:        true,
		NextRunAt:      time.Now().Add(-time.Minute).UTC(),
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// The shipped security policies filter a plain pool that has no
	// SESSION_CONTEXT, so the reads below would return nothing at all through
	// one -- a green test that measured no rows. MSSQLAdminDB is a no-op on a
	// schema without policies and a cleat_admin connection on one with them.
	store := NewMSSQLStore(testutil.MSSQLAdminDB(t, db))
	store.tenantID = tid

	for _, tc := range []struct {
		name string
		read func() ([]Schedule, error)
	}{
		{"GetDueSchedules", func() ([]Schedule, error) { return store.GetDueSchedules(ctx) }},
		{"ListSchedules", func() ([]Schedule, error) { return store.ListSchedules(ctx) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedules, err := tc.read()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			var found *Schedule
			for i := range schedules {
				if schedules[i].Name == "tenant-id-round-trip" {
					found = &schedules[i]
				}
			}
			if found == nil {
				t.Fatalf("%s returned %d schedule(s), none named tenant-id-round-trip -- "+
					"the fixture is not reaching the read under test", tc.name, len(schedules))
			}

			if _, err := uuid.Parse(found.TenantID); err != nil {
				t.Errorf("TenantID = %q (%d bytes) does not parse as a UUID: %v",
					found.TenantID, len(found.TenantID), err)
			}
			if found.TenantID != tid {
				t.Errorf("TenantID = %q, want %q", found.TenantID, tid)
			}

			// The failure that actually broke cron: the scheduler loop binds
			// this value back to a UNIQUEIDENTIFIER parameter.
			idemKey := "cron:" + found.TenantID + ":" + found.Name + ":1700000000"
			if _, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), idemKey, found.TenantID, 0); err != nil {
				t.Fatalf("StartNewRun with the tenant %s returned: %v\n"+
					"this is what the scheduler loop does with every due schedule, so "+
					"this error means no schedule fires on SQL Server at all", tc.name, err)
			}

			// The same value is the idempotency key that carries at-least-once
			// delivery. Raw bytes there would still be stable, so this is a
			// legibility and collision-surface check rather than a correctness
			// one -- but a key nobody can read is a key nobody can debug.
			if strings.ContainsRune(idemKey, '\x00') || !strings.Contains(idemKey, tid) {
				t.Errorf("idempotency key does not carry readable tenant text: %q", idemKey)
			}
		})
	}
}
