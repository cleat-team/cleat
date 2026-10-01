package jobqueue

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for the job queue. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS task_queue (
					tenant_id    UUID NOT NULL,
					queue_name   TEXT NOT NULL,
					job_id       UUID NOT NULL,
					payload      JSONB NOT NULL DEFAULT '{}',
					status       TEXT NOT NULL DEFAULT 'pending',
					created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
					started_at   TIMESTAMPTZ,
					completed_at TIMESTAMPTZ,
					PRIMARY KEY (tenant_id, queue_name, job_id)
				);

				CREATE INDEX IF NOT EXISTS idx_task_queue_status ON task_queue(tenant_id, status);
				CREATE INDEX IF NOT EXISTS idx_task_queue_created ON task_queue(tenant_id, queue_name, created_at DESC);
			`,
			UpMySQL: "\n" +
				"\t\t\t\tCREATE TABLE IF NOT EXISTS task_queue (\n" +
				"\t\t\t\t\ttenant_id    CHAR(36) NOT NULL,\n" +
				"\t\t\t\t\tqueue_name   VARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\tjob_id       CHAR(36) NOT NULL,\n" +
				"\t\t\t\t\tpayload      JSON NOT NULL DEFAULT ('{}'),\n" +
				"\t\t\t\t\t`status`     VARCHAR(255) NOT NULL DEFAULT 'pending',\n" +
				"\t\t\t\t\tcreated_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6),\n" +
				"\t\t\t\t\tstarted_at   TIMESTAMP(6),\n" +
				"\t\t\t\t\tcompleted_at TIMESTAMP(6),\n" +
				"\t\t\t\t\tPRIMARY KEY (tenant_id, queue_name, job_id)\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_status ON task_queue(tenant_id, `status`);\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_created ON task_queue(tenant_id, queue_name, created_at DESC);\n" +
				"\t\t\t",
			UpMSSQL: "\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'task_queue')\n" +
				"\t\t\t\tCREATE TABLE task_queue (\n" +
				"\t\t\t\t\ttenant_id    UNIQUEIDENTIFIER NOT NULL,\n" +
				"\t\t\t\t\tqueue_name   NVARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\tjob_id       UNIQUEIDENTIFIER NOT NULL,\n" +
				"\t\t\t\t\tpayload      NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),\n" +
				"\t\t\t\t\t[status]     NVARCHAR(255) NOT NULL DEFAULT 'pending',\n" +
				"\t\t\t\t\tcreated_at   DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),\n" +
				"\t\t\t\t\tstarted_at   DATETIMEOFFSET,\n" +
				"\t\t\t\t\tcompleted_at DATETIMEOFFSET,\n" +
				"\t\t\t\t\tPRIMARY KEY (tenant_id, queue_name, job_id)\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_task_queue_status' AND object_id = OBJECT_ID('task_queue'))\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_status ON task_queue(tenant_id, [status]);\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_task_queue_created' AND object_id = OBJECT_ID('task_queue'))\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_created ON task_queue(tenant_id, queue_name, created_at DESC);\n" +
				"\t\t\t",
			Down: `
				DROP TABLE IF EXISTS task_queue;
			`,
		},
		{
			Version: 2,
			Up: `ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS def_name TEXT;
			     ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS input JSONB;
			     ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS run_id TEXT;`,
			UpMySQL: `
				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'task_queue' AND column_name = 'def_name');
				SET @ddl := IF(@col = 0, 'ALTER TABLE task_queue ADD COLUMN def_name VARCHAR(255)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'task_queue' AND column_name = 'input');
				SET @ddl := IF(@col = 0, 'ALTER TABLE task_queue ADD COLUMN input JSON', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'task_queue' AND column_name = 'run_id');
				SET @ddl := IF(@col = 0, 'ALTER TABLE task_queue ADD COLUMN run_id VARCHAR(255)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('task_queue') AND name = 'def_name')
				ALTER TABLE task_queue ADD def_name NVARCHAR(MAX);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('task_queue') AND name = 'input')
				ALTER TABLE task_queue ADD input NVARCHAR(MAX);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('task_queue') AND name = 'run_id')
				ALTER TABLE task_queue ADD run_id NVARCHAR(MAX);
			`,
			Down: `ALTER TABLE task_queue DROP COLUMN IF EXISTS run_id;
			       ALTER TABLE task_queue DROP COLUMN IF EXISTS input;
			       ALTER TABLE task_queue DROP COLUMN IF EXISTS def_name;`,
		},
		{
			// Tenant isolation for task_queue. cleat#1278.
			//
			// A separate version rather than a TenantScoped on v1, for the
			// reason kvstore's v2 gives: v1 is already recorded as applied
			// everywhere jobqueue runs, and a recorded migration never runs
			// again. Editing it would protect new databases and leave every
			// existing one open.
			//
			// Up is empty on purpose. The runtime emits ENABLE / FORCE / the
			// policy from TenantScoped (plugin.applyTenantScoping), using
			// cleat.tenant_row_is_visible so that a sweep which named itself
			// through plugin.AcrossAllTenants is admitted and an unmarked one
			// still fails closed. On MySQL and SQL Server this version is
			// recorded and installs nothing, which is what the field means.
			//
			// jobqueue could not adopt this when kvstore did, because kvstore
			// qualified only by having no background loop. The loop is what
			// made this hard, and Run() naming itself cross-tenant is what
			// makes it possible -- see plugins/jobqueue/background.go.
			Version:      3,
			TenantScoped: []string{"task_queue"},
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
			// This is migrations/mysql/070 applied to jobqueue, including the
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
			Version:         4,
			Up:              "",
			DialectSpecific: "MySQL only: converting this plugin's JSON columns to LONGTEXT. PostgreSQL's JSONB preserves a large number already and SQL Server has always used NVARCHAR(MAX) here, so neither has anything to do and an arm for them would be a statement that must not exist. cleat#1622.",
			UpMySQL: `
				ALTER TABLE task_queue
					MODIFY input LONGTEXT NULL,
					MODIFY payload LONGTEXT NULL;

				ALTER TABLE task_queue
					ADD CONSTRAINT ck_task_queue_input CHECK (input IS NULL OR JSON_VALID(input)),
					ADD CONSTRAINT ck_task_queue_payload CHECK (payload IS NULL OR JSON_VALID(payload));
			`,
			// Reversal is MySQL-only because the change is. It restores the
			// JSON type and with it the narrowing -- a value stored intact
			// while this migration was applied is rewritten by the ALTER
			// itself, so this is lossy and is only here because a migration
			// that writes SQL must be reversible.
			DownMySQL: `
				ALTER TABLE task_queue
					DROP CONSTRAINT ck_task_queue_input,
					DROP CONSTRAINT ck_task_queue_payload;

				ALTER TABLE task_queue
					MODIFY input JSON NULL,
					MODIFY payload JSON NULL;
			`,
		},
		{
			// v4's `MODIFY payload LONGTEXT NULL` restated the column
			// without NOT NULL or DEFAULT ('{}') -- MySQL's MODIFY
			// redefines a column wholesale, so anything from v1 not
			// repeated there is gone, not merely left alone. cleat#2282:
			// checked empirically against a scratch database carrying v4,
			// `DESCRIBE task_queue` reports payload as `NULL: YES,
			// Default: NULL` -- both attributes lost, not just
			// nullability. PostgreSQL (JSONB NOT NULL DEFAULT '{}', v1,
			// untouched since) and SQL Server (NVARCHAR(MAX) NOT NULL
			// DEFAULT ('{}'), v1, untouched -- v4's own DialectSpecific
			// note: "SQL Server has always used NVARCHAR(MAX) ... here")
			// never had a v4 arm, so only MySQL drifted.
			//
			// No tracked, non-test INSERT into task_queue omits payload
			// (commands.go, routes.go both supply it), so this is not
			// known to have produced a NULL row yet -- but the column
			// having neither a NOT NULL guard nor a DEFAULT means any
			// FUTURE insert that omits it, on MySQL only, would silently
			// write NULL where PostgreSQL and SQL Server fall back to
			// '{}'. That asymmetry, not a known bad row, is what this
			// closes.
			//
			// THE BACKFILL IS NOT OPTIONAL. Measured directly: seeding one
			// row with no payload (so it reads NULL under v4's shape),
			// then running a bare `MODIFY payload LONGTEXT NOT NULL
			// DEFAULT ('{}')` with no backfill, fails --
			// `ERROR 1265 (01000): Data truncated for column 'payload' at
			// row 1` -- on MySQL 8.4, strict mode (this repo's default).
			// A one-line "add NOT NULL back" migration is unsafe on any
			// database already carrying a NULL row; the UPDATE first is
			// what makes it safe on every database, whether or not one
			// exists there today.
			//
			// input is NOT part of this: it has never been NOT NULL on
			// any dialect (v2 added it plain JSONB/JSON/NVARCHAR(MAX),
			// nullable everywhere), so there is no drift to close there.
			Version:         5,
			Up:              "",
			DialectSpecific: "MySQL only: restoring payload's NOT NULL DEFAULT ('{}'), which v4's MODIFY silently dropped. PostgreSQL and SQL Server never lost it, so an arm for them would be a statement that must not exist. cleat#2282.",
			UpMySQL: `
				UPDATE task_queue SET payload = '{}' WHERE payload IS NULL;
				ALTER TABLE task_queue MODIFY payload LONGTEXT NOT NULL DEFAULT ('{}');
			`,
			// Reversal restores v4's shape (nullable, no default) -- a row
			// backfilled to '{}' by Up's UPDATE stays '{}' rather than
			// reverting to NULL, the same lossy-but-reversible tradeoff
			// v4's own Down makes for input/payload's type change.
			DownMySQL: `
				ALTER TABLE task_queue MODIFY payload LONGTEXT NULL;
			`,
		},
	}
}
