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
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

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
			generation             BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (id),
			FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

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
			tenant_id          VARCHAR(255),
			PRIMARY KEY (workflow_id, step),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// workflow_signals
		`CREATE TABLE IF NOT EXISTS workflow_signals (
			workflow_id    VARCHAR(255) NOT NULL,
			signal_name    VARCHAR(255) NOT NULL,
			payload        TEXT NOT NULL,
			delivered_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			tenant_id      VARCHAR(255),
			PRIMARY KEY (workflow_id, signal_name),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

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
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// concurrency_keys
		// Note: no FOREIGN KEY on workflow_id — concurrency keys are ephemeral
		// and workflow instances may be cleaned up before their keys are released.
		// This matches the Postgres and MSSQL test schemas.
		`CREATE TABLE IF NOT EXISTS concurrency_keys (
			key_hash     VARBINARY(32) PRIMARY KEY,
			key_text     TEXT NOT NULL,
			workflow_id  VARCHAR(255) NOT NULL,
			tenant_id    VARCHAR(255) NOT NULL DEFAULT '',
			acquired_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			expires_at   TIMESTAMP(6) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// workflow_promises
		`CREATE TABLE IF NOT EXISTS workflow_promises (
			workflow_id   VARCHAR(255) NOT NULL,
			promise_id    VARCHAR(255) NOT NULL,
			promise_name  VARCHAR(255) NOT NULL,
			tenant_id     VARCHAR(255) NOT NULL,
			status        VARCHAR(50) NOT NULL DEFAULT 'pending',
			result        JSON,
			error_msg     TEXT,
			created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			resolved_at   TIMESTAMP(6),
			PRIMARY KEY (workflow_id, promise_id),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// workflow_update_requests
		`CREATE TABLE IF NOT EXISTS workflow_update_requests (
			workflow_id   VARCHAR(255) NOT NULL,
			update_name   VARCHAR(255) NOT NULL,
			payload       TEXT NOT NULL,
			promise_id    VARCHAR(255),
			status        VARCHAR(50) NOT NULL DEFAULT 'pending',
			result        JSON,
			error_msg     TEXT,
			created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			completed_at  TIMESTAMP(6),
			PRIMARY KEY (workflow_id, update_name),
			FOREIGN KEY (workflow_id) REFERENCES workflow_instances(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// idempotency_keys
		`CREATE TABLE IF NOT EXISTS idempotency_keys (
			key_hash     VARBINARY(32) PRIMARY KEY,
			workflow_id  VARCHAR(255) NOT NULL,
			result       JSON,
			error_msg    TEXT,
			created_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			expires_at   TIMESTAMP(6) NOT NULL DEFAULT (NOW(6) + INTERVAL 7 DAY)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// workflow_memory_samples
		`CREATE TABLE IF NOT EXISTS workflow_memory_samples (
			id            BIGINT AUTO_INCREMENT PRIMARY KEY,
			def_name      VARCHAR(255) NOT NULL,
			sample_bytes  BIGINT NOT NULL,
			recorded_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// workflow_memory_stats
		`CREATE TABLE IF NOT EXISTS workflow_memory_stats (
			def_name      VARCHAR(255) PRIMARY KEY,
			mean_bytes    DOUBLE NOT NULL DEFAULT 0,
			sample_count  INTEGER NOT NULL DEFAULT 0,
			alpha         DOUBLE NOT NULL DEFAULT 0.3,
			updated_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// plugin_defs
		`CREATE TABLE IF NOT EXISTS plugin_defs (
			name          VARCHAR(255) NOT NULL,
			version       VARCHAR(255) NOT NULL,
			wasm_bytes    LONGBLOB,
			config        JSON NOT NULL DEFAULT ('{}'),
			created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
			deprecated    TINYINT(1) NOT NULL DEFAULT 0,
			PRIMARY KEY (name, version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}

	for i, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup MySQL full schema: statement %d: %v", i, err)
		}
	}
}

// CleanupMySQLTestData removes all test data from MySQL tables.
// Order respects FK constraints — child tables first.
func CleanupMySQLTestData(t *testing.T, db *sql.DB) {
	t.Helper()

	tables := []string{
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
