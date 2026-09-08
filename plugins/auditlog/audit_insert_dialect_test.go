package auditlog

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// TestTheAuditInsertWorksOnMySQLsIDColumn is the test whose absence let
// cleat#958 ship, and it needs a real database because a fake cannot have this
// defect.
//
// # What was wrong
//
// The insert omitted `id` and relied on a database-side default. Two of three
// dialects have one:
//
//	postgres   id UUID PRIMARY KEY DEFAULT gen_random_uuid()
//	mssql      id UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID()
//	mysql      id CHAR(36) NOT NULL          -- none
//
// So every insert on MySQL failed with
// `Error 1364 (HY000): Field 'id' doesn't have a default value`, and the table
// had never held a row on that dialect. The error is logged and swallowed, so
// the only symptom was an empty audit table -- indistinguishable from an audit
// log for a quiet system, which is the one failure mode audit logging exists
// not to have.
//
// # Why nothing caught it
//
// The behavioural tests use a fake driver that INVENTED an id with uuid.New()
// while reading the caller's other arguments by ordinal. A double that supplies
// what the database will not is indistinguishable from a database that supplies
// it, so a statement MySQL rejects looked fine. The fake now parses the id the
// caller sends and refuses anything that is not a UUID.
func TestTheAuditInsertWorksOnMySQLsIDColumn(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MYSQL not set, skipping MySQL tests")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("opening mysql: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		// Configured but unreachable is a failure, not a skip: a skip here
		// would restore exactly the silence this test exists to break.
		t.Fatalf("configured MySQL is unreachable: %v", err)
	}

	// A scratch DATABASE, not a renamed table.
	//
	// The plugin's SQL names audit_events literally, so the fixture cannot
	// rename it -- and creating it in the shared test database collides with the
	// copy already there (Error 1061, duplicate index name), which reads as a
	// failure of the code rather than of the fixture. It would also leave this
	// test asserting against whatever shape that copy happens to have, which
	// CLAUDE.md warns about directly: a long-lived database keeps its old
	// columns and `CREATE TABLE IF NOT EXISTS` never adds one.
	scratch := "cleat_audit958_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+scratch); err != nil {
		t.Fatalf("creating scratch database: %v", err)
	}
	defer func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + scratch) }()

	db, err := sql.Open("mysql", scratchDSN(dsn, scratch))
	if err != nil {
		t.Fatalf("opening the scratch database: %v", err)
	}
	defer db.Close()

	// The SHIPPED MySQL DDL, not a copy. A hand-written CREATE TABLE here would
	// let this pass while the real column kept its missing default -- the test
	// would be asserting something about itself.
	var ddl string
	for _, m := range (&Plugin{}).Migrations() {
		if strings.Contains(m.UpMySQL, "audit_events") &&
			strings.Contains(strings.ToUpper(m.UpMySQL), "CREATE TABLE") {
			ddl = m.UpMySQL
			break
		}
	}
	if ddl == "" {
		t.Fatal("no MySQL audit_events CREATE TABLE found in the plugin's migrations; " +
			"this test can no longer reach the column it exists to check")
	}

	// The plugin's SQL names audit_events literally, so the fixture uses that
	// name and cleans up after itself rather than renaming the table.
	const table = "audit_events"
	// ReplaceAll, not Replace: the DDL also names indexes after the table
	// (idx_audit_events_tenant_ts), and renaming only the first occurrence
	// leaves those colliding with the real ones -- Error 1061, duplicate key
	// name, which reads as a test failure rather than as the fixture problem it
	// is.
	if _, err := db.ExecContext(context.Background(), ddl); err != nil {
		t.Fatalf("creating %s from the shipped migration: %v", table, err)
	}

	// Drive the PLUGIN, not a copy of its statement.
	//
	// An earlier version of this test issued the INSERT itself. That asserts the
	// shipped DDL accepts a statement I wrote, and would pass unchanged if the
	// plugin's own SQL regressed to omitting the id -- which is the entire
	// defect. recordAudit is unexported and this test is in the package, so
	// there is no reason to settle for the weaker thing.
	//
	// recordAudit logs and swallows its error, exactly as in production, so the
	// assertion has to be on the ROW rather than on a returned error. That is
	// also the honest shape: an operator's only signal was an empty table.
	p := &Plugin{
		db:      &engine.SQLDBAdapter{DB: db},
		dialect: plugin.DialectMySQL,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	tenant := uuid.New()
	p.recordAudit(context.Background(), tenant, "GET", "/api/workflows", 200,
		"127.0.0.1", "test-agent", 3*time.Millisecond)

	var n int
	if err := db.QueryRowContext(context.Background(),
		fmt.Sprintf("SELECT count(*) FROM %s WHERE tenant_id = ?", table), tenant.String()).Scan(&n); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("the plugin recorded %d audit rows on MySQL, want 1.\n\n"+
			"cleat#958: audit_events.id is CHAR(36) with no default there, so an insert that "+
			"does not supply one fails while the same statement succeeds on PostgreSQL and SQL "+
			"Server. recordAudit logs and swallows the error, so the only symptom is this "+
			"count -- an empty audit table, indistinguishable from a quiet system.", n)
	}

	// The control, and it is what makes the assertion above mean anything.
	// Without it this test passes just as happily against a column that HAS a
	// default -- the configuration in which the bug cannot occur -- so it would
	// keep passing if the caller stopped supplying an id.
	_, err = db.ExecContext(context.Background(), fmt.Sprintf(`
		INSERT INTO %s (tenant_id, method, path, status_code, user_id, ip_address, user_agent, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, table),
		uuid.NewString(), "GET", "/api/workflows", 200, "", "127.0.0.1", "test", 3)
	if err == nil {
		t.Error("an insert OMITTING the id succeeded on MySQL.\n\n" +
			"That means this column now has a default, so the assertion above passed for a " +
			"reason unrelated to the fix. If a default was added deliberately, delete this " +
			"control and say so in the migration.")
	}
}

// scratchDSN rewrites a MySQL DSN's database name, which is the path segment
// after the host in `user:pass@tcp(host:port)/db?params`.
func scratchDSN(dsn, db string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	tail := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	return dsn[:slash+1] + db + tail
}
