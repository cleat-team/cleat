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

				CREATE INDEX idx_schedules_tenant_enabled_next
					ON schedules (tenant_id, enabled, next_run_at);
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
			Version: 3,
			Up:      "",
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
	}
}
