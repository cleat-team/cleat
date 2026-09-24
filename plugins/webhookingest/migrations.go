package webhookingest

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for webhook source management and
// event storage. Tables are idempotent (IF NOT EXISTS) and safe to run
// multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS webhook_sources (
					tenant_id   UUID NOT NULL,
					id          UUID PRIMARY KEY,
					name        TEXT,
					source_type TEXT NOT NULL DEFAULT 'generic',
					secret      TEXT NOT NULL DEFAULT '',
					enabled     BOOLEAN NOT NULL DEFAULT true,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS webhook_events (
					id          UUID PRIMARY KEY,
					source_id   UUID NOT NULL REFERENCES webhook_sources(id),
					tenant_id   UUID NOT NULL,
					event_type  TEXT NOT NULL DEFAULT '',
					headers     JSONB NOT NULL DEFAULT '{}',
					payload     JSONB NOT NULL DEFAULT '{}',
					received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
					processed   BOOLEAN NOT NULL DEFAULT false
				);

				CREATE INDEX IF NOT EXISTS idx_webhook_sources_tenant ON webhook_sources(tenant_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_events_source ON webhook_events(source_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_events_tenant ON webhook_events(tenant_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_events_processed ON webhook_events(processed);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS webhook_sources (
					tenant_id   CHAR(36) NOT NULL,
					id          CHAR(36) PRIMARY KEY,
					` + "`name`" + `      VARCHAR(255),
					source_type VARCHAR(255) NOT NULL DEFAULT 'generic',
					secret      VARCHAR(255) NOT NULL DEFAULT '',
					enabled     TINYINT(1) NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE TABLE IF NOT EXISTS webhook_events (
					id          CHAR(36) PRIMARY KEY,
					source_id   CHAR(36) NOT NULL REFERENCES webhook_sources(id),
					tenant_id   CHAR(36) NOT NULL,
					event_type  VARCHAR(255) NOT NULL DEFAULT '',
					headers     JSON NOT NULL DEFAULT ('{}'),
					payload     JSON NOT NULL DEFAULT ('{}'),
					received_at TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					processed   TINYINT(1) NOT NULL DEFAULT 0
				);

				CREATE INDEX idx_webhook_sources_tenant ON webhook_sources(tenant_id);
				CREATE INDEX idx_webhook_events_source ON webhook_events(source_id);
				CREATE INDEX idx_webhook_events_tenant ON webhook_events(tenant_id);
				CREATE INDEX idx_webhook_events_processed ON webhook_events(processed);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_sources')
				CREATE TABLE webhook_sources (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					id          UNIQUEIDENTIFIER PRIMARY KEY,
					[name]      NVARCHAR(MAX),
					source_type NVARCHAR(MAX) NOT NULL DEFAULT 'generic',
					secret      NVARCHAR(MAX) NOT NULL DEFAULT '',
					enabled     BIT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_events')
				CREATE TABLE webhook_events (
					id          UNIQUEIDENTIFIER PRIMARY KEY,
					source_id   UNIQUEIDENTIFIER NOT NULL REFERENCES webhook_sources(id),
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					event_type  NVARCHAR(MAX) NOT NULL DEFAULT '',
					headers     NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					payload     NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					received_at DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					processed   BIT NOT NULL DEFAULT 0
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_sources_tenant' AND object_id = OBJECT_ID('webhook_sources'))
				CREATE INDEX idx_webhook_sources_tenant ON webhook_sources(tenant_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_events_source' AND object_id = OBJECT_ID('webhook_events'))
				CREATE INDEX idx_webhook_events_source ON webhook_events(source_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_events_tenant' AND object_id = OBJECT_ID('webhook_events'))
				CREATE INDEX idx_webhook_events_tenant ON webhook_events(tenant_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_events_processed' AND object_id = OBJECT_ID('webhook_events'))
				CREATE INDEX idx_webhook_events_processed ON webhook_events(processed);
			`,
			Down: `
				DROP TABLE IF EXISTS webhook_events;
				DROP TABLE IF EXISTS webhook_sources;
			`,
		},
		{
			Version: 3,
			Up: `ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_workflow_id TEXT;
	ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_name TEXT NOT NULL DEFAULT 'webhook_received';`,
			UpMySQL: `ALTER TABLE webhook_sources ADD COLUMN signal_workflow_id VARCHAR(255);
	ALTER TABLE webhook_sources ADD COLUMN signal_name VARCHAR(255) NOT NULL DEFAULT 'webhook_received';`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'signal_workflow_id')
				ALTER TABLE webhook_sources ADD signal_workflow_id NVARCHAR(MAX);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'signal_name')
				ALTER TABLE webhook_sources ADD signal_name NVARCHAR(MAX) NOT NULL DEFAULT 'webhook_received';
			`,
			Down: `ALTER TABLE webhook_sources DROP COLUMN IF EXISTS signal_workflow_id;
	ALTER TABLE webhook_sources DROP COLUMN IF EXISTS signal_name;`,
		},
		{
			Version: 4,
			Up: `
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS retry_count INTEGER DEFAULT 0;
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS last_retry_at TIMESTAMPTZ;
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS status TEXT DEFAULT 'pending';
				ALTER TABLE webhook_events ADD COLUMN IF NOT EXISTS error_msg TEXT;
			`,
			UpMySQL: `
				ALTER TABLE webhook_events ADD COLUMN retry_count INT DEFAULT 0;
				ALTER TABLE webhook_events ADD COLUMN last_retry_at TIMESTAMP(6);
				ALTER TABLE webhook_events ADD COLUMN ` + "`status`" + ` VARCHAR(255) DEFAULT 'pending';
				ALTER TABLE webhook_events ADD COLUMN error_msg TEXT;
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'retry_count')
				ALTER TABLE webhook_events ADD retry_count INT DEFAULT 0;
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'last_retry_at')
				ALTER TABLE webhook_events ADD last_retry_at DATETIMEOFFSET;
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'status')
				ALTER TABLE webhook_events ADD [status] NVARCHAR(MAX) DEFAULT 'pending';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_events') AND name = 'error_msg')
				ALTER TABLE webhook_events ADD error_msg NVARCHAR(MAX);
			`,
			Down: `
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS error_msg;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS status;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS last_retry_at;
				ALTER TABLE webhook_events DROP COLUMN IF EXISTS retry_count;
			`,
		},
		{
			// Tenant isolation for both webhook tables. cleat#1512.
			//
			// Version 5, not 2: this plugin's versions run 1, 3, 4 -- there is
			// no 2 -- so the next free number is 5 rather than the count of
			// entries. A new version rather than TenantScoped on an existing
			// one, because a recorded migration never runs again and editing
			// one would protect new databases while leaving every existing one
			// open.
			//
			// Up is empty on purpose: the runtime emits ENABLE / FORCE / the
			// policy from TenantScoped via plugin.applyTenantScoping.
			//
			// Both tables, not just webhook_events: the retry scan LEFT JOINs
			// webhook_sources, and scoping one side of a join while leaving
			// the other open would make the pair's isolation depend on which
			// table a query happened to start from.
			Version:      5,
			TenantScoped: []string{"webhook_events", "webhook_sources"},
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
			// This is migrations/mysql/070 applied to webhookingest, including the
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
			Version:         6,
			Up:              "",
			DialectSpecific: "MySQL only: converting this plugin's JSON columns to LONGTEXT. PostgreSQL's JSONB preserves a large number already and SQL Server has always used NVARCHAR(MAX) here, so neither has anything to do and an arm for them would be a statement that must not exist. cleat#1622.",
			UpMySQL: `
				ALTER TABLE webhook_events
					MODIFY payload LONGTEXT NOT NULL DEFAULT ('{}');

				ALTER TABLE webhook_events
					ADD CONSTRAINT ck_webhook_events_payload CHECK (JSON_VALID(payload));
			`,
			// Reversal is MySQL-only because the change is. It restores the
			// JSON type and with it the narrowing -- a value stored intact
			// while this migration was applied is rewritten by the ALTER
			// itself, so this is lossy and is only here because a migration
			// that writes SQL must be reversible.
			DownMySQL: `
				ALTER TABLE webhook_events
					DROP CONSTRAINT ck_webhook_events_payload;

				ALTER TABLE webhook_events
					MODIFY payload JSON NOT NULL DEFAULT ('{}');
			`,
		},
		{
			// webhook_sources.secret moves into tenant secrets, sealed under the
			// same envelope encryption every other tenant secret uses. cleat#1992.
			// Mirrors plugins/notifications/migrations.go's v5, which this
			// plugin's #1992 sibling did first -- same column shape (DEFAULT
			// ''), same reasoning, same fix.
			//
			// secret_configured REPLACES it rather than merely accompanying it.
			// handleIngestWebhook can no longer answer "does this source have a
			// signing secret" by looking at its own secret column -- that value
			// now lives in a different store, envelope-encrypted, unreadable to
			// a WHERE clause -- so the row carries the answer itself instead.
			// The marker is honoured at ingest: unset means today's unsigned
			// behaviour, unchanged; set means the secret MUST be readable, and
			// any lookup failure (not found, empty, error) refuses the request
			// rather than silently accepting an unsigned payload for a source
			// that was supposed to require one.
			//
			// NO BACKFILL: 0.3.0 requires a fresh database, with no upgrade path
			// from v0.2.0 (cleat#2058, owner decision 3), so no deployment ever
			// has an existing plaintext secret to move -- ADD then DROP is
			// unconditionally correct.
			Version: 7,
			Up: `
				ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS secret_configured BOOLEAN NOT NULL DEFAULT false;
				ALTER TABLE webhook_sources DROP COLUMN IF EXISTS secret;
			`,
			UpMySQL: `
				ALTER TABLE webhook_sources ADD COLUMN secret_configured TINYINT(1) NOT NULL DEFAULT 0;
				ALTER TABLE webhook_sources DROP COLUMN secret;
			`,
			// v1's secret column carries DEFAULT '', which SQL Server backs
			// with an unnamed default constraint. DROP COLUMN refuses while
			// any object still depends on the column (error 5074), so the
			// constraint has to be found by its parent (object_id, column_id)
			// and dropped by name before the column can go -- the same move
			// plugins/tenantquota/migrations.go and
			// plugins/notifications/migrations.go's v5 use.
			//
			// NO BEGIN/END, on purpose: plugin.splitStatements shreds a
			// migration's SQL into separate exec calls on every literal ';',
			// with no awareness of T-SQL block structure -- a BEGIN in one
			// fragment and its END in another is two batches, neither valid
			// on its own. Each statement below has to be independently
			// complete. A missing 'secret' column makes the DECLARE/SELECT
			// above set @dfname to NULL rather than error, so the
			// constraint-drop step needs no existence guard of its own; only
			// the column DROP does.
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'secret_configured')
				ALTER TABLE webhook_sources ADD secret_configured BIT NOT NULL DEFAULT 0;

				DECLARE @dfname sysname
				SELECT @dfname = dc.name
					FROM sys.default_constraints dc
					JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id
					WHERE dc.parent_object_id = OBJECT_ID('webhook_sources') AND c.name = 'secret'
				IF @dfname IS NOT NULL EXEC('ALTER TABLE webhook_sources DROP CONSTRAINT [' + @dfname + ']');

				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'secret')
				ALTER TABLE webhook_sources DROP COLUMN secret;
			`,
			// Down restores the SCHEMA, not the data -- the value is gone
			// from webhook_sources the moment Up runs; it now lives in tenant
			// secrets, a different store. No attempt to recover
			// secret_configured's state into the restored column either:
			// existing rows have nothing to put there.
			Down: `
				ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS secret TEXT NOT NULL DEFAULT '';
				ALTER TABLE webhook_sources DROP COLUMN IF EXISTS secret_configured;
			`,
			DownMySQL: `
				ALTER TABLE webhook_sources ADD COLUMN secret VARCHAR(255) NOT NULL DEFAULT '';
				ALTER TABLE webhook_sources DROP COLUMN secret_configured;
			`,
			// Mirrors Up's DROP COLUMN secret above -- secret_configured's own
			// DEFAULT 0 gets an unnamed constraint too, and no BEGIN/END for
			// the same splitStatements reason given on UpMSSQL.
			DownMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'secret')
				ALTER TABLE webhook_sources ADD secret NVARCHAR(MAX) NOT NULL DEFAULT '';

				DECLARE @dfname sysname
				SELECT @dfname = dc.name
					FROM sys.default_constraints dc
					JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id
					WHERE dc.parent_object_id = OBJECT_ID('webhook_sources') AND c.name = 'secret_configured'
				IF @dfname IS NOT NULL EXEC('ALTER TABLE webhook_sources DROP CONSTRAINT [' + @dfname + ']');

				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'secret_configured')
				ALTER TABLE webhook_sources DROP COLUMN secret_configured;
			`,
		},
		{
			// A deleted source is soft-deleted, not removed. cleat#2199:
			// webhook_events.source_id REFERENCES webhook_sources(id) with no
			// ON DELETE action, so a hard DELETE on webhook_sources 500s on
			// PostgreSQL and SQL Server for any source with at least one event,
			// and on MySQL succeeds while orphaning that source's
			// webhook_events rows (InnoDB ignores an inline-column REFERENCES).
			//
			// Nothing is removed either way now -- handleDeleteSource sets
			// enabled = false and deleted_at, so the FK is never exercised on
			// any of the three dialects, and a source's ingested events (its
			// audit trail) survive the source that received them, which
			// matters most exactly when a source is deleted for a leaked
			// secret: that is an incident, and the history of what was
			// ingested during the compromise window is what an operator needs
			// kept, not erased.
			Version: 8,
			Up: `
					ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
				`,
			UpMySQL: `
					ALTER TABLE webhook_sources ADD COLUMN deleted_at TIMESTAMP(6) NULL;
				`,
			UpMSSQL: `
					IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'deleted_at')
					ALTER TABLE webhook_sources ADD deleted_at DATETIMEOFFSET NULL;
				`,
			Down: `
					ALTER TABLE webhook_sources DROP COLUMN IF EXISTS deleted_at;
				`,
			DownMySQL: `
					ALTER TABLE webhook_sources DROP COLUMN deleted_at;
				`,
			DownMSSQL: `
					IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_sources') AND name = 'deleted_at')
					ALTER TABLE webhook_sources DROP COLUMN deleted_at;
				`,
		},
	}
}
