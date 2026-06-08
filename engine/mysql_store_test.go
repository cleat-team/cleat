package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
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
	factory := NewMySQLStoreFactory(db, "root:cleat@tcp(127.0.0.1:3306)/")
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

// ---------------------------------------------------------------------------
// Pure-logic tests (no database required)
// ---------------------------------------------------------------------------

func TestInClausePlaceholders(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, ""},
		{1, "?"},
		{2, "?, ?"},
		{3, "?, ?, ?"},
		{5, "?, ?, ?, ?, ?"},
	}
	for _, tt := range tests {
		got := inClausePlaceholders(tt.n)
		if got != tt.want {
			t.Errorf("inClausePlaceholders(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestTaskQueueClause(t *testing.T) {
	tests := []struct {
		name       string
		taskQueues []string
		wantPH     string
		wantArgs   []interface{}
	}{
		{
			name:       "default",
			taskQueues: []string{"default"},
			wantPH:     "?",
			wantArgs:   []interface{}{"default"},
		},
		{
			name:       "single custom",
			taskQueues: []string{"gpu"},
			wantPH:     "?",
			wantArgs:   []interface{}{"gpu"},
		},
		{
			name:       "multiple",
			taskQueues: []string{"default", "gpu", "high-memory"},
			wantPH:     "?, ?, ?",
			wantArgs:   []interface{}{"default", "gpu", "high-memory"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &MySQLStore{taskQueues: tt.taskQueues}
			ph, args := s.taskQueueClause()
			if ph != tt.wantPH {
				t.Errorf("placeholder = %q, want %q", ph, tt.wantPH)
			}
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("args len = %d, want %d", len(args), len(tt.wantArgs))
			}
			for i, a := range args {
				if a != tt.wantArgs[i] {
					t.Errorf("args[%d] = %v, want %v", i, a, tt.wantArgs[i])
				}
			}
		})
	}
}

func TestNewMySQLStoreDefaults(t *testing.T) {
	// Nil db — tests construction without needing a real connection.
	s := NewMySQLStore(nil)
	if s == nil {
		t.Fatal("NewMySQLStore returned nil")
	}
	if len(s.taskQueues) != 1 || s.taskQueues[0] != "default" {
		t.Errorf("taskQueues = %v, want [default]", s.taskQueues)
	}
	if s.dialect != DialectMySQL {
		t.Errorf("dialect = %q, want %q", s.dialect, DialectMySQL)
	}
	if s.idempotencyKeyTTL != 720*time.Hour {
		t.Errorf("idempotencyKeyTTL = %v, want 720h", s.idempotencyKeyTTL)
	}
	if s.tenantID != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("tenantID = %q, want default tenant", s.tenantID)
	}

	// Multiple task queues.
	s2 := NewMySQLStore(nil, "gpu", "high-memory")
	if len(s2.taskQueues) != 2 {
		t.Errorf("taskQueues len = %d, want 2", len(s2.taskQueues))
	}
	if s2.taskQueues[0] != "gpu" || s2.taskQueues[1] != "high-memory" {
		t.Errorf("taskQueues = %v, want [gpu high-memory]", s2.taskQueues)
	}
}

func TestMySQLStoreWithEncryptionNonNil(t *testing.T) {
	s := NewMySQLStore(nil)
	enc := &PayloadEncryption{key: make([]byte, 32)}
	s2 := s.WithEncryption(enc, true)
	if s2.encryption != enc {
		t.Error("encryption reference not stored")
	}
	if !s2.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be true")
	}
	if s.encryptSensitivePayloads {
		t.Error("original store should not be mutated")
	}
	if s.encryption != nil {
		t.Error("original store encryption should be nil")
	}

	// Toggle off.
	s3 := s2.WithEncryption(enc, false)
	if s3.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be false after disabling")
	}
	if s3.encryption != enc {
		t.Error("encryption reference should be preserved when disabling")
	}
}

func TestBuildTenantDSNEdgeCases(t *testing.T) {
	// Base DSN without a slash.
	f := &MySQLStoreFactory{baseDSN: "user:pass@tcp(localhost:3306)"}
	got := f.buildTenantDSN("mydb")
	if got != "user:pass@tcp(localhost:3306)mydb" {
		t.Errorf("buildTenantDSN (no slash) = %q", got)
	}

	// Base DSN with trailing slash, no query params.
	f2 := &MySQLStoreFactory{baseDSN: "root:cleat@tcp(127.0.0.1:3306)/"}
	got2 := f2.buildTenantDSN("cleat_test")
	if got2 != "root:cleat@tcp(127.0.0.1:3306)/cleat_test" {
		t.Errorf("buildTenantDSN (trailing slash) = %q, want .../cleat_test", got2)
	}
}

func TestNewMySQLStoreFactoryCustomTTL(t *testing.T) {
	// With custom TTL.
	customTTL := 1 * time.Hour
	f := NewMySQLStoreFactory(nil, "user:pass@tcp(localhost:3306)/", customTTL)
	if f.idempotencyKeyTTL != customTTL {
		t.Errorf("idempotencyKeyTTL = %v, want %v", f.idempotencyKeyTTL, customTTL)
	}

	// Without TTL — defaults to 720h.
	f2 := NewMySQLStoreFactory(nil, "user:pass@tcp(localhost:3306)/")
	if f2.idempotencyKeyTTL != 720*time.Hour {
		t.Errorf("default idempotencyKeyTTL = %v, want 720h", f2.idempotencyKeyTTL)
	}
}

func TestMySQLStoreConfigOptionsExtended(t *testing.T) {
	s := NewMySQLStore(nil)

	// WithReadRedactionDisabled round-trip.
	s2 := s.WithReadRedactionDisabled(true)
	if !s2.disableReadRedaction {
		t.Error("disableReadRedaction should be true")
	}
	s3 := s2.WithReadRedactionDisabled(false)
	if s3.disableReadRedaction {
		t.Error("disableReadRedaction should be false after toggling back")
	}

	// WithEncryption with nil key and enabled=false.
	s4 := s.WithEncryption(nil, false)
	if s4.encryptSensitivePayloads {
		t.Error("encryptSensitivePayloads should be false")
	}
	if s4.encryption != nil {
		t.Error("encryption should be nil")
	}
}

// ---------------------------------------------------------------------------
// DB-backed tests (require CLEAT_TEST_MYSQL)
// ---------------------------------------------------------------------------

func TestMySQLStoreBeginTx(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set")
	}

	db := openMySQLTestDB(t)
	defer db.Close()

	s := NewMySQLStore(db)
	tx, err := s.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}
	if tx == nil {
		t.Fatal("beginTx returned nil tx")
	}
	if err := tx.Rollback(); err != nil {
		t.Errorf("rollback: %v", err)
	}
}

func TestMySQLEnforceParentClosePolicy(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MYSQL") == "" {
		t.Skip("CLEAT_TEST_MYSQL not set")
	}

	db := testutil.MySQLTestDB(t)
	testutil.SetupMySQLFullSchema(t, db)
	defer testutil.CleanupMySQLTestData(t, db)

	// Insert a workflow_defs row to satisfy FK constraints.
	_, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES (?, ?, ?)`,
		"test-parent-close-policy", 1, []byte{})
	if err != nil {
		t.Fatalf("insert workflow_defs: %v", err)
	}

	parentID := uuid.New().String()
	childTerminateID := uuid.New().String()
	childCancelID := uuid.New().String()

	// Insert parent workflow.
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status) VALUES (?, ?, ?, ?)`,
		parentID, "test-parent-close-policy", 1, "running")
	if err != nil {
		t.Fatalf("insert parent workflow: %v", err)
	}

	// Insert TERMINATE child.
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, parent_workflow_id, parent_close_policy) VALUES (?, ?, ?, ?, ?, ?)`,
		childTerminateID, "test-parent-close-policy", 1, "running", parentID, "TERMINATE")
	if err != nil {
		t.Fatalf("insert terminate child: %v", err)
	}

	// Insert REQUEST_CANCEL child.
	_, err = db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, parent_workflow_id, parent_close_policy) VALUES (?, ?, ?, ?, ?, ?)`,
		childCancelID, "test-parent-close-policy", 1, "running", parentID, "REQUEST_CANCEL")
	if err != nil {
		t.Fatalf("insert cancel child: %v", err)
	}

	s := NewMySQLStore(db)
	s.enforceParentClosePolicy(context.Background(), parentID)

	// Verify TERMINATE child is now failed.
	var status, errorMsg string
	err = db.QueryRow(`SELECT status, error_msg FROM workflow_instances WHERE id = ?`, childTerminateID).Scan(&status, &errorMsg)
	if err != nil {
		t.Fatalf("query terminate child: %v", err)
	}
	if status != "failed" {
		t.Errorf("terminate child status = %q, want failed", status)
	}
	if errorMsg != "parent workflow terminated" {
		t.Errorf("terminate child error_msg = %q, want 'parent workflow terminated'", errorMsg)
	}

	// Verify REQUEST_CANCEL child has cancellation_requested.
	var cancelled bool
	err = db.QueryRow(`SELECT cancellation_requested FROM workflow_instances WHERE id = ?`, childCancelID).Scan(&cancelled)
	if err != nil {
		t.Fatalf("query cancel child: %v", err)
	}
	if !cancelled {
		t.Error("cancel child should have cancellation_requested = true")
	}

	// Verify parent is unchanged.
	var parentStatus string
	err = db.QueryRow(`SELECT status FROM workflow_instances WHERE id = ?`, parentID).Scan(&parentStatus)
	if err != nil {
		t.Fatalf("query parent: %v", err)
	}
	if parentStatus != "running" {
		t.Errorf("parent status = %q, want running", parentStatus)
	}
}
