package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestIsDuplicateKeyError(t *testing.T) {
	mysqlErr := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	if !isDuplicateKeyError(mysqlErr) {
		t.Error("isDuplicateKeyError should return true for MySQL 1062")
	}

	mysqlErr1205 := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout"}
	if isDuplicateKeyError(mysqlErr1205) {
		t.Error("isDuplicateKeyError should return false for MySQL 1205")
	}

	if isDuplicateKeyError(errors.New("generic error")) {
		t.Error("isDuplicateKeyError should return false for non-MySQL error")
	}
}

func TestIsLockWaitTimeout(t *testing.T) {
	mysqlErr := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	if !isLockWaitTimeout(mysqlErr) {
		t.Error("isLockWaitTimeout should return true for MySQL 1205")
	}

	mysqlErr1062 := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	if isLockWaitTimeout(mysqlErr1062) {
		t.Error("isLockWaitTimeout should return false for MySQL 1062")
	}

	if isLockWaitTimeout(errors.New("generic error")) {
		t.Error("isLockWaitTimeout should return false for non-MySQL error")
	}
}

func TestIsDeadlockError(t *testing.T) {
	mysqlErr := &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
	if !isDeadlockError(mysqlErr) {
		t.Error("isDeadlockError should return true for MySQL 1213")
	}

	mysqlErr1062 := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	if isDeadlockError(mysqlErr1062) {
		t.Error("isDeadlockError should return false for MySQL 1062")
	}

	if isDeadlockError(errors.New("generic error")) {
		t.Error("isDeadlockError should return false for non-MySQL error")
	}
}

func TestMySQLStoreConfigOptions(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set")
	}

	db := openMySQLTestDB(t)
	defer db.Close()

	store := NewMySQLStore(db, "default")
	if store == nil {
		t.Fatal("NewMySQLStore returned nil")
	}

	// WithIdempotencyKeyTTL
	customTTL := 10 * time.Minute
	s2 := store.WithIdempotencyKeyTTL(customTTL)
	if s2.idempotencyKeyTTL != customTTL {
		t.Errorf("idempotencyKeyTTL = %v, want %v", s2.idempotencyKeyTTL, customTTL)
	}
	if store.idempotencyKeyTTL == customTTL {
		t.Error("original store should not be mutated by WithIdempotencyKeyTTL")
	}

	// WithReadRedactionDisabled
	s3 := store.WithReadRedactionDisabled(true)
	if !s3.disableReadRedaction {
		t.Error("disableReadRedaction should be true")
	}
	if store.disableReadRedaction {
		t.Error("original store should not be mutated by WithReadRedactionDisabled")
	}

	// WithEncryption
	s4 := store.WithEncryption(nil, false)
	if s4 == nil {
		t.Error("WithEncryption returned nil")
	}

	// WithTenant
	s5 := store.WithTenant("test-tenant-id")
	if s5.tenantID != "test-tenant-id" {
		t.Errorf("tenantID = %q, want %q", s5.tenantID, "test-tenant-id")
	}
	if store.tenantID == "test-tenant-id" {
		t.Error("original store should not be mutated by WithTenant")
	}
}

func TestMySQLStoreFactory(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set")
	}

	db := openMySQLTestDB(t)
	defer db.Close()

	// Use the connection without a database for the master.
	factory := NewMySQLStoreFactory(db, mysqlTestBaseDSN(t))
	if factory == nil {
		t.Fatal("NewMySQLStoreFactory returned nil")
	}

	if factory.DriverName() != "mysql" {
		t.Errorf("DriverName = %q, want %q", factory.DriverName(), "mysql")
	}

	if factory.Dialect() != DialectMySQL {
		t.Errorf("Dialect() = %v, want %v", factory.Dialect(), DialectMySQL)
	}

	// OpenStore should work with the default tenant.
	store, closer, err := factory.OpenStore(context.Background(), "00000000-0000-0000-0000-000000000000", "default")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if store == nil {
		t.Fatal("OpenStore returned nil store")
	}
	if err := closer.Close(); err != nil {
		t.Errorf("closer.Close: %v", err)
	}

	if err := factory.Close(); err != nil {
		t.Errorf("factory.Close: %v", err)
	}
}

func TestBuildTenantDSN(t *testing.T) {
	factory := &MySQLStoreFactory{
		baseDSN: "user:pass@tcp(localhost:3306)/?parseTime=true",
	}

	tests := []struct {
		dbName   string
		expected string
	}{
		{"cleat_test", "user:pass@tcp(localhost:3306)/cleat_test?parseTime=true"},
		{"cleat_00000000_0000_0000_0000_000000000000", "user:pass@tcp(localhost:3306)/cleat_00000000_0000_0000_0000_000000000000?parseTime=true"},
	}

	for _, tc := range tests {
		got := factory.buildTenantDSN(tc.dbName)
		if got != tc.expected {
			t.Errorf("buildTenantDSN(%q) = %q, want %q", tc.dbName, got, tc.expected)
		}
	}

	// Test without query parameters.
	factory2 := &MySQLStoreFactory{
		baseDSN: "user:pass@tcp(localhost:3306)/",
	}
	got := factory2.buildTenantDSN("mydb")
	if got != "user:pass@tcp(localhost:3306)/mydb" {
		t.Errorf("buildTenantDSN without params = %q, want %q", got, "user:pass@tcp(localhost:3306)/mydb")
	}
}

// openMySQLTestDB opens a connection to the MySQL test database using the same
// defaults as testutil.MySQLTestDB.
func openMySQLTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		dsn = "root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true&multiStatements=true"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL: %v", err)
	}
	return db
}
