package scheduler

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for the schedules table.
// Tables are idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS schedules (
					tenant_id     UUID NOT NULL,
					id            UUID PRIMARY KEY,
					name          TEXT NOT NULL,
					cron          TEXT NOT NULL,
					workflow_name TEXT NOT NULL,
					input         JSONB DEFAULT '{}',
					enabled       BOOLEAN DEFAULT true,
					last_run_at   TIMESTAMPTZ,
					next_run_at   TIMESTAMPTZ,
					created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_schedules_tenant_enabled_next
					ON schedules (tenant_id, enabled, next_run_at);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS schedules (
					tenant_id     CHAR(36) NOT NULL,
					id            CHAR(36) PRIMARY KEY,
					` + "`name`" + `        VARCHAR(255) NOT NULL,
					cron          VARCHAR(255) NOT NULL,
					workflow_name VARCHAR(255) NOT NULL,
					input         JSON DEFAULT ('{}'),
					enabled       TINYINT(1) DEFAULT 1,
					last_run_at   TIMESTAMP(6),
					next_run_at   TIMESTAMP(6),
					created_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at    TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'schedules' AND index_name = 'idx_schedules_tenant_enabled_next');
				SET @ddl := IF(@idx = 0, 'CREATE INDEX idx_schedules_tenant_enabled_next ON schedules (tenant_id, enabled, next_run_at)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'schedules')
				CREATE TABLE schedules (
					tenant_id     UNIQUEIDENTIFIER NOT NULL,
					id            UNIQUEIDENTIFIER PRIMARY KEY,
					[name]        NVARCHAR(MAX) NOT NULL,
					cron          NVARCHAR(MAX) NOT NULL,
					workflow_name NVARCHAR(MAX) NOT NULL,
					input         NVARCHAR(MAX) DEFAULT ('{}'),
					enabled       BIT DEFAULT 1,
					last_run_at   DATETIMEOFFSET,
					next_run_at   DATETIMEOFFSET,
					created_at    DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at    DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_schedules_tenant_enabled_next' AND object_id = OBJECT_ID('schedules'))
				CREATE INDEX idx_schedules_tenant_enabled_next
					ON schedules (tenant_id, enabled, next_run_at);
			`,
			Down: `
				DROP INDEX IF EXISTS idx_schedules_tenant_enabled_next;
				DROP TABLE IF EXISTS schedules;
			`,
		},
		{
			// Tenant isolation for schedules. cleat#1512.
			//
			// A new version rather than TenantScoped on v1: v1 is already
			// recorded everywhere scheduler runs and a recorded migration
			// never runs again, so editing it would protect new databases and
			// leave every existing one open.
			//
			// Up is empty on purpose -- the runtime emits ENABLE / FORCE / the
			// policy from TenantScoped via plugin.applyTenantScoping.
			//
			// The three CLI commands in commands.go are converted in the same
			// change, deliberately. They open their own *sql.DB and would
			// start failing the moment this policy exists; shipping the policy
			// without them is what broke `cleat jobqueue-enqueue` in #1511.
			Version:      2,
			TenantScoped: []string{"schedules"},
		},
		{
			// A caller's JSON is stored as TEXT on MySQL, as it already is on
			// SQL Server.
			//
			// cleat#1622, the plugin half of cleat#1022. MySQL's JSON type
			// keeps an integer as INT64 or UINT64 and falls back to DOUBLE
			// when it fits neither, so a value outside [-2^63, 2^64-1] -- and
			// any decimal needing more precision than a float64 holds -- is
			// REWRITTEN on the way in. Nothing errors, and the result is still
			// valid JSON of the right shape:
			//
			//     sent    {"x":123456789012345678901234567890}
			//     stored  {"x": 1.2345678901234566e29}
			//
			// The narrowing belongs to the JSON TYPE, not to any column: the
			// same INSERT into a TEXT column in the same row keeps the digits.
			// This is migrations/mysql/070 applied to scheduler, including the
			// CHECK that restores the validation LONGTEXT gives up.
			//
			// The CHECK is not optional: dropping to LONGTEXT surrenders the
			// JSON type's validation, and invalid JSON would become storable
			// where the column refuses it today. JSON_VALID restores that and
			// nothing else. It is parsed and IGNORED before MySQL 8.0.16, a
			// pre-existing dependency this repo already has.
			//
			// Existing rows are untouched and already-degraded values stay as
			// they are -- the digits were lost at write time and there is
			// nothing to recover. What changes is every write from here on.
			//
			// PostgreSQL and SQL Server need nothing: JSONB preserves, and
			// SQL Server has always used NVARCHAR(MAX) + ISJSON here.
			Version:         3,
			Up:              "",
			DialectSpecific: "MySQL only: converting this plugin's JSON columns to LONGTEXT. PostgreSQL's JSONB preserves a large number already and SQL Server has always used NVARCHAR(MAX) here, so neither has anything to do and an arm for them would be a statement that must not exist. cleat#1622.",
			UpMySQL: `
				ALTER TABLE schedules
					MODIFY input LONGTEXT NULL DEFAULT ('{}');

				ALTER TABLE schedules
					ADD CONSTRAINT ck_schedules_input CHECK (input IS NULL OR JSON_VALID(input));
			`,
			// Reversal is MySQL-only because the change is. It restores the
			// JSON type and with it the narrowing -- a value stored intact
			// while this migration was applied is rewritten by the ALTER
			// itself, so this is lossy and is only here because a migration
			// that writes SQL must be reversible.
			DownMySQL: `
				ALTER TABLE schedules
					DROP CONSTRAINT ck_schedules_input;

				ALTER TABLE schedules
					MODIFY input JSON NULL DEFAULT ('{}');
			`,
		},
		{
			// cleat#3641 (owner decision D60): this plugin's workflow-definition NAME column compares exactly
			// on SQL Server, as it already does on PostgreSQL and as migration 011 made the core columns.
			// Until now schedules.workflow_name inherited the database collation, which is
			// case-insensitive on a default install.
			//
			// NOTHING IN SQL COMPARES THIS COLUMN TODAY (it is written and read back in a select list, with no
			// predicate, join, grouping, ordering or key on it), so this fixes no user-visible behaviour. It makes the
			// schema uniformly exact, which is what a later comparison (event routing) needs.
			//
			// NOT METADATA-ONLY, MEASURED: the column is NVARCHAR(MAX), and on 1,000,000 rows (720 MB) a plain ALTER
			// COLUMN ... COLLATE took 9 to 17 s, wrote about 2.7 million log records and doubled the table's used space,
			// blocking reads and writes for its duration. #3635's metadata-only result was for NVARCHAR(255). This runs
			// under --migrate-only, with the core migrations. The type and nullability are restated exactly.
			Version:         4,
			Up:              "",
			DialectSpecific: "SQL Server only: PostgreSQL's comparison of this column is already exact, and MySQL's is a separate step (cleat#3658) because its cost and its decision differ (a write-blocking table copy).",
			UpMSSQL: `
				ALTER TABLE schedules ALTER COLUMN workflow_name NVARCHAR(MAX) COLLATE Latin1_General_100_BIN2 NOT NULL;
			`,
			// Reversal restores the database default, which is what the column had before.
			DownMSSQL: `
				ALTER TABLE schedules ALTER COLUMN workflow_name NVARCHAR(MAX) COLLATE DATABASE_DEFAULT NOT NULL;
			`,
		},
		{
			// cleat#3658 (owner decision D60): this plugin's workflow-definition NAME column compares exactly on
			// MySQL, as it already does on PostgreSQL and -- since step 4, cleat#3641 -- on SQL Server. See
			// plugins/eventtriggers/migrations.go's step 12 for the full reasoning; this is the same change.
			//
			// A NEW VERSION, NOT AN UpMySQL ADDED TO 4: MySQL has 4 recorded as applied already, because 4's Up
			// is empty and the runner records a version whether or not any SQL ran for that dialect.
			//
			// NOT IN-PLACE, MEASURED: ALGORITHM=INPLACE is refused (error 1845), so this asks for COPY,
			// LOCK=SHARED -- reads continue, writers wait for the whole copy. schedules holds one row per
			// schedule: 100,000 rows took 1.3 s on MySQL 8.4.11 with a 128 MB buffer pool. Nothing in SQL
			// compares this column today, so it fixes no visible behaviour.
			Version:         5,
			Up:              "",
			DialectSpecific: "MySQL only: PostgreSQL's comparison of this column is already exact, and SQL Server's was made exact by this plugin's step 4 (cleat#3641).",
			UpMySQL: `
				SET @col := (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'schedules' AND COLUMN_NAME = 'workflow_name');
				SET @ddl := IF(@col <> 'utf8mb4_0900_bin', 'ALTER TABLE schedules MODIFY workflow_name VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL, ALGORITHM=COPY, LOCK=SHARED', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			// Reversal restores the server default, which is what the column had before. It costs the same copy.
			DownMySQL: `
				SET @col := (SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'schedules' AND COLUMN_NAME = 'workflow_name');
				SET @ddl := IF(@col <> 'utf8mb4_0900_ai_ci', 'ALTER TABLE schedules MODIFY workflow_name VARCHAR(255) COLLATE utf8mb4_0900_ai_ci NOT NULL, ALGORITHM=COPY, LOCK=SHARED', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
		},
	}
}
