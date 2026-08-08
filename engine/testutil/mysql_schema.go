package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// SetupMySQLFullSchema creates all tables needed for full WorkflowStore testing
// against a MySQL 8.0+ or MariaDB 10.5+ backend. Uses CREATE TABLE IF NOT EXISTS
// for idempotency. The final column set includes all migration additions so no
// ALTER TABLE is needed for test setup.
//
// Deliberately does NOT pin an explicit CHARSET/COLLATE on these tables (unlike
// an earlier version of this file, which hardcoded utf8mb4_unicode_ci). The
// production migrations (migrations/mysql/*.sql) specify no charset/collation
// either, so both production tables and the stored procedures in
// 003_procedures.sql / 004_*.sql inherit whatever collation the target
// database was created with. Pinning a different, hardcoded collation here
// made this test schema disagree with the procedures applied on top of it by
// applyMySQLProcedures (store_backends_test.go), producing spurious "Illegal
// mix of collations" errors (1267) on MySQL 8.4+, whose server default
// (utf8mb4_0900_ai_ci) differs from the old hardcoded utf8mb4_unicode_ci.
// Letting these tables inherit the database default instead keeps this test
// schema's collation behavior identical to production's.
func SetupMySQLFullSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	statements := []string{
		// workflow_defs
		`CREATE TABLE IF NOT EXISTS workflow_defs (
			name               VARCHAR(255) NOT NULL,
			version            INTEGER NOT NULL,
			wasm_bytes         LONGBLOB NOT NULL,
			entry_points       JSON NOT NULL DEFAULT ('[]'),
			min_version        INTEGER NOT NULL DEFAULT 0,
			
			max_history_length INTEGER NOT NULL DEFAULT 0,
			dag_spec           JSON DEFAULT NULL,
			abi_version        INTEGER NOT NULL DEFAULT 1,
			plugin_deps        JSON NOT NULL DEFAULT ('{}'),
			deprecated         TINYINT(1) NOT NULL DEFAULT 0,
			task_queue         VARCHAR(255) NOT NULL DEFAULT 'default',
			tenant_id          VARCHAR(255),
			created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			PRIMARY KEY (name, version)
		) ENGINE=InnoDB`,

		// workflow_instances
		`CREATE TABLE IF NOT EXISTS workflow_instances (
			id                     VARCHAR(255) NOT NULL,
			def_name               VARCHAR(255) NOT NULL,
			def_version            INTEGER NOT NULL,
			status                 VARCHAR(50) NOT NULL DEFAULT 'ready',
			input                  JSON NOT NULL DEFAULT ('{}'),
			assigned_to            VARCHAR(255),
			heartbeat_at           TIMESTAMP(6),
			next_wake_at           TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			created_at             TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			completed_at           TIMESTAMP(6),
			cancellation_requested TINYINT(1) NOT NULL DEFAULT 0,
			cancellation_reason    TEXT,
			result                 JSON,
			error_msg              TEXT,
			error_code             VARCHAR(255),
			error_op               VARCHAR(255),
			parent_workflow_id     VARCHAR(255),
			parent_close_policy    VARCHAR(50) DEFAULT 'ABANDON',
			query_state            JSON DEFAULT ('{}'),
			
			trace_id               VARCHAR(255),
			sticky_worker_id       VARCHAR(255),
			task_queue             VARCHAR(255) NOT NULL DEFAULT 'default',
			compaction_state       JSON,
			compacted_at           TIMESTAMP(6),
			compaction_step        INTEGER,
			plugin_vers            JSON NOT NULL DEFAULT ('{}'),
			tenant_id              VARCHAR(255),
			priority               INTEGER NOT NULL DEFAULT 0,
			generation             BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (id),
			FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
		) ENGINE=InnoDB`,

		// event_history
		`CREATE TABLE IF NOT EXISTS event_history (
			workflow_id        VARCHAR(255) NOT NULL,
			step               INTEGER NOT NULL,
			event_type         VARCHAR(255) NOT NULL DEFAULT 'call',
			service            VARCHAR(255),
			operation          VARCHAR(255),
			request            TEXT,
			response           TEXT,
			error              TEXT,
			duration_ms        BIGINT,
			signal_names       TEXT,
			timeout_ms         BIGINT,
			signal_name        VARCHAR(255),
			signal_payload     TEXT,
			defer_description  TEXT,
			defer_id           VARCHAR(255),
			child_name         VARCHAR(255),
			child_input        TEXT,
			run_id             VARCHAR(255),
			new_input          TEXT,
			plugin_name        VARCHAR(255),
			plugin_func        VARCHAR(255),
			plugin_input       TEXT,
			plugin_output      TEXT,
			plugin_error       TEXT,
			promise_name       VARCHAR(255),
			promise_id         VARCHAR(255),
			promise_result     TEXT,
			promise_error      TEXT,
			payload            TEXT,
			created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			checksum           VARCHAR(255),
			-- Write-ahead call intent (1.4 phase D). Must match
			-- migrations/mysql/020_event_intent.sql: an event is pending iff
			-- intent_at IS NOT NULL AND checksum IS NULL, and LoadEventHistory
			-- selects that expression on every dialect.
			intent_at          TIMESTAMP(6) NULL DEFAULT NULL,
			tenant_id          VARCHAR(255),
			PRIMARY KEY (workflow_id, step),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB`,

		// workflow_signals
		`CREATE TABLE IF NOT EXISTS workflow_signals (
			workflow_id    VARCHAR(255) NOT NULL,
			signal_name    VARCHAR(255) NOT NULL,
			payload        TEXT NOT NULL,
			delivered_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			tenant_id      VARCHAR(255),
			PRIMARY KEY (workflow_id, signal_name),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB`,

		// workflow_schedules
		`CREATE TABLE IF NOT EXISTS workflow_schedules (
			name               VARCHAR(255) NOT NULL,
			def_name           VARCHAR(255) NOT NULL,
			entry_point        VARCHAR(255) NOT NULL DEFAULT '',
			cron_expression    TEXT NOT NULL,
			input              JSON NOT NULL DEFAULT ('{}'),
			enabled            TINYINT(1) NOT NULL DEFAULT 1,
			next_run_at        TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			last_run_at        TIMESTAMP(6),
			created_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			tenant_id          VARCHAR(255),
			timezone           VARCHAR(64) NOT NULL DEFAULT 'UTC',
			PRIMARY KEY (name)
		) ENGINE=InnoDB`,

		// concurrency_keys
		// Note: no FOREIGN KEY on workflow_id in test schema (intentionally omitted
		// for test isolation). Production now has CASCADE FK via migration 007.
		// The Postgres and MSSQL test schemas also omit FKs for test isolation.
		`CREATE TABLE IF NOT EXISTS concurrency_keys (
			key_hash     VARBINARY(32) PRIMARY KEY,
			key_text     TEXT NOT NULL,
			workflow_id  VARCHAR(255) NOT NULL,
			tenant_id    VARCHAR(255) NOT NULL DEFAULT '',
			acquired_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			expires_at   TIMESTAMP(6) NOT NULL
		) ENGINE=InnoDB`,

		// workflow_promises
		`CREATE TABLE IF NOT EXISTS workflow_promises (
			workflow_id   VARCHAR(255) NOT NULL,
			promise_id    VARCHAR(255) NOT NULL,
			promise_name  VARCHAR(255) NOT NULL,
			tenant_id     VARCHAR(255) NOT NULL,
			priority      INTEGER NOT NULL DEFAULT 0,
			status        VARCHAR(50) NOT NULL DEFAULT 'pending',
			result        JSON,
			error_msg     TEXT,
			created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			resolved_at   TIMESTAMP(6),
			PRIMARY KEY (workflow_id, promise_id),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB`,

		// workflow_update_requests
		`CREATE TABLE IF NOT EXISTS workflow_update_requests (
			workflow_id   VARCHAR(255) NOT NULL,
			update_name   VARCHAR(255) NOT NULL,
			priority      INTEGER NOT NULL DEFAULT 0,
			payload       TEXT NOT NULL,
			promise_id    VARCHAR(255),
			status        VARCHAR(50) NOT NULL DEFAULT 'pending',
			result        JSON,
			error_msg     TEXT,
			created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			completed_at  TIMESTAMP(6),
			PRIMARY KEY (workflow_id, update_name),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB`,

		// idempotency_keys. The primary key is (key_hash, tenant_id), not
		// key_hash alone: an Idempotency-Key is a client-supplied header, so
		// two tenants picking "order-123" must not collide.
		// migrations/mysql/010_idempotency_keys_tenant_id.sql,
		// IMPROVEMENT-PLAN 3.10.
		`CREATE TABLE IF NOT EXISTS idempotency_keys (
			key_hash     VARBINARY(32) NOT NULL,
			workflow_id  VARCHAR(255) NOT NULL,
			result       JSON,
			error_msg    TEXT,
			created_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			expires_at   TIMESTAMP(6) NOT NULL DEFAULT (NOW(6) + INTERVAL 7 DAY),
			tenant_id    CHAR(36) NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
			PRIMARY KEY (key_hash, tenant_id)
		) ENGINE=InnoDB`,

		// workflow_memory_samples
		`CREATE TABLE IF NOT EXISTS workflow_memory_samples (
			id            BIGINT AUTO_INCREMENT PRIMARY KEY,
			def_name      VARCHAR(255) NOT NULL,
			sample_bytes  BIGINT NOT NULL,
			recorded_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
		) ENGINE=InnoDB`,

		// workflow_memory_stats
		`CREATE TABLE IF NOT EXISTS workflow_memory_stats (
			def_name      VARCHAR(255) PRIMARY KEY,
			mean_bytes    DOUBLE NOT NULL DEFAULT 0,
			sample_count  INTEGER NOT NULL DEFAULT 0,
			alpha         DOUBLE NOT NULL DEFAULT 0.3,
			updated_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
		) ENGINE=InnoDB`,

		// plugin_defs
		`CREATE TABLE IF NOT EXISTS plugin_defs (
			name          VARCHAR(255) NOT NULL,
			version       VARCHAR(255) NOT NULL,
			wasm_bytes    LONGBLOB,
			config        JSON NOT NULL DEFAULT ('{}'),
			created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			deprecated    TINYINT(1) NOT NULL DEFAULT 0,
			PRIMARY KEY (name, version)
		) ENGINE=InnoDB`,

		// tenant_api_keys
		// Note: no FOREIGN KEY on tenant_id in test schema (intentionally
		// omitted for test isolation, same as concurrency_keys above) --
		// this test schema has no tenants table. The MSSQL test schema
		// (mssql_schema.go) omits the same FK for the same reason.
		`CREATE TABLE IF NOT EXISTS tenant_api_keys (
			key_id       VARCHAR(36) NOT NULL,
			tenant_id    VARCHAR(36) NOT NULL,
			key_hash     VARBINARY(32) NOT NULL,
			description  VARCHAR(1024) NOT NULL DEFAULT '',
			created_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			revoked_at   TIMESTAMP(6),
			PRIMARY KEY (key_id)
		) ENGINE=InnoDB`,
	}

	for i, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup MySQL full schema: statement %d: %v", i, err)
		}
	}

	// Migration: add columns that may be missing from older test databases.
	// Errors are ignored — if the column already exists, that's fine.
	migrations := []string{
		"ALTER TABLE workflow_instances ADD COLUMN event_count BIGINT NOT NULL DEFAULT 0",
		"ALTER TABLE workflow_instances ADD COLUMN allowed_signals JSON DEFAULT NULL",
	}
	for _, m := range migrations {
		_, _ = db.Exec(m) // best-effort, ignore errors
	}

	migrateMySQLIdempotencyTenantID(t, db)
	migrateMySQLEventIntentAt(t, db)

	// Indexes. These used to live in SetupFullSchema, which meant the schema you
	// got depended on which entry point the test called: SetupMySQLFullSchema
	// built the tables without them, SetupFullSchema built them with. Both now
	// route here, so there is one answer. IMPROVEMENT-PLAN 2.60b.
	//
	// MySQL has no CREATE INDEX IF NOT EXISTS, hence execIgnoreDupKey.
	execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_ready ON workflow_instances(status, next_wake_at)`)
	execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_heartbeat ON workflow_instances(assigned_to, heartbeat_at)`)
	execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_stale ON workflow_instances(status, heartbeat_at)`)
	execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_sticky ON workflow_instances(sticky_worker_id)`)
	_, _ = db.Exec(`DROP INDEX idx_instances_tenant_queue_ready ON workflow_instances`)
	execIgnoreDupKey(t, db, `CREATE INDEX idx_instances_tenant_queue_ready ON workflow_instances(tenant_id, task_queue, status, priority, next_wake_at)`)
	execIgnoreDupKey(t, db, `CREATE INDEX idx_concurrency_keys_workflow ON concurrency_keys(workflow_id)`)
	execIgnoreDupKey(t, db, `CREATE INDEX idx_idempotency_keys_expires ON idempotency_keys(expires_at)`)
	execIgnoreDupKey(t, db, `CREATE INDEX idx_mem_samples_def ON workflow_memory_samples(def_name, recorded_at DESC)`)

	// workflow_schedules.timezone, added by migrations/mysql/021.
	//
	// A no-op on a fresh database -- the CREATE TABLE above already declares
	// the column, and execIgnoreDupKey swallows MySQL's 1060 (Duplicate column
	// name). It matters when the table ALREADY EXISTS: the CREATE is guarded by
	// IF NOT EXISTS, and a guard on the TABLE says nothing about its COLUMNS,
	// so a database built by an earlier test from a schema predating the column
	// would keep its old shape and every CreateSchedule against it would fail.
	// CLAUDE.md's "CREATE TABLE IF NOT EXISTS never adds a column", in its
	// test-harness form. The SQL Server side has the same guard.
	execIgnoreDupKey(t, db, `ALTER TABLE workflow_schedules ADD COLUMN timezone VARCHAR(64) NOT NULL DEFAULT 'UTC'`)
}

// migrateMySQLEventIntentAt adds event_history.intent_at to an already-existing
// test database.
//
// The CREATE TABLE above declares it, which covers a database built from
// scratch and nothing else: it is IF NOT EXISTS against a shared, long-lived
// test database, so a table created before 1.4 phase D keeps its old shape and
// every LoadEventHistory fails with
//
//	Error 1054 (42S22): Unknown column 'intent_at' in 'field list'
//
// CI does not see this because its databases are new every run; a developer's
// is not. Same reason migrateMySQLIdempotencyTenantID exists, and the same
// failure mode IMPROVEMENT-PLAN 2.60b describes.
func migrateMySQLEventIntentAt(t *testing.T, db *sql.DB) {
	t.Helper()

	var haveColumn int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'event_history'
		  AND COLUMN_NAME = 'intent_at'`).Scan(&haveColumn); err != nil {
		t.Fatalf("setup MySQL full schema: check event_history.intent_at: %v", err)
	}
	if haveColumn == 0 {
		if _, err := db.Exec(`ALTER TABLE event_history
			ADD COLUMN intent_at TIMESTAMP(6) NULL DEFAULT NULL`); err != nil {
			t.Fatalf("setup MySQL full schema: add event_history.intent_at: %v", err)
		}
	}
}

// migrateMySQLIdempotencyTenantID brings an already-existing test database up
// to the (key_hash, tenant_id) primary key that
// migrations/mysql/010_idempotency_keys_tenant_id.sql introduces.
//
// The CREATE TABLE above cannot do this on its own: it is IF NOT EXISTS
// against a shared, long-lived test database, so a table created by an earlier
// checkout keeps its old shape forever and the new column simply never
// appears. That is the failure mode IMPROVEMENT-PLAN 2.60b describes -- the
// schema you get depends on when your database was created, and the suite
// still prints ok.
//
// Unlike the best-effort ALTERs above, this one fails the test rather than
// swallowing the error: with the column missing, StartNewRun's SQL does not
// merely degrade, it names a column that is not there, and a silent skip here
// would turn that into a confusing failure elsewhere.
func migrateMySQLIdempotencyTenantID(t *testing.T, db *sql.DB) {
	t.Helper()

	var haveColumn int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'idempotency_keys'
		  AND COLUMN_NAME = 'tenant_id'`).Scan(&haveColumn); err != nil {
		t.Fatalf("setup MySQL full schema: check idempotency_keys.tenant_id: %v", err)
	}
	if haveColumn == 0 {
		if _, err := db.Exec(`ALTER TABLE idempotency_keys
			ADD COLUMN tenant_id CHAR(36) NOT NULL
			DEFAULT '00000000-0000-0000-0000-000000000000'`); err != nil {
			t.Fatalf("setup MySQL full schema: add idempotency_keys.tenant_id: %v", err)
		}
	}

	// information_schema.STATISTICS has one row per key column, so a PRIMARY
	// of one row is the pre-010 shape.
	var pkColumns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'idempotency_keys'
		  AND INDEX_NAME = 'PRIMARY'`).Scan(&pkColumns); err != nil {
		t.Fatalf("setup MySQL full schema: check idempotency_keys primary key: %v", err)
	}
	switch pkColumns {
	case 1:
		if _, err := db.Exec(`ALTER TABLE idempotency_keys
			DROP PRIMARY KEY, ADD PRIMARY KEY (key_hash, tenant_id)`); err != nil {
			t.Fatalf("setup MySQL full schema: widen idempotency_keys primary key: %v", err)
		}
	case 0:
		if _, err := db.Exec(`ALTER TABLE idempotency_keys
			ADD PRIMARY KEY (key_hash, tenant_id)`); err != nil {
			t.Fatalf("setup MySQL full schema: add idempotency_keys primary key: %v", err)
		}
	}
}

// CleanupMySQLTestData removes all test data from MySQL tables.
// Order respects FK constraints — child tables first.
func CleanupMySQLTestData(t *testing.T, db *sql.DB) {
	t.Helper()

	tables := []string{
		"tenant_api_keys",
		"workflow_update_requests",
		"workflow_promises",
		"workflow_signals",
		"concurrency_keys",
		"idempotency_keys",
		"event_history",
		"workflow_memory_samples",
		"workflow_memory_stats",
		"workflow_schedules",
		"workflow_instances",
		"workflow_defs",
		"plugin_defs",
	}

	for _, table := range tables {
		if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s", table)); err != nil {
			t.Logf("cleanup: delete from %s: %v", table, err)
		}
	}
}

// MySQLTestDB opens a connection to the MySQL test database.
// Uses CLEAT_TEST_MYSQL environment variable.
// Default: root:cleat@tcp(127.0.0.1:3306)/cleat
func MySQLTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping MySQL database test in short mode")
	}

	dsn := os.Getenv("CLEAT_TEST_MYSQL")
	if dsn == "" {
		dsn = "root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true&multiStatements=true"
		t.Logf("CLEAT_TEST_MYSQL not set, using default: %s", dsn)
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL test DB: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("ping MySQL test DB: %v\nHint: Is MySQL running? Start with:\n  docker run -e MYSQL_ROOT_PASSWORD=cleat -e MYSQL_DATABASE=cleat -p 3306:3306 -d mysql:8.4", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}
