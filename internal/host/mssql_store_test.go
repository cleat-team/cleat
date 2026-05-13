package host

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/cleat-team/cleat/internal/host/testutil"

	_ "github.com/microsoft/go-mssqldb"
)

// TestMSSQLStoreBasic validates that MSSQLStore can be created and
// connected. This test requires a running SQL Server instance.
// Set CLEAT_TEST_MSSQL to the connection string, or it will be skipped.
func TestMSSQLStoreBasic(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}

	db := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLMinimalSchema(t, db)

	store := NewMSSQLStore(db, "default")
	if store == nil {
		t.Fatal("NewMSSQLStore returned nil")
	}

	// Verify basic properties.
	if store.db == nil {
		t.Fatal("store.db is nil")
	}
	if len(store.taskQueues) != 1 || store.taskQueues[0] != "default" {
		t.Fatalf("unexpected taskQueues: %v", store.taskQueues)
	}

	// Cleanup.
	testutil.CleanupMSSQLTestData(t, db)
}

// TestMSSQLStoreFactory tests the factory creation path.
func TestMSSQLStoreFactory(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}

	connStr := os.Getenv("CLEAT_TEST_MSSQL")

	// Open a temporary connection for schema setup.
	setupDB, err := sql.Open("sqlserver", connStr)
	if err != nil {
		t.Fatalf("open setup DB: %v", err)
	}
	defer setupDB.Close()
	testutil.SetupMSSQLMinimalSchema(t, setupDB)
	defer testutil.CleanupMSSQLTestData(t, setupDB)

	factory := NewMSSQLStoreFactory(connStr)
	if factory == nil {
		t.Fatal("NewMSSQLStoreFactory returned nil")
	}

	if factory.DriverName() != "mssql" {
		t.Fatalf("unexpected driver name: %s", factory.DriverName())
	}

	store, closer, err := factory.OpenStore(context.Background(), "00000000-0000-0000-0000-000000000000", "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer closer.Close()

	if store == nil {
		t.Fatal("OpenStore returned nil store")
	}
}

// TestMSSQLStoreWithTenant tests tenant scoping.
func TestMSSQLStoreWithTenant(t *testing.T) {
	store := NewMSSQLStore(nil, "default")
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("unexpected default tenant: %s", store.tenantID)
	}

	scoped := store.WithTenant("11111111-1111-1111-1111-111111111111")
	if scoped.tenantID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("WithTenant did not set tenant: %s", scoped.tenantID)
	}
	if store.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Fatal("WithTenant mutated original store")
	}
}
