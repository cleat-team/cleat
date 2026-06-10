package testutil

import (
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"
)

func TestDialectConstants(t *testing.T) {
	if DialectPostgres != "postgres" {
		t.Errorf("DialectPostgres = %q, want %q", DialectPostgres, "postgres")
	}
	if DialectMySQL != "mysql" {
		t.Errorf("DialectMySQL = %q, want %q", DialectMySQL, "mysql")
	}
	if DialectMSSQL != "mssql" {
		t.Errorf("DialectMSSQL = %q, want %q", DialectMSSQL, "mssql")
	}
}

func TestPluginTestBackends(t *testing.T) {
	backends := NewPluginTestBackends(t)
	if len(backends) < 1 {
		t.Fatal("NewPluginTestBackends returned no backends; expected at least PostgreSQL")
	}

	for _, b := range backends {
		if b.Name == "" {
			t.Error("backend Name is empty")
		}
		if b.Dialect == "" {
			t.Error("backend Dialect is empty")
		}
		if b.DB == nil {
			t.Errorf("backend %s DB is nil", b.Name)
		}
		if b.Cleanup == nil {
			t.Errorf("backend %s Cleanup is nil", b.Name)
		}
	}

	// Verify PostgreSQL is present.
	foundPG := false
	for _, b := range backends {
		if b.Dialect == DialectPostgres {
			foundPG = true
			break
		}
	}
	if !foundPG {
		t.Error("expected a PostgreSQL backend in NewPluginTestBackends")
	}

	// Verify Cleanup doesn't panic.
	for _, b := range backends {
		b.Cleanup()
	}
}

func TestSetupMinimalSchema(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	defer db.Close()

	CleanupPostgresTestData(t, db)

	// First call creates tables.
	SetupMinimalSchema(t, db, DialectPostgres)

	// Verify tables exist by querying them.
	for _, table := range []string{"workflow_defs", "workflow_instances", "event_history", "workflow_signals"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}

	// Second call is idempotent (uses IF NOT EXISTS).
	SetupMinimalSchema(t, db, DialectPostgres)

	for _, table := range []string{"workflow_defs", "workflow_instances", "event_history", "workflow_signals"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Errorf("table %s after second call: %v", table, err)
		}
	}
}

func TestSetupFullSchema(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	defer db.Close()

	CleanupPostgresTestData(t, db)

	SetupFullSchema(t, db, DialectPostgres)

	// Verify core tables exist.
	expectedTables := []string{
		"workflow_defs", "workflow_instances", "event_history", "workflow_signals",
		"workflow_schedules", "concurrency_keys", "workflow_promises",
		"idempotency_keys", "workflow_update_requests",
		"workflow_memory_samples", "workflow_memory_stats",
	}
	for _, table := range expectedTables {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}

	// Verify idempotency — second call should not fail.
	SetupFullSchema(t, db, DialectPostgres)
}

func TestCleanupPostgresTestData(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	defer db.Close()

	SetupFullSchema(t, db, DialectPostgres)
	CleanupPostgresTestData(t, db)

	// Insert test data.
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version)
		VALUES ('test-cleanup-id', 'test-def', 1)`); err != nil {
		t.Fatalf("insert test instance: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_signals (workflow_id, signal_name)
		VALUES ('test-cleanup-id', 'test-signal')`); err != nil {
		t.Fatalf("insert test signal: %v", err)
	}

	// Run cleanup.
	CleanupPostgresTestData(t, db)

	// Verify cleanup removed the rows.
	var count int
	if err := db.QueryRow("SELECT count(*) FROM workflow_instances WHERE id = 'test-cleanup-id'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 workflow_instances after cleanup, got %d", count)
	}
	if err := db.QueryRow("SELECT count(*) FROM workflow_signals WHERE workflow_id = 'test-cleanup-id'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 workflow_signals after cleanup, got %d", count)
	}
}

func TestTestDB(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	if db == nil {
		t.Fatal("TestDB returned nil")
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("ping failed after TestDB: %v", err)
	}

	// Minimal schema should already be set up.
	var count int
	if err := db.QueryRow("SELECT count(*) FROM workflow_defs").Scan(&count); err != nil {
		t.Fatalf("minimal schema not set up: %v", err)
	}
}

func TestExecIgnoreDupKey(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	defer db.Close()
	CleanupPostgresTestData(t, db)
	SetupMinimalSchema(t, db, DialectPostgres)

	// execIgnoreDupKey is designed for MySQL but we can verify it doesn't
	// panic with a valid statement on PostgreSQL.
	execIgnoreDupKey(t, db, `CREATE INDEX IF NOT EXISTS idx_testutil_test ON workflow_instances(status)`)
}

func TestExecMSSQLBestEffort(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	defer db.Close()
	CleanupPostgresTestData(t, db)
	SetupMinimalSchema(t, db, DialectPostgres)

	// execMSSQLBestEffort ignores MSSQL-specific errors.
	// On PostgreSQL, a valid statement should succeed without error.
	execMSSQLBestEffort(t, db, `CREATE INDEX IF NOT EXISTS idx_testutil_mssql_test ON workflow_instances(status)`)
}

func TestCleanupTestData(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	defer db.Close()

	SetupFullSchema(t, db, DialectPostgres)
	CleanupPostgresTestData(t, db)

	runID := "test-cleanup-pattern-%"

	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version)
		VALUES ('test-cleanup-pattern-1', 'test-def', 1)`); err != nil {
		t.Fatalf("insert instance: %v", err)
	}

	CleanupTestData(t, db, DialectPostgres, runID)

	var count int
	if err := db.QueryRow("SELECT count(*) FROM workflow_instances WHERE id LIKE $1", runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after CleanupTestData, got %d", count)
	}
}
