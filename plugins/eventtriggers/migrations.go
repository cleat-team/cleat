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
			Version:         5,
			Up:              "",
			DialectSpecific: "MySQL only: converting this plugin's JSON columns to LONGTEXT. PostgreSQL's JSONB preserves a large number already and SQL Server has always used NVARCHAR(MAX) here, so neither has anything to do and an arm for them would be a statement that must not exist. cleat#1622.",
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
		{
			// Key slots and correlation -- P1 of
			// docs/contributor/design/event-routing-design.md. cleat#2625.
			//
			// Three string slots, added to both sides of a match: the event
			// that arrived and the run that is waiting. `await_event` gains a
			// Keys parameter (host_functions.go) that populates these on the
			// awaiter row, and PublishEvent populates them on the event row
			// from the publisher's envelope. Equality on all three is what
			// correlation IS -- see the design doc's §2 for why this has to
			// be an indexed equality rather than anything more expressive.
			//
			// NOT NULL DEFAULT '', never NULL: NULL = NULL is unknown in SQL,
			// and the portable fix (IS NOT DISTINCT FROM) is not portable
			// across all three dialects. A sentinel empty string is what
			// makes three-way equality behave identically everywhere, and it
			// is also what makes this backward compatible with every
			// existing row and every existing call: an event or an awaiter
			// that never mentions keys gets '' in all three slots on both
			// sides, so old-shape publishes and old-shape awaits still match
			// each other exactly as before.
			//
			// Binary collation, explicit rather than inherited: this repo's
			// migrations never specify one anywhere else
			// (`grep -rhno 'COLLATE [A-Za-z0-9_]*' migrations/` finds
			// nothing), so every string column takes its server's default --
			// and MySQL 8's default, utf8mb4_0900_ai_ci, is
			// accent-and-case-insensitive. Under that collation "ORDER-1"
			// and "order-1" correlate as the same key on one dialect only,
			// silently. An opaque correlation key wants byte-exact equality,
			// which a binary collation gives on all three.
			//
			// 128 bytes, not more: three VARCHAR(128) slots under
			// utf8mb4_bin cost up to 1536 bytes of InnoDB's 3072-byte index
			// key limit once tenant_id and event_type are added ahead of
			// them -- comfortable, but a fourth slot or a wider one would
			// not fit. See the design doc's §4.3 for the full budget.
			//
			// event_awaiters' primary key moves from (workflow_id,
			// event_type) to a surrogate id. The composite key meant a
			// second `await_event` call for the same type -- in a loop, or
			// awaiting a second order while the first is still pending --
			// upserted over the FIRST registration rather than adding a
			// second one, so only the most recent await could ever be woken.
			// A surrogate key lets two awaits for the same (workflow, type)
			// coexist as long as their key slots differ, which is the whole
			// point of adding key slots in the first place. The unique index
			// below on the five-column tuple keeps the one property the
			// composite PK was actually enforcing -- a REPLAYED await_event
			// call (this host function is Idempotent: false,
			// SameValueOnReplay: false, so a replay re-executes for real)
			// re-registers the same row rather than accumulating a duplicate
			// -- without collapsing two awaits that differ only in their
			// keys.
			//
			// `seq`/`event_subscriptions.source`/`def_version` from the
			// design doc's §8 are NOT part of this migration. Both belong to
			// later phases -- seq is a P2 concern (the suspend/resume
			// watermark, §6.5), source/def_version is P3's (triggers
			// declared in source, §7) -- and P1 is scoped to correlation
			// alone, per IMPROVEMENT-PLAN's phasing.
			Version: 6,
			Up: `
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS key1 VARCHAR(128) COLLATE "C" NOT NULL DEFAULT '';
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS key2 VARCHAR(128) COLLATE "C" NOT NULL DEFAULT '';
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS key3 VARCHAR(128) COLLATE "C" NOT NULL DEFAULT '';
				CREATE INDEX IF NOT EXISTS idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at);

				ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS key1 VARCHAR(128) COLLATE "C" NOT NULL DEFAULT '';
				ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS key2 VARCHAR(128) COLLATE "C" NOT NULL DEFAULT '';
				ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS key3 VARCHAR(128) COLLATE "C" NOT NULL DEFAULT '';

				ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS id UUID DEFAULT gen_random_uuid();
				UPDATE event_awaiters SET id = gen_random_uuid() WHERE id IS NULL;
				ALTER TABLE event_awaiters ALTER COLUMN id SET NOT NULL;
				ALTER TABLE event_awaiters DROP CONSTRAINT IF EXISTS event_awaiters_pkey;
				ALTER TABLE event_awaiters ADD CONSTRAINT event_awaiters_pkey PRIMARY KEY (id);

				CREATE UNIQUE INDEX IF NOT EXISTS uq_event_awaiters_registration ON event_awaiters(workflow_id, event_type, key1, key2, key3);
				CREATE INDEX IF NOT EXISTS idx_event_awaiters_correlate ON event_awaiters(tenant_id, event_type, key1, key2, key3);
			`,
			UpMySQL: `
				ALTER TABLE ingested_events ADD COLUMN key1 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '';
				ALTER TABLE ingested_events ADD COLUMN key2 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '';
				ALTER TABLE ingested_events ADD COLUMN key3 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '';
				CREATE INDEX idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at);

				ALTER TABLE event_awaiters ADD COLUMN key1 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '';
				ALTER TABLE event_awaiters ADD COLUMN key2 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '';
				ALTER TABLE event_awaiters ADD COLUMN key3 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '';

				ALTER TABLE event_awaiters ADD COLUMN id CHAR(36) NULL;
				UPDATE event_awaiters SET id = UUID() WHERE id IS NULL;
				ALTER TABLE event_awaiters MODIFY id CHAR(36) NOT NULL;
				ALTER TABLE event_awaiters DROP PRIMARY KEY, ADD PRIMARY KEY (id);

				CREATE UNIQUE INDEX uq_event_awaiters_registration ON event_awaiters(workflow_id, event_type, key1, key2, key3);
				CREATE INDEX idx_event_awaiters_correlate ON event_awaiters(tenant_id, event_type, key1, key2, key3);
			`,
			// The PK swap looks up the existing auto-named constraint rather
			// than assuming a name, the same shape as
			// plugins/scheduledbackup/migrations.go's foreign-key
			// replacement: find whatever is there under a DIFFERENT name
			// than the one this migration is about to install, drop it if
			// found, then add the new one guarded by existence. That makes
			// this idempotent by construction rather than by an outer
			// "has this version already run" check.
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'key1')
				ALTER TABLE ingested_events ADD key1 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'key2')
				ALTER TABLE ingested_events ADD key2 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'key3')
				ALTER TABLE ingested_events ADD key3 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_correlate' AND object_id = OBJECT_ID('ingested_events'))
				CREATE INDEX idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at);

				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'key1')
				ALTER TABLE event_awaiters ADD key1 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'key2')
				ALTER TABLE event_awaiters ADD key2 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'key3')
				ALTER TABLE event_awaiters ADD key3 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';

				DECLARE @pkname sysname
				SELECT @pkname = kc.name
					FROM sys.key_constraints kc
					WHERE kc.parent_object_id = OBJECT_ID('event_awaiters')
					  AND kc.type = 'PK'
					  AND kc.name <> 'pk_event_awaiters'
				IF @pkname IS NOT NULL EXEC('ALTER TABLE event_awaiters DROP CONSTRAINT [' + @pkname + ']');

				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'id')
				ALTER TABLE event_awaiters ADD id UNIQUEIDENTIFIER DEFAULT NEWID();
				UPDATE event_awaiters SET id = NEWID() WHERE id IS NULL;
				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'id' AND is_nullable = 1)
				ALTER TABLE event_awaiters ALTER COLUMN id UNIQUEIDENTIFIER NOT NULL;

				IF NOT EXISTS (SELECT 1 FROM sys.key_constraints WHERE name = 'pk_event_awaiters')
				ALTER TABLE event_awaiters ADD CONSTRAINT pk_event_awaiters PRIMARY KEY (id);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'uq_event_awaiters_registration' AND object_id = OBJECT_ID('event_awaiters'))
				CREATE UNIQUE INDEX uq_event_awaiters_registration ON event_awaiters(workflow_id, event_type, key1, key2, key3);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_event_awaiters_correlate' AND object_id = OBJECT_ID('event_awaiters'))
				CREATE INDEX idx_event_awaiters_correlate ON event_awaiters(tenant_id, event_type, key1, key2, key3);
			`,
			// Lossy on purpose, like Version 5's MySQL reversal: a row
			// inserted post-Up with a key1/2/3 combination that duplicates
			// another awaiter's (workflow_id, event_type) pair -- legitimate
			// now, was impossible under the old composite PK -- makes the
			// composite PRIMARY KEY this restores fail to re-add. Down is
			// for a migration applied and immediately rolled back in
			// development, not for undoing a deployment that has taken
			// traffic under the new shape.
			Down: `
				DROP INDEX IF EXISTS idx_event_awaiters_correlate;
				DROP INDEX IF EXISTS uq_event_awaiters_registration;
				ALTER TABLE event_awaiters DROP CONSTRAINT IF EXISTS event_awaiters_pkey;
				ALTER TABLE event_awaiters ADD CONSTRAINT event_awaiters_pkey PRIMARY KEY (workflow_id, event_type);
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS id;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key3;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key2;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key1;
				DROP INDEX IF EXISTS idx_ingested_events_correlate;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key3;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key2;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key1;
			`,
			DownMySQL: `
				DROP INDEX idx_event_awaiters_correlate ON event_awaiters;
				DROP INDEX uq_event_awaiters_registration ON event_awaiters;
				ALTER TABLE event_awaiters DROP PRIMARY KEY, ADD PRIMARY KEY (workflow_id, event_type);
				ALTER TABLE event_awaiters DROP COLUMN id;
				ALTER TABLE event_awaiters DROP COLUMN key3;
				ALTER TABLE event_awaiters DROP COLUMN key2;
				ALTER TABLE event_awaiters DROP COLUMN key1;
				DROP INDEX idx_ingested_events_correlate ON ingested_events;
				ALTER TABLE ingested_events DROP COLUMN key3;
				ALTER TABLE ingested_events DROP COLUMN key2;
				ALTER TABLE ingested_events DROP COLUMN key1;
			`,
			DownMSSQL: `
				DROP INDEX IF EXISTS idx_event_awaiters_correlate ON event_awaiters;
				DROP INDEX IF EXISTS uq_event_awaiters_registration ON event_awaiters;
				ALTER TABLE event_awaiters DROP CONSTRAINT IF EXISTS pk_event_awaiters;
				ALTER TABLE event_awaiters ADD CONSTRAINT pk_event_awaiters_restored PRIMARY KEY (workflow_id, event_type);
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS id;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key3;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key2;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key1;
				DROP INDEX IF EXISTS idx_ingested_events_correlate ON ingested_events;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key3;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key2;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key1;
			`,
		},
	}
}
