package engine

// IMPROVEMENT-PLAN 3.16: CreateSchedule could not create a schedule on any SQL
// Server built from the shipped schema.
//
// json.RawMessage is a []byte, and go-mssqldb binds a []byte as VARBINARY, so
// workflow_schedules.input received the binary rendering of the JSON rather
// than the JSON. migrations/mssql/001_schema.sql guards that column with
// `CONSTRAINT ck_workflow_schedules_input CHECK (ISJSON(input) = 1)`, which
// rejects it:
//
//	The INSERT statement conflicted with the CHECK constraint
//	"ck_workflow_schedules_input" ... column 'input'
//
// Every scheduled workflow on SQL Server therefore failed to be created.
//
// It survived because engine/testutil's hand-written MSSQL schema declares no
// CHECK constraint on that column: the malformed value went in, `go test`
// stayed green, and the only place the defect was visible was a database
// built from the file that ships. That is the 2.71 schema residual in one
// concrete failure -- which is how it was found.
//
// This test therefore does NOT use the shared test schema. It applies the real
// constraint to the table it is about, so the assertion is against what
// production enforces.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestMSSQLCreateSchedule_SurvivesTheShippedInputConstraint(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server schedule input test")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, db)
	testutil.CleanupMSSQLTestData(t, db)

	// Apply the shipped CHECK constraint, and take it away again afterwards:
	// leaving it behind would change what every later MSSQL test in this
	// binary is running against, which is the hazard
	// mssql_rls_enforcement_test.go documents for the security policies.
	const constraintName = "ck_workflow_schedules_input_test"
	if _, err := db.Exec(`IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = '` + constraintName + `')
		ALTER TABLE dbo.workflow_schedules DROP CONSTRAINT ` + constraintName); err != nil {
		t.Fatalf("drop leftover constraint: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE dbo.workflow_schedules
		ADD CONSTRAINT ` + constraintName + ` CHECK (ISJSON(input) = 1)`); err != nil {
		t.Fatalf("apply the shipped input constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`IF EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = '` + constraintName + `')
			ALTER TABLE dbo.workflow_schedules DROP CONSTRAINT ` + constraintName); err != nil {
			t.Errorf("drop the input constraint: %v", err)
		}
	})

	ctx := context.Background()

	// Built the way production builds it. NewMSSQLStore(db) on a plain pool
	// puts sp_set_session_context on none of its connections, so under the
	// shipped security policies the store cannot see the rows it just wrote --
	// which surfaces here as "read input back: sql: no rows in result set",
	// three subtests deep, looking nothing like a store-construction problem.
	// Same §1.3 shape setupMSSQLIntegrationTest documents.
	ws, closer, err := NewMSSQLStoreFactory(os.Getenv("CLEAT_TEST_MSSQL")).OpenStore(
		ctx, DefaultTenantUUID, "default")
	if err != nil {
		t.Fatalf("open a tenant-scoped store: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	store, ok := ws.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", ws)
	}

	// The raw-SQL reads below are subject to the same policies. They check
	// what the store did rather than acting as a tenant, which is
	// administrative work, so they go through the admin connection.
	adminDB := testutil.MSSQLAdminDB(t, db)

	for _, tc := range []struct {
		name  string
		input json.RawMessage
	}{
		{"an object", json.RawMessage(`{"key":"value"}`)},
		{"empty object", json.RawMessage(`{}`)},
		{"no input at all", nil},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			name := "sched-input-" + tc.name
			if err := store.CreateSchedule(ctx, Schedule{
				Name:           name,
				DefName:        "test-workflow",
				EntryPoint:     "main",
				CronExpression: "* * * * *",
				Input:          tc.input,
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatalf("CreateSchedule: %v\n\nThis is what a SQL Server built from "+
					"migrations/mssql/001_schema.sql does with every schedule.", err)
			}
			t.Cleanup(func() {
				_, _ = adminDB.Exec(`DELETE FROM workflow_schedules WHERE name = @p1`, name)
			})

			// And the value has to come back as the JSON that went in, not as
			// something that merely satisfies ISJSON.
			var stored string
			if err := adminDB.QueryRow(
				`SELECT CAST(input AS NVARCHAR(MAX)) FROM workflow_schedules WHERE name = @p1`,
				name).Scan(&stored); err != nil {
				t.Fatalf("read input back: %v", err)
			}
			want := string(tc.input)
			if len(tc.input) == 0 {
				want = "{}"
			}
			if stored != want {
				t.Errorf("input stored as %q, want %q", stored, want)
			}
		})
	}
}
