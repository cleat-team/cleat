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
				ALTER TABLE task_queue ADD COLUMN def_name VARCHAR(255);
				ALTER TABLE task_queue ADD COLUMN input JSON;
				ALTER TABLE task_queue ADD COLUMN run_id VARCHAR(255);
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
			Version: 4,
			Up:      "",
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
	}
}
