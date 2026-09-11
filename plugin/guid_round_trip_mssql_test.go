package plugin_test

import (
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"

	// The driver this whole file is about. testutil.TestDB opens
	// "sqlserver://...", and without this blank import the package's test binary
	// fails with `unknown driver "sqlserver"` -- which testutil reports as
	// "configured mssql database is unreachable", naming the network rather than
	// the missing import.
	_ "github.com/microsoft/go-mssqldb"
)

// Round-trip a UNIQUEIDENTIFIER through a REAL SQL Server.
//
// cleat#1137 asked for exactly this, and it is the one thing the existing
// coverage does not do. What is already here:
//
//   - guid_scan_test.go / scanrow_test.go assert the byte swap against a
//     16-byte slice written by hand;
//   - guid_scan_types_test.go proves, with the type checker, that no plugin
//     scans into uuid.UUID any more.
//
// Both are worth having and neither measures the artifact. They assert a MODEL
// of what SQL Server sends. If the model is wrong -- a driver version that
// swaps for us, a column that is NVARCHAR rather than UNIQUEIDENTIFIER, a
// future go-mssqldb that returns the text form -- every one of those tests
// still passes, and plugin.GUID silently corrupts ids that arrived correct.
//
// # The control is the point, not the assertion
//
// The first subtest scans the same column into a bare uuid.UUID and requires it
// to come back WRONG. That reads backwards until you ask what a green run means
// without it: if this server, driver and column type did not actually exhibit
// the mixed-endian behaviour, the GUID assertion below would pass for a reason
// that has nothing to do with the fix, and this file would be a test of
// nothing.
//
// So the pair says: the hazard is real HERE, and plugin.GUID is what repairs
// it. If the control ever stops failing, the swap has become wrong and the
// right response is to delete GUID, not to update this test.
func TestAUUIDSurvivesARoundTripThroughSQLServer(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMSSQL)

	// A value whose first three groups are NOT palindromic, so a byte swap is
	// visible. An id like 00000000-0000-... would round-trip identically under
	// both the bug and the fix and prove nothing.
	const known = "cafbe5d6-8d74-4215-9908-9e01d7ae2654"
	want := uuid.MustParse(known)

	const table = "cleat_guid_round_trip_1137"
	mustExec(t, db, `IF OBJECT_ID('`+table+`', 'U') IS NOT NULL DROP TABLE `+table)
	mustExec(t, db, `CREATE TABLE `+table+` (id UNIQUEIDENTIFIER NOT NULL PRIMARY KEY, note NVARCHAR(64) NOT NULL)`)
	t.Cleanup(func() {
		_, _ = db.Exec(`IF OBJECT_ID('` + table + `', 'U') IS NOT NULL DROP TABLE ` + table)
	})

	// Written through the same path a plugin uses: uuid.UUID's Value, which is
	// the text form. cleat#1137 records that writes need no correction, and this
	// is what makes that claim observable rather than assumed -- the server's
	// own text rendering is compared below.
	mustExec(t, db, `INSERT INTO `+table+` (id, note) VALUES (@p1, @p2)`, want, "round-trip")

	t.Run("the server stored the id we asked for", func(t *testing.T) {
		// Read as text, which bypasses the driver's binary representation
		// entirely. If this disagrees, the WRITE is wrong and every conclusion
		// below would be about the wrong row.
		var asText string
		if err := db.QueryRow(`SELECT CONVERT(NVARCHAR(36), id) FROM ` + table).Scan(&asText); err != nil {
			t.Fatalf("reading the id as text: %v", err)
		}
		if got := uuid.MustParse(asText); got != want {
			t.Fatalf("the server holds %s, we wrote %s -- uuid.UUID's Value does not "+
				"round-trip on SQL Server, which cleat#1137 assumed it did", got, want)
		}
	})

	t.Run("CONTROL: scanning into uuid.UUID still corrupts it", func(t *testing.T) {
		var got uuid.UUID
		if err := db.QueryRow(`SELECT id FROM ` + table).Scan(&got); err != nil {
			t.Fatalf("scanning into uuid.UUID: %v", err)
		}
		if got == want {
			t.Fatalf("scanning UNIQUEIDENTIFIER straight into uuid.UUID returned the CORRECT "+
				"id (%s).\n\n"+
				"That is not good news. plugin.GUID swaps bytes unconditionally for any "+
				"16-byte source, so if this driver and column now deliver the right order, "+
				"GUID is corrupting ids rather than repairing them. Check go-mssqldb's "+
				"behaviour before touching anything else -- the fix, not the test, is what "+
				"has become wrong.", got)
		}
		t.Logf("confirmed mixed-endian on this server: uuid.UUID reads %s where the id is %s", got, want)
	})

	t.Run("plugin.GUID reads it back correctly", func(t *testing.T) {
		var got plugin.GUID
		if err := db.QueryRow(`SELECT id FROM ` + table).Scan(&got); err != nil {
			t.Fatalf("scanning into plugin.GUID: %v", err)
		}
		if got.UUID != want {
			t.Errorf("plugin.GUID read %s, want %s.\n\n"+
				"This is the whole of cleat#1137: the scan reports no error either way, so "+
				"a wrong value here becomes a WHERE clause that matches no row and an UPDATE "+
				"that reports success having changed nothing.", got.UUID, want)
		}
	})

	t.Run("and the corrected id matches a WHERE clause", func(t *testing.T) {
		// The consequence, asserted rather than reasoned about. A corrupted id
		// is well-formed, so the failure it causes is zero rows affected -- not
		// an error. This is the shape that left every claimed schedule
		// unadvanced in cleat#1133.
		var g plugin.GUID
		if err := db.QueryRow(`SELECT id FROM ` + table).Scan(&g); err != nil {
			t.Fatalf("scanning into plugin.GUID: %v", err)
		}
		res, err := db.Exec(`UPDATE `+table+` SET note = @p1 WHERE id = @p2`, "updated", g.UUID)
		if err != nil {
			t.Fatalf("UPDATE: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("RowsAffected: %v", err)
		}
		if n != 1 {
			t.Errorf("UPDATE ... WHERE id = <scanned id> affected %d rows, want 1.\n\n"+
				"Zero here is the silent failure mode: valid SQL, no error, no row.", n)
		}
	})
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %.60q: %v", q, err)
	}
}
