package eventtriggers

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for event subscription management and
// event storage. Tables are idempotent (IF NOT EXISTS) and safe to run
// multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS event_subscriptions (
					id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
					tenant_id UUID NOT NULL,
					event_type TEXT NOT NULL,
					def_name TEXT NOT NULL,
					entry_point TEXT DEFAULT '',
					input_template JSONB DEFAULT '{}',
					filter_expr TEXT DEFAULT '',
					enabled BOOLEAN DEFAULT true,
					created_at TIMESTAMPTZ DEFAULT now()
				);
				CREATE TABLE IF NOT EXISTS ingested_events (
					id UUID PRIMARY KEY,
					tenant_id UUID NOT NULL,
					event_type TEXT NOT NULL,
					event_data JSONB DEFAULT '{}',
					received_at TIMESTAMPTZ DEFAULT now(),
					processed BOOLEAN DEFAULT false,
					error_msg TEXT
				);
				CREATE INDEX IF NOT EXISTS idx_event_subscriptions_type ON event_subscriptions(tenant_id, event_type);
				CREATE INDEX IF NOT EXISTS idx_ingested_events_unprocessed ON ingested_events(processed, received_at) WHERE NOT processed;
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS event_subscriptions (
					id CHAR(36) PRIMARY KEY,
					tenant_id CHAR(36) NOT NULL,
					event_type VARCHAR(255) NOT NULL,
					def_name VARCHAR(255) NOT NULL,
					entry_point VARCHAR(900) DEFAULT '',
					input_template JSON DEFAULT ('{}'),
					filter_expr VARCHAR(900) DEFAULT '',
					enabled TINYINT(1) DEFAULT 1,
					created_at TIMESTAMP(6) DEFAULT NOW(6)
				);
				CREATE TABLE IF NOT EXISTS ingested_events (
					id CHAR(36) PRIMARY KEY,
					tenant_id CHAR(36) NOT NULL,
					event_type VARCHAR(255) NOT NULL,
					event_data JSON DEFAULT ('{}'),
					received_at TIMESTAMP(6) DEFAULT NOW(6),
					processed TINYINT(1) DEFAULT 0,
					error_msg TEXT
				);
				CREATE INDEX idx_event_subscriptions_type ON event_subscriptions(tenant_id, event_type);
				CREATE INDEX idx_ingested_events_unprocessed ON ingested_events(processed, received_at);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'event_subscriptions')
				CREATE TABLE event_subscriptions (
					id UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID(),
					tenant_id UNIQUEIDENTIFIER NOT NULL,
					event_type NVARCHAR(255) NOT NULL,
					def_name NVARCHAR(MAX) NOT NULL,
					entry_point NVARCHAR(MAX) DEFAULT '',
					input_template NVARCHAR(MAX) DEFAULT ('{}'),
					filter_expr NVARCHAR(MAX) DEFAULT '',
					enabled BIT DEFAULT 1,
					created_at DATETIMEOFFSET DEFAULT SYSUTCDATETIME()
				);
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'ingested_events')
				CREATE TABLE ingested_events (
					id UNIQUEIDENTIFIER PRIMARY KEY,
					tenant_id UNIQUEIDENTIFIER NOT NULL,
					event_type NVARCHAR(MAX) NOT NULL,
					event_data NVARCHAR(MAX) DEFAULT ('{}'),
					received_at DATETIMEOFFSET DEFAULT SYSUTCDATETIME(),
					processed BIT DEFAULT 0,
					error_msg NVARCHAR(MAX)
				);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_event_subscriptions_type' AND object_id = OBJECT_ID('event_subscriptions'))
				CREATE INDEX idx_event_subscriptions_type ON event_subscriptions(tenant_id, event_type);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_unprocessed' AND object_id = OBJECT_ID('ingested_events'))
				CREATE INDEX idx_ingested_events_unprocessed ON ingested_events(processed, received_at) WHERE processed = 0;
			`,
			Down: `
				DROP TABLE IF EXISTS ingested_events;
				DROP TABLE IF EXISTS event_subscriptions;
			`,
		},
		{
			Version: 2,
			Up: `
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS retry_count INTEGER DEFAULT 0;
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS last_retry_at TIMESTAMPTZ;
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS status TEXT DEFAULT 'pending';
				ALTER TABLE event_subscriptions ADD COLUMN IF NOT EXISTS max_retries INTEGER DEFAULT 3;
			`,
			UpMySQL: "\n" +
				"\t\t\t\tALTER TABLE ingested_events ADD COLUMN retry_count INT DEFAULT 0;\n" +
				"\t\t\t\tALTER TABLE ingested_events ADD COLUMN last_retry_at TIMESTAMP(6);\n" +
				"\t\t\t\tALTER TABLE ingested_events ADD COLUMN `status` VARCHAR(255) DEFAULT 'pending';\n" +
				"\t\t\t\tALTER TABLE event_subscriptions ADD COLUMN max_retries INT DEFAULT 3;\n" +
				"\t\t\t",
			UpMSSQL: "\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'retry_count')\n" +
				"\t\t\t\tALTER TABLE ingested_events ADD retry_count INT DEFAULT 0;\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'last_retry_at')\n" +
				"\t\t\t\tALTER TABLE ingested_events ADD last_retry_at DATETIMEOFFSET;\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'status')\n" +
				"\t\t\t\tALTER TABLE ingested_events ADD [status] NVARCHAR(MAX) DEFAULT 'pending';\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_subscriptions') AND name = 'max_retries')\n" +
				"\t\t\t\tALTER TABLE event_subscriptions ADD max_retries INT DEFAULT 3;\n" +
				"\t\t\t",
			Down: `
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS retry_count;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS last_retry_at;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS status;
				ALTER TABLE event_subscriptions DROP COLUMN IF EXISTS max_retries;
			`,
		},
		{
			Version: 3,
			Up: `
				CREATE TABLE IF NOT EXISTS event_awaiters (
					workflow_id TEXT NOT NULL,
					tenant_id UUID NOT NULL,
					event_type TEXT NOT NULL,
					created_at TIMESTAMPTZ DEFAULT now(),
					PRIMARY KEY (workflow_id, event_type)
				);
				CREATE INDEX IF NOT EXISTS idx_event_awaiters_type ON event_awaiters(tenant_id, event_type);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS event_awaiters (
					workflow_id VARCHAR(255) NOT NULL,
					tenant_id CHAR(36) NOT NULL,
					event_type VARCHAR(255) NOT NULL,
					created_at TIMESTAMP(6) DEFAULT NOW(6),
					PRIMARY KEY (workflow_id, event_type)
				);
				CREATE INDEX idx_event_awaiters_type ON event_awaiters(tenant_id, event_type);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'event_awaiters')
				CREATE TABLE event_awaiters (
					workflow_id NVARCHAR(255) NOT NULL,
					tenant_id UNIQUEIDENTIFIER NOT NULL,
					event_type NVARCHAR(255) NOT NULL,
					created_at DATETIMEOFFSET DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (workflow_id, event_type)
				);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_event_awaiters_type' AND object_id = OBJECT_ID('event_awaiters'))
				CREATE INDEX idx_event_awaiters_type ON event_awaiters(tenant_id, event_type);
			`,
			Down: `
				DROP TABLE IF EXISTS event_awaiters;
			`,
		},
		{
			// Tenant isolation for the three event-trigger tables. cleat#1512.
			//
			// A new version rather than TenantScoped on v1: v1 is already
			// recorded everywhere eventtriggers runs, and a recorded migration
			// never runs again, so editing it would protect new databases and
			// leave every existing one open.
			//
			// Up is empty on purpose -- the runtime emits ENABLE / FORCE / the
			// policy from TenantScoped via plugin.applyTenantScoping, using
			// cleat.tenant_row_is_visible. Writing that SQL here would put a
			// second, drifting copy of the policy in the tree.
			//
			// All three tables carry tenant_id and are read on request paths,
			// host calls, and the retry sweep. The sweep is why this plugin
			// could not adopt scoping when kvstore did; see Run() and the
			// ForTenant call in background.go for how the scan and the
			// per-event work are separated.
			Version:      4,
			TenantScoped: []string{"ingested_events", "event_subscriptions", "event_awaiters"},
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
			// This is migrations/mysql/070 applied to eventtriggers, including the
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
			Version: 5,
			Up:      "",
			UpMySQL: `
				ALTER TABLE event_subscriptions
					MODIFY input_template LONGTEXT NULL DEFAULT ('{}');

				ALTER TABLE event_subscriptions
					ADD CONSTRAINT ck_event_subscriptions_input_template CHECK (input_template IS NULL OR JSON_VALID(input_template));
			`,
			// Reversal is MySQL-only because the change is. It restores the
			// JSON type and with it the narrowing -- a value stored intact
			// while this migration was applied is rewritten by the ALTER
			// itself, so this is lossy and is only here because a migration
			// that writes SQL must be reversible.
			DownMySQL: `
				ALTER TABLE event_subscriptions
					DROP CONSTRAINT ck_event_subscriptions_input_template;

				ALTER TABLE event_subscriptions
					MODIFY input_template JSON NULL DEFAULT ('{}');
			`,
		},
	}
}
