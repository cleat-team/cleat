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
			// 128 bytes, not more: see the design doc's §4.3 for the budget
			// this was chosen against. That budget undercounted the tables
			// this repo actually has -- see the registration_key note below,
			// found the hard way in cleat-review on this PR.
			//
			// event_awaiters' primary key moves from (workflow_id,
			// event_type) to a surrogate id. The composite key meant a
			// second `await_event` call for the same type -- in a loop, or
			// awaiting a second order while the first is still pending --
			// upserted over the FIRST registration rather than adding a
			// second one, so only the most recent await could ever be woken.
			// A surrogate key lets two awaits for the same (workflow, type)
			// coexist as long as their key slots differ, which is the whole
			// point of adding key slots in the first place.
			//
			// REGISTRATION_KEY, NOT A FIVE-COLUMN UNIQUE INDEX. The first
			// version of this migration made
			// (workflow_id, event_type, key1, key2, key3) the unique
			// constraint that replaces what the composite PK was enforcing,
			// and it does not fit on two of three dialects: MySQL's
			// utf8mb4 VARCHAR(255) columns cost 1020 bytes each, so
			// workflow_id + event_type + three 128-byte keys is
			// 1020+1020+1536 = 3576 bytes against InnoDB's 3072-byte index
			// key limit. Narrowing workflow_id to VARCHAR(128) gets MySQL to
			// 3068/3072 -- four bytes of margin on a limit that has already
			// bitten this migration once, and SQL Server is worse: even
			// after narrowing event_type below, NVARCHAR is 2 bytes/char, so
			// 255+255+3*128 = 766 characters is 1532 bytes... plus
			// workflow_id's own 255 chars at full width was 510+510+768 =
			// 1788 bytes against SQL Server's 1700-byte NONCLUSTERED index
			// limit -- and SQL Server does not refuse that CREATE INDEX. It
			// warns and lets a later INSERT whose values are wide enough
			// fail instead, which is invisible to any migration test and
			// exactly the "reads cleanest where it measured least" trap
			// CLAUDE.md's whole "Is this result real?" section exists for.
			//
			// registration_key is a CHAR(64) SHA-256 hex digest of the five
			// fields, computed in Go (keys.go's registrationKey) with each
			// field length-prefixed before hashing -- concatenation with a
			// separator would let ("ab","c") and ("a","bc") collide onto the
			// same key, which length-prefixing removes by construction
			// rather than by hoping the separator never appears in a value.
			// 64 ASCII bytes fits every dialect's limit with room to spare,
			// and sidesteps the ceiling permanently instead of buying
			// headroom the next column change would spend.
			//
			// Pre-existing rows get a DB-side SHA-256 hex digest of
			// 'workflow_id:event_type' as their backfilled registration_key,
			// not the raw concatenation this migration originally used.
			// cleat-review caught that version on cleat#2646's first review:
			// workflow_id and event_type are each up to 255 characters, so
			// the raw concatenation is up to 511 bytes against a CHAR(64)
			// column -- "value too long for type character(64)" on Postgres,
			// a strict-mode truncation error on MySQL, string-or-binary-data
			// truncation on MSSQL. A pre-existing awaiter with a realistic
			// workflow_id (a UUID is already 36 characters) and an event
			// type of any length fails the migration outright; only a
			// short-ids test tree hid it. sha256/SHA2/HASHBYTES('SHA2_256',
			// ...) each produce a 64-character lowercase hex digest here,
			// but NOT the same one across dialects for the same logical
			// input, and that is a fact worth stating rather than a gap in
			// this comment: HASHBYTES hashes the bytes of its argument
			// exactly as stored, and workflow_id/event_type are NVARCHAR on
			// MSSQL -- UTF-16LE -- while Postgres and MySQL hash the UTF-8
			// bytes of the equivalent VARCHAR/TEXT value. Measured directly:
			// HASHBYTES('SHA2_256', 'wf-1:order.paid') (a VARCHAR literal)
			// gives bedeb380..., matching Python's
			// hashlib.sha256(b'wf-1:order.paid') exactly, but
			// HASHBYTES('SHA2_256', N'wf-1:order.paid') (NVARCHAR, what the
			// real column concatenation actually produces) gives
			// e7a2b910... -- a different digest, cleat-review caught this
			// on round 3 after an earlier version of this comment claimed
			// the three were byte-identical, confirmed against a VARCHAR
			// literal rather than the NVARCHAR column type this migration
			// actually hashes. It does not matter functionally: nothing
			// compares a backfilled registration_key across dialects, and
			// within one dialect the digest only has to be distinct per
			// input and fit CHAR(64), both of which every dialect's digest
			// does independently.
			//
			// This is still not the SAME digest registrationKey (Go,
			// keys.go) computes for an equivalent fresh row: Go hashes five
			// LENGTH-PREFIXED fields (workflow_id, event_type, key1, key2,
			// key3), not a colon-joined pair. Reproducing that exact framing
			// in SQL -- an 8-byte big-endian length prefix per field, ahead
			// of the field's own bytes, fed into one hash -- is not
			// impossible but is impractical on MSSQL in particular, where
			// NVARCHAR's DATALENGTH is UTF-16 code units, not the UTF-8
			// byte count Go's binary.BigEndian.PutUint64(len(s)) measures.
			// Given P0's read side (queryLatestUnprocessedEvent/awaitEvent)
			// does not yet consult keys at all -- nothing running today can
			// even observe registration_key -- an exact match is not worth
			// that complexity.
			//
			// The gap this leaves: a workflow that registered as an awaiter
			// BEFORE this migration and is later replayed (re-registers via
			// the real Go registerAwaiter path, e.g. after a crash) computes
			// a DIFFERENT registration_key than its own backfilled row, so
			// upsertAwaiter's ON CONFLICT never fires against it -- it
			// INSERTs a second row for the same (workflow_id, event_type)
			// instead of updating the first. This is bounded, not a leak:
			// at most one extra row per legacy awaiter, ever (the second
			// row's key is the real Go hash, so every SUBSEQUENT replay of
			// that same workflow does match it and upserts cleanly), and
			// unregisterAwaiter's DELETE matches on (workflow_id,
			// event_type, key1, key2, key3) -- not registration_key -- so it
			// removes both rows for that awaiter when the workflow completes
			// or is purged. Exercised by
			// TestALegacyAwaiterReplayLeavesAtMostTwoRowsAndUnregisterRemovesBoth,
			// all three dialects.
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
				ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS registration_key CHAR(64) NOT NULL DEFAULT '';
				UPDATE event_awaiters SET registration_key = encode(sha256(convert_to(workflow_id || ':' || event_type, 'UTF8')), 'hex') WHERE registration_key = '';

				ALTER TABLE event_awaiters ADD COLUMN IF NOT EXISTS id UUID DEFAULT gen_random_uuid();
				UPDATE event_awaiters SET id = gen_random_uuid() WHERE id IS NULL;
				ALTER TABLE event_awaiters ALTER COLUMN id SET NOT NULL;
				ALTER TABLE event_awaiters DROP CONSTRAINT IF EXISTS event_awaiters_pkey;
				ALTER TABLE event_awaiters ADD CONSTRAINT event_awaiters_pkey PRIMARY KEY (id);

				CREATE UNIQUE INDEX IF NOT EXISTS uq_event_awaiters_registration ON event_awaiters(registration_key);
				CREATE INDEX IF NOT EXISTS idx_event_awaiters_correlate ON event_awaiters(tenant_id, event_type, key1, key2, key3);
			`,
			// Every statement guarded through information_schema, matching
			// plugins/notifications/migrations.go's v6 and
			// plugins/scheduledbackup/migrations.go: MySQL DDL is not
			// transactional and has no IF NOT EXISTS for ADD COLUMN or
			// CREATE INDEX, so a v6 that fails partway (as this one did, the
			// first time) leaves a database that cannot re-run it -- the
			// surviving statements report "Duplicate column"/"Duplicate key
			// name" on retry. Guarding each one is what makes retry possible
			// rather than merely making a fresh database work.
			UpMySQL: `
				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND column_name = 'key1');
				SET @ddl := IF(@col = 0, 'ALTER TABLE ingested_events ADD COLUMN key1 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT \'\'', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND column_name = 'key2');
				SET @ddl := IF(@col = 0, 'ALTER TABLE ingested_events ADD COLUMN key2 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT \'\'', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND column_name = 'key3');
				SET @ddl := IF(@col = 0, 'ALTER TABLE ingested_events ADD COLUMN key3 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT \'\'', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_correlate');
				SET @ddl := IF(@idx = 0, 'CREATE INDEX idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'event_awaiters' AND column_name = 'key1');
				SET @ddl := IF(@col = 0, 'ALTER TABLE event_awaiters ADD COLUMN key1 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT \'\'', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'event_awaiters' AND column_name = 'key2');
				SET @ddl := IF(@col = 0, 'ALTER TABLE event_awaiters ADD COLUMN key2 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT \'\'', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'event_awaiters' AND column_name = 'key3');
				SET @ddl := IF(@col = 0, 'ALTER TABLE event_awaiters ADD COLUMN key3 VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT \'\'', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'event_awaiters' AND column_name = 'registration_key');
				SET @ddl := IF(@col = 0, 'ALTER TABLE event_awaiters ADD COLUMN registration_key CHAR(64) NOT NULL DEFAULT \'\'', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				UPDATE event_awaiters SET registration_key = SHA2(CONCAT(workflow_id, ':', event_type), 256) WHERE registration_key = '';

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'event_awaiters' AND column_name = 'id');
				SET @ddl := IF(@col = 0, 'ALTER TABLE event_awaiters ADD COLUMN id CHAR(36) NULL', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				UPDATE event_awaiters SET id = UUID() WHERE id IS NULL;
				ALTER TABLE event_awaiters MODIFY id CHAR(36) NOT NULL;
				ALTER TABLE event_awaiters DROP PRIMARY KEY, ADD PRIMARY KEY (id);

				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'event_awaiters' AND index_name = 'uq_event_awaiters_registration');
				SET @ddl := IF(@idx = 0, 'CREATE UNIQUE INDEX uq_event_awaiters_registration ON event_awaiters(registration_key)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'event_awaiters' AND index_name = 'idx_event_awaiters_correlate');
				SET @ddl := IF(@idx = 0, 'CREATE INDEX idx_event_awaiters_correlate ON event_awaiters(tenant_id, event_type, key1, key2, key3)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			// The PK swap looks up the existing auto-named constraint rather
			// than assuming a name, the same shape as
			// plugins/scheduledbackup/migrations.go's foreign-key
			// replacement: find whatever is there under a DIFFERENT name
			// than the one this migration is about to install, drop it if
			// found, then add the new one guarded by existence. That makes
			// this idempotent by construction rather than by an outer
			// "has this version already run" check.
			//
			// ingested_events.event_type is narrowed from NVARCHAR(MAX) to
			// NVARCHAR(255) FIRST, before anything tries to index it: SQL
			// Server refuses a MAX-length column as an index key column
			// outright (error 1919), not merely warns. The narrowing is
			// itself guarded by a data check that THROWs rather than
			// truncating -- a silently truncated event_type would break
			// every future correlation lookup against that row in a way
			// that surfaces much later than the migration that caused it.
			// 255 matches event_subscriptions.event_type's existing bound
			// (v1, NVARCHAR(255)), which every event_type value in this
			// plugin already respects there.
			//
			// TWO INDEPENDENT single-statement IFs below, not one BEGIN/END
			// block -- plugin/migration.go's splitStatements divides UpMSSQL
			// on every semicolon with no awareness of BEGIN/END, quotes, or
			// anything else, and executes each fragment as its own
			// ExecContext call. A THROW followed by an ALTER inside one
			// BEGIN/END puts two statements and the block's own closing
			// syntax across one semicolon boundary, so the naive split cuts
			// it into a dangling BEGIN with no END -- reproduced directly
			// against a live SQL Server: "Incorrect syntax near '1'", where
			// '1' is THROW's own state argument, because the fragment ends
			// at THROW's semicolon with the enclosing IF/BEGIN never closed.
			// Every existing UpMSSQL block in this file already follows the
			// fragment rule (guard, then exactly one statement, repeated)
			// for the same reason; this was the first violation of it, and
			// the first time it was checked against a real server rather
			// than assumed correct because it parses as valid T-SQL in
			// isolation. If execution reaches the second IF, the THROW in
			// the first one did not fire (a fired THROW aborts the whole
			// migration via execSQLStatements' immediate return on error),
			// so the second IF does not need to re-check the row length.
			UpMSSQL: `
				-- Both ingested_events and event_awaiters are TenantScoped
				-- (Version 4), and SQL Server's FORCE security policy
				-- applies to every principal INCLUDING the login running
				-- this migration -- plugin.pluginLockedSession sets no
				-- tenant context for it, so a plain UPDATE against either
				-- table's pre-existing rows silently matches NOTHING: the
				-- filter predicate hides every row from a session with no
				-- 'tenant_id' and no 'cross_tenant' key set. Reproduced
				-- directly: this migration's own id backfill below
				-- (UPDATE event_awaiters SET id = NEWID() WHERE id IS NULL)
				-- left a real pre-existing row's id NULL, which the very
				-- next statement's NOT NULL validation then rejected --
				-- "Cannot insert the value NULL into column 'id' ...
				-- UPDATE fails" (515) -- for ANY pre-existing awaiter, not
				-- only a wide one. The registration_key backfill a few
				-- lines below has the identical exposure and fails
				-- silently rather than loudly: a row RLS hides from that
				-- UPDATE keeps the '' the ADD COLUMN ... DEFAULT '' already
				-- gave it (that part is DDL and touches every row
				-- regardless of RLS), which is wrong but raises nothing.
				--
				-- sp_set_session_context's 'cross_tenant' key is the same
				-- bypass engine/plugindb_tenant.go's markCrossTenantOnTx
				-- uses for the sweep path (plugin/migration.go's
				-- mssqlPluginTenantFilter reads it) -- SESSION-scoped, so
				-- setting it once here covers every statement below. That
				-- session is NOT scoped to this migration, or even to this
				-- plugin: pluginMigrationSession (plugin/migration.go:511-515)
				-- pins ONE connection for the entire RunMigrations call, and
				-- the loop at :531 runs every LATER plugin's migrations on
				-- that same connection before releasing it. Left set, this
				-- bypass would silently carry into every plugin migrated
				-- after event-triggers on whichever boot happens to apply
				-- v6 -- present on that boot, absent on every other one that
				-- finds v6 already applied. Cleared at the end of this arm
				-- (below the SET statement's twin, after the correlate
				-- index) rather than left to rely on go-mssqldb's
				-- ResetSession clearing it on reuse, which only fires
				-- between separate RunMigrations calls, not between
				-- plugins within one.
				EXEC sp_set_session_context @key = N'cross_tenant', @value = N'event-triggers migration 6 backfill, cleat#2625';

				-- Every statement below that REFERENCES key1/key2/key3/
				-- registration_key/id -- as opposed to the IF-guarded
				-- ALTER TABLE ... ADD that creates each one -- is wrapped
				-- in EXEC('...'), and that wrapping is load-bearing, not
				-- decorative. plugin.RunMigrations ("production") sends
				-- this file one statement at a time, so by the time any
				-- later statement runs, the earlier ADD COLUMN has already
				-- committed and the column exists. tests/plugin-harness's
				-- RunPluginMigrations (a SEPARATE, independent runner --
				-- see its own comment on migration.SplitMSSQL) does not:
				-- it splits only on GO, and this migration has none, so
				-- the ENTIRE block above is sent to SQL Server as ONE
				-- batch. Reproduced directly: SQL Server compiles a plain
				-- ad-hoc batch's column references against the catalog as
				-- it stood BEFORE the batch started, and a column added by
				-- a CONDITIONAL ALTER (inside an IF) earlier in that same
				-- batch does not count as existing yet for that pass --
				-- "Msg 207: Invalid column name 'registration_key'" on the
				-- very first plain reference to it, four lines below
				-- where cleat-review's Layer 3 finding pointed. Dynamic
				-- SQL is not compiled until EXEC actually runs it, which
				-- is after every earlier statement in the batch has
				-- already executed and the column genuinely exists --
				-- the same technique the DECLARE @pkname block below
				-- already used for the old PK constraint, applied here to
				-- every other conditionally-created column this migration
				-- touches again.
				IF EXISTS (SELECT 1 FROM sys.columns c WHERE c.object_id = OBJECT_ID('ingested_events') AND c.name = 'event_type' AND c.max_length = -1)
					AND EXISTS (SELECT 1 FROM ingested_events WHERE LEN(event_type) > 255)
				THROW 50001, 'event-triggers migration 6: ingested_events.event_type has a value over 255 characters. Refusing to narrow it to NVARCHAR(255) rather than truncate it. See cleat#2625.', 1;
				IF EXISTS (SELECT 1 FROM sys.columns c WHERE c.object_id = OBJECT_ID('ingested_events') AND c.name = 'event_type' AND c.max_length = -1)
				ALTER TABLE ingested_events ALTER COLUMN event_type NVARCHAR(255) NOT NULL;

				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'key1')
				ALTER TABLE ingested_events ADD key1 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'key2')
				ALTER TABLE ingested_events ADD key2 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'key3')
				ALTER TABLE ingested_events ADD key3 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_correlate' AND object_id = OBJECT_ID('ingested_events'))
				EXEC('CREATE INDEX idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at)');

				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'key1')
				ALTER TABLE event_awaiters ADD key1 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'key2')
				ALTER TABLE event_awaiters ADD key2 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'key3')
				ALTER TABLE event_awaiters ADD key3 NVARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'registration_key')
				ALTER TABLE event_awaiters ADD registration_key CHAR(64) NOT NULL DEFAULT '';
				EXEC('UPDATE event_awaiters SET registration_key = LOWER(CONVERT(CHAR(64), HASHBYTES(''SHA2_256'', workflow_id + '':'' + event_type), 2)) WHERE registration_key = ''''');

				DECLARE @pkname sysname
				SELECT @pkname = kc.name
					FROM sys.key_constraints kc
					WHERE kc.parent_object_id = OBJECT_ID('event_awaiters')
					  AND kc.type = 'PK'
					  AND kc.name <> 'pk_event_awaiters'
				IF @pkname IS NOT NULL EXEC('ALTER TABLE event_awaiters DROP CONSTRAINT [' + @pkname + ']');

				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'id')
				ALTER TABLE event_awaiters ADD id UNIQUEIDENTIFIER DEFAULT NEWID();
				EXEC('UPDATE event_awaiters SET id = NEWID() WHERE id IS NULL');
				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('event_awaiters') AND name = 'id' AND is_nullable = 1)
				EXEC('ALTER TABLE event_awaiters ALTER COLUMN id UNIQUEIDENTIFIER NOT NULL');

				IF NOT EXISTS (SELECT 1 FROM sys.key_constraints WHERE name = 'pk_event_awaiters')
				EXEC('ALTER TABLE event_awaiters ADD CONSTRAINT pk_event_awaiters PRIMARY KEY (id)');

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'uq_event_awaiters_registration' AND object_id = OBJECT_ID('event_awaiters'))
				EXEC('CREATE UNIQUE INDEX uq_event_awaiters_registration ON event_awaiters(registration_key)');
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_event_awaiters_correlate' AND object_id = OBJECT_ID('event_awaiters'))
				EXEC('CREATE INDEX idx_event_awaiters_correlate ON event_awaiters(tenant_id, event_type, key1, key2, key3)');

				-- Twin of the SET above: this session outlives this
				-- migration (see that comment), so the bypass must not.
				-- Cleared unconditionally, not inside an IF -- cheap, and it
				-- means this line does not depend on remembering to update
				-- it if anything above it changes from unconditional to
				-- guarded.
				EXEC sp_set_session_context @key = N'cross_tenant', @value = NULL;
			`,
			// Lossy on purpose, like Version 5's MySQL reversal: a row
			// inserted post-Up with a key1/2/3 combination that duplicates
			// another awaiter's (workflow_id, event_type) pair -- legitimate
			// now, was impossible under the old composite PK -- makes the
			// composite PRIMARY KEY this restores fail to re-add. Down is
			// for a migration applied and immediately rolled back in
			// development, not for undoing a deployment that has taken
			// traffic under the new shape. ingested_events.event_type is
			// left at NVARCHAR(255) on the way down (not widened back to
			// MAX) for the same reason -- reversing a narrowing that has
			// taken writes under the new bound is a separate decision from
			// reversing this migration.
			Down: `
				DROP INDEX IF EXISTS idx_event_awaiters_correlate;
				DROP INDEX IF EXISTS uq_event_awaiters_registration;
				ALTER TABLE event_awaiters DROP CONSTRAINT IF EXISTS event_awaiters_pkey;
				ALTER TABLE event_awaiters ADD CONSTRAINT event_awaiters_pkey PRIMARY KEY (workflow_id, event_type);
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS id;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS registration_key;
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
				ALTER TABLE event_awaiters DROP COLUMN registration_key;
				ALTER TABLE event_awaiters DROP COLUMN key3;
				ALTER TABLE event_awaiters DROP COLUMN key2;
				ALTER TABLE event_awaiters DROP COLUMN key1;
				DROP INDEX idx_ingested_events_correlate ON ingested_events;
				ALTER TABLE ingested_events DROP COLUMN key3;
				ALTER TABLE ingested_events DROP COLUMN key2;
				ALTER TABLE ingested_events DROP COLUMN key1;
			`,
			// SQL Server gives every inline "DEFAULT ..." its own auto-named
			// constraint object (DF__event_awa__id__<hex>), and ALTER TABLE
			// ... DROP COLUMN refuses while one is attached: "The object
			// 'DF__...' is dependent on column 'id'." (Msg 5074/4922,
			// reproduced directly against a live SQL Server). id
			// (DEFAULT NEWID()), registration_key, key1, key2 and key3
			// (DEFAULT '') on event_awaiters, and key1/key2/key3 on
			// ingested_events, all carry one. The name is generated, not
			// something this migration chose, so it has to be looked up
			// per table -- one dynamic block per table, run before any
			// DROP COLUMN on that table, rather than one per column: it
			// finds every default constraint on the columns this Down
			// removes and drops them all in one EXEC, using the same
			// DECLARE/SELECT/IF-EXEC shape (no semicolon until the closing
			// one) UpMSSQL already relies on above to keep the naive
			// splitter in plugin/migration.go from treating DECLARE,
			// SELECT and IF as separate statements and losing the
			// variable between them.
			//
			// The concatenated string is built with CHAR(59), not a literal
			// ';', for the same reason -- plugin/migration.go's
			// splitStatements has zero quote-awareness (this repo's own
			// documented hazard: THROW's embedded semicolon hit the same
			// wall above). A literal ';' inside this string literal would
			// be cut by the OUTER splitter before the statement ever
			// reaches SQL Server, same as the BEGIN/END fragmentation this
			// migration already had to route around once. CHAR(59) puts a
			// real semicolon in the dynamic SQL EXEC() runs without one
			// ever appearing in the migration's raw text.
			DownMSSQL: `
				DROP INDEX IF EXISTS idx_event_awaiters_correlate ON event_awaiters;
				DROP INDEX IF EXISTS uq_event_awaiters_registration ON event_awaiters;
				ALTER TABLE event_awaiters DROP CONSTRAINT IF EXISTS pk_event_awaiters;
				ALTER TABLE event_awaiters ADD CONSTRAINT pk_event_awaiters_restored PRIMARY KEY (workflow_id, event_type);

				DECLARE @eaDefaults NVARCHAR(MAX) = ''
				SELECT @eaDefaults = @eaDefaults + 'ALTER TABLE event_awaiters DROP CONSTRAINT [' + dc.name + ']' + CHAR(59) + ' '
					FROM sys.default_constraints dc
					JOIN sys.columns c ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
					WHERE dc.parent_object_id = OBJECT_ID('event_awaiters')
					  AND c.name IN ('id', 'registration_key', 'key1', 'key2', 'key3')
				IF @eaDefaults <> '' EXEC(@eaDefaults);

				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS id;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS registration_key;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key3;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key2;
				ALTER TABLE event_awaiters DROP COLUMN IF EXISTS key1;

				DROP INDEX IF EXISTS idx_ingested_events_correlate ON ingested_events;

				DECLARE @ieDefaults NVARCHAR(MAX) = ''
				SELECT @ieDefaults = @ieDefaults + 'ALTER TABLE ingested_events DROP CONSTRAINT [' + dc.name + ']' + CHAR(59) + ' '
					FROM sys.default_constraints dc
					JOIN sys.columns c ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
					WHERE dc.parent_object_id = OBJECT_ID('ingested_events')
					  AND c.name IN ('key1', 'key2', 'key3')
				IF @ieDefaults <> '' EXEC(@ieDefaults);

				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key3;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key2;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS key1;
			`,
		},
		{
			// cleat#2669, found by cleat-review reviewing cleat#2668's
			// MySQL lock-order fix. That fix forced the claim query onto
			// idx_ingested_events_correlate (tenant_id, event_type, key1,
			// key2, key3, received_at) -- correct for the locking defect it
			// fixed, but that index has NO processed column, so it cannot
			// exclude rows the claim's "AND NOT processed" predicate will
			// reject. The claim walks every PROCESSED row for its key tuple,
			// in received_at order, before reaching the first unprocessed
			// one -- and since nothing deletes from ingested_events, that
			// walk is the tenant's entire history of that event type, for
			// an unkeyed await (today's only caller): three tier-1 slots
			// have a use once queries beyond await_event start filling them.
			//
			// MEASURED, not assumed, on real Postgres 16, SQL Server 2022,
			// and MySQL 8.4 containers: seeded 20,000 processed rows for
			// one (tenant, type, key) ahead of one unprocessed target row,
			// then timed the claim query exactly as written before this
			// migration. Postgres's own EXPLAIN (ANALYZE, BUFFERS) named
			// the cost directly -- "Rows Removed by Filter: 20000" -- at
			// 2.5ms; SQL Server took 193ms for the identical shape, with
			// no EXPLAIN needed to show why; MySQL, FORCE INDEXed onto the
			// old idx_ingested_events_correlate exactly as queries.go had
			// it, took 28.4ms. Adding a CANDIDATE index of this
			// migration's exact shape, with no other query change, cut
			// Postgres to 0.46ms (5.5x), SQL Server to 5.5ms (35x), and
			// MySQL to 0.33ms (85x) -- Postgres and SQL Server's
			// optimizers picked the new index on their own, no
			// FORCE/hint needed on either.
			//
			// So this is not a MySQL-only cost, and MySQL's old shape was
			// not only a performance cost either -- it was ALSO a
			// correctness bug, but a narrower one than #2668's, and the
			// two do not overlap. #2668 fixed CROSS-key locking: a claim
			// for key A no longer takes a next-key lock on an older row of
			// a DIFFERENT key B that the scan passes on its way to A.
			// idx_ingested_events_correlate has no processed column, so
			// even with #2668's fix a claim for key A still walked, and
			// under MySQL's default REPEATABLE READ locked, every
			// PROCESSED row of key A's own history before reaching the
			// first unprocessed one -- SAME-key locking, over a claim's
			// own history rather than a different awaiter's key. That is
			// this migration's MySQL half, confirmed by EXPLAIN ANALYZE
			// against the new index: "Index lookup ... processed=0 ...
			// actual rows=1" -- a real index seek on the exact row wanted,
			// not a scan that locks its way past the rest.
			//
			// idx_ingested_events_correlate is DROPPED, not kept alongside
			// the replacement: grepping every query in this package that
			// touches ingested_events, the claim query
			// (queryOldestUnprocessedEventForClaim) is the ONLY one that
			// ever referenced it, by name, via FORCE INDEX on MySQL --
			// queryUnprocessedEvents (the background dispatcher) has no
			// tenant/type/key predicate at all and needs
			// idx_ingested_events_unprocessed instead, which this migration
			// does not touch. A v7 index carrying every column the old one
			// had, in the same order, plus `processed` ahead of
			// `received_at`, strictly dominates it for that one consumer --
			// keeping both would be a second index nothing reads, paying
			// write cost for no query.
			//
			// PARTIAL ON POSTGRES, FILTERED ON MSSQL, FULL ON MYSQL --
			// three different shapes because the dialects differ in what
			// they can express, not because the query differs. Postgres
			// and SQL Server both support an index that excludes processed
			// rows BY CONSTRUCTION (`WHERE NOT processed` / `WHERE
			// processed = 0`), which is what the measurement above credits
			// for the 5.5x/35x gains -- a processed row is never IN the
			// index at all, not merely filtered out of it after a match.
			// MySQL has no partial or filtered index support of any kind,
			// so `processed` has to be a real column in the key, placed
			// BEFORE received_at so an exact-equality claim (tenant_id,
			// event_type, key1, key2, key3, processed) still gets
			// received_at as pure index order within that match, the same
			// property the old correlate index had for the key columns
			// alone. FORCE INDEX stays on the MySQL arm only, retargeted to
			// this index's name -- Postgres and SQL Server are left
			// unforced, because the measurement above already showed both
			// optimizers choose the new index on their own.
			Version: 7,
			Up: `
				DROP INDEX IF EXISTS idx_ingested_events_correlate;
				CREATE INDEX IF NOT EXISTS idx_ingested_events_claim ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at) WHERE NOT processed;
			`,
			// Guarded through information_schema, the same idempotency
			// discipline Version 6's MySQL arm established for this table:
			// MySQL DDL is not transactional, so a migration that failed
			// partway must be safe to re-run rather than answering
			// "Duplicate key name" on retry.
			UpMySQL: `
				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_correlate');
				SET @ddl := IF(@idx > 0, 'DROP INDEX idx_ingested_events_correlate ON ingested_events', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_claim');
				SET @ddl := IF(@idx = 0, 'CREATE INDEX idx_ingested_events_claim ON ingested_events(tenant_id, event_type, key1, key2, key3, processed, received_at)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			UpMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_correlate' AND object_id = OBJECT_ID('ingested_events'))
				DROP INDEX idx_ingested_events_correlate ON ingested_events;
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_claim' AND object_id = OBJECT_ID('ingested_events'))
				CREATE INDEX idx_ingested_events_claim ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at) WHERE processed = 0;
			`,
			// Reverses to the Version 6 shape, not to "no index at all" --
			// dropping idx_ingested_events_claim and leaving nothing would
			// reopen cleat#2668's lock-order defect on a database that
			// still runs code expecting the claim query's FORCE INDEX
			// target to exist. As with every other Down in this file, this
			// is for a migration applied and immediately rolled back in
			// development, not for undoing a deployment that has taken
			// claim traffic under the new index.
			Down: `
				DROP INDEX IF EXISTS idx_ingested_events_claim;
				CREATE INDEX IF NOT EXISTS idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at);
			`,
			DownMySQL: `
				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_claim');
				SET @ddl := IF(@idx > 0, 'DROP INDEX idx_ingested_events_claim ON ingested_events', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_correlate');
				SET @ddl := IF(@idx = 0, 'CREATE INDEX idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			DownMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_claim' AND object_id = OBJECT_ID('ingested_events'))
				DROP INDEX idx_ingested_events_claim ON ingested_events;
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_correlate' AND object_id = OBJECT_ID('ingested_events'))
				CREATE INDEX idx_ingested_events_correlate ON ingested_events(tenant_id, event_type, key1, key2, key3, received_at);
			`,
		},
		{
			// cleat#2663: `processed` was shared by two writers with no model
			// of each other. tryClaim (claim.go) sets it when an awaiter's
			// backstop scan (queryOldestUnprocessedEventForClaim) consumes a
			// row; retryEvent/markRetryFailed (background.go) set it on every
			// terminal subscription-dispatch outcome, for reasons that have
			// nothing to do with whether an awaiter still wants that row.
			// Whichever writer got there first retired the other's interest:
			// a dispatched-and-completed event became permanently invisible
			// to the awaiter backstop scan (a lost wakeup, since PublishEvent
			// itself sets `processed` on NEITHER path -- every event sits at
			// `processed = false` until processBatch's 30s sweep reaches it,
			// so this did not need the T1/T2 registration race to trigger,
			// only an event type with no subscriptions or a >10s-old event),
			// and the mirror direction: an awaiter's claim permanently
			// starved a failed dispatch of its retry.
			//
			// The fix is two independent columns for two independent
			// questions, not a merge of the two writers' rules. `processed`
			// stays exclusively the awaiter-claim path's (claim.go,
			// queryOldestUnprocessedEventForClaim, idx_ingested_events_claim
			// -- NONE of which change in this migration). This adds
			// `dispatch_processed`, which becomes the subscription-dispatch
			// path's own flag: background.go's writers and
			// queryUnprocessedEvents's read move onto it in the same PR that
			// adds this migration.
			//
			// idx_ingested_events_unprocessed (Version 1, `(processed,
			// received_at)`) is KEPT, not dropped, though nothing names it by
			// hint or FORCE INDEX any more since Version 7 moved the claim
			// query onto idx_ingested_events_claim. Measured directly rather
			// than assumed: dropping it reintroduces a real bug in
			// queryOldestUnprocessedEventForClaim (claim.go) on SQL Server --
			// TestAwaitEventConcurrentClaimsSkipTheLockedRow/mssql fails
			// deterministically (10/10) with it dropped and passes
			// deterministically (10/10) with it present, both on a genuinely
			// fresh database each run, everything else held constant. The
			// claim query's own plan is a Clustered Index Scan either way
			// (confirmed via SHOWPLAN_TEXT, identical operator on both), so
			// this is not the seek-vs-scan mechanism it would be tempting to
			// assume -- the index's mere PRESENCE changes the query
			// optimizer's behaviour for a statement that does not use it by
			// name, and removing it lets a concurrent claim (UPDLOCK, READPAST,
			// ROWLOCK) return zero rows while an unlocked, matching row still
			// exists -- a claim-starvation bug, not merely a missed
			// optimization. Isolated by adding ONLY this column to a clean
			// develop checkout (harmless) and then ONLY dropping this index on
			// that same checkout (reproduces the failure) -- see cleat#2821,
			// filed rather than chased further here because the mechanism
			// inside SQL Server's optimizer is not this issue's question.
			//
			// SUPERSEDED 2026-09-30 (cleat#2821/#2866, PR #2869), in three
			// parts -- a bare "superseded" invites re-deriving the wrong
			// conclusion from each one separately, so all three are stated
			// together:
			//
			// 1. The REASON above no longer holds. The paragraph describes
			// the MSSQL arm of queryOldestUnprocessedEventForClaim, which no
			// longer exists. That bug was real but narrower than first
			// measured -- at realistic multi-tenant scale the starvation
			// reproduced WITH this index present too (it only avoided the
			// symptom in a near-empty table) -- and the fix was a
			// query-shape change, not a bigger reliance on this index: the
			// MSSQL claim path now reads an unlocked candidate list
			// (READPAST, no UPDLOCK) and claims by primary key, which cannot
			// exhibit either the optimizer-dependent behaviour this
			// paragraph describes or the starvation it was worked around
			// for.
			//
			// 2. The index is now DROPPABLE, but deliberately DEFERRED, per
			// the owner (2026-09-30, relayed on PR #2869): "if we're not
			// using that any more we can drop it, but wait to do that when
			// we can combine it with some other migration" -- i.e. not as a
			// standalone drop-only migration. cleat#2870 is the follow-up
			// that keeps this from being forgotten the next time someone is
			// in this file for an unrelated reason.
			//
			// 3. Until then it is kept by the OWNER'S RULING, not because any
			// current query needs it. Checked rather than assumed:
			// queryUnprocessedEvents (this file's sibling query) reads
			// dispatch_processed, served by its own idx_ingested_events_dispatch,
			// not this index. The only remaining readers of bare `processed`
			// are the claim queries (queryOldestUnprocessedEventForClaim,
			// queryCandidateUnprocessedEventIDsMSSQL, queryClaimEventByIDMSSQL),
			// and all three lead with tenant_id/event_type/key1/key2/key3,
			// which idx_ingested_events_claim (Version 7) covers -- not this
			// index. So nothing depends on idx_ingested_events_unprocessed
			// for correctness OR performance today; it survives purely
			// because cleat#2870 defers its removal to ride with a future
			// migration, per the owner.
			//
			// dispatch_processed gets its OWN new index,
			// idx_ingested_events_dispatch, same per-dialect partial/plain
			// shape Version 1 established for the one this keeps.
			//
			// THE BACKFILL BELOW IS NOT OPTIONAL, and its absence was a real
			// defect caught by cleat-review before this shipped (#2822 round
			// 1), measured on PG16 with the real v1-v7 Postgres migrations
			// applied to four seeded aged rows (completed, dead_letter,
			// consumed, pending): develop's predicate selects 1 (pending);
			// this migration's Up, run with no backfill, selected 4 --
			// dispatch_processed defaults to false for every row that
			// existed before the upgrade, and the dropped `status` predicate
			// (see queryUnprocessedEvents's own comment on why it is
			// redundant GOING FORWARD) was the only thing that had ever
			// excluded them. Nothing deletes from ingested_events, so an
			// upgrade with no backfill would re-dispatch a deployment's
			// entire event history, restarting every workflow it ever
			// triggered. Fresh-database tests cannot see this: every row in
			// a fresh database gets dispatch_processed correctly from birth.
			// The backfill preserves exactly what develop's own sweep would
			// have selected -- a row is already dispatch-settled if the
			// awaiter-claim path already consumed it (processed) or the
			// dispatch path already reached a terminal, non-pending status.
			// Rescuing rows the OLD flag starved (this issue's whole point,
			// going forward) is deliberately not backdated here -- that is a
			// data-migration decision for whoever wants historical recovery,
			// not a side effect of adding the column.
			Version: 8,
			Up: `
				ALTER TABLE ingested_events ADD COLUMN IF NOT EXISTS dispatch_processed BOOLEAN NOT NULL DEFAULT false;
				UPDATE ingested_events SET dispatch_processed = true
					WHERE processed OR (status IS NOT NULL AND status <> 'pending');
				CREATE INDEX IF NOT EXISTS idx_ingested_events_dispatch ON ingested_events(dispatch_processed, received_at) WHERE NOT dispatch_processed;
			`,
			// Idempotency via information_schema, matching Version 6/7's own
			// discipline: MySQL DDL is not transactional, so a migration that
			// failed partway must be safe to re-run. The backfill UPDATE
			// needs no such guard -- it is naturally idempotent, re-setting
			// an already-true row to true.
			UpMySQL: `
				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND column_name = 'dispatch_processed');
				SET @ddl := IF(@col = 0, 'ALTER TABLE ingested_events ADD COLUMN dispatch_processed TINYINT(1) NOT NULL DEFAULT 0', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				UPDATE ingested_events SET dispatch_processed = 1
					WHERE processed = 1 OR (status IS NOT NULL AND status <> 'pending');

				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_dispatch');
				SET @ddl := IF(@idx = 0, 'CREATE INDEX idx_ingested_events_dispatch ON ingested_events(dispatch_processed, received_at)', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			UpMSSQL: `
				-- ingested_events is TenantScoped (Version 4), and SQL
				-- Server's security policy applies to every principal
				-- INCLUDING the login running this migration -- the same
				-- exposure Version 6's own backfill documents at length
				-- above. A plain UPDATE with no tenant_id predicate matches
				-- NOTHING under a session with no 'tenant_id' and no
				-- 'cross_tenant' key set: the filter predicate hides every
				-- row first. Reproduced directly while fixing cleat-review's
				-- round-1 GAP on cleat#2822 -- the backfill below silently
				-- affected 0 rows without this bypass, which is worse than
				-- an error: the migration reports success and the re-dispatch
				-- hazard R1 exists to prevent survives on this dialect alone.
				-- Same session-scoped key Version 6 uses, cleared the same way.
				EXEC sp_set_session_context @key = N'cross_tenant', @value = N'event-triggers migration 8 backfill, cleat#2822';

				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'dispatch_processed')
				ALTER TABLE ingested_events ADD dispatch_processed BIT NOT NULL DEFAULT 0;

				-- EXEC(...), not a plain statement: dispatch_processed may
				-- have just been added by the conditional ALTER above, in
				-- the SAME batch, on the runner (tests/plugin-harness) that
				-- does not split on statement boundaries -- Version 6's own
				-- comment on this exact hazard explains why a plain
				-- reference compiles against the catalog as it stood before
				-- the batch started and fails with "Invalid column name".
				EXEC('UPDATE ingested_events SET dispatch_processed = 1 WHERE processed = 1 OR (status IS NOT NULL AND status <> ''pending'')');

				-- EXEC(...) for the same reason as the UPDATE above: this
				-- CREATE INDEX references dispatch_processed too, in the same
				-- batch as the conditional ALTER -- a plain statement here
				-- compiles against the pre-batch catalog on a runner that
				-- does not split statements and fails "Invalid column name
				-- 'dispatch_processed'". Caught by tests/plugin-harness'
				-- Multi-DB CI job, not by this package's own test suite,
				-- which runs each statement separately through
				-- plugin.RunMigrations -- exactly the gap the UPDATE's own
				-- comment above already named and this statement missed.
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_dispatch' AND object_id = OBJECT_ID('ingested_events'))
				EXEC('CREATE INDEX idx_ingested_events_dispatch ON ingested_events(dispatch_processed, received_at) WHERE dispatch_processed = 0');

				-- Twin of the SET above: cleared unconditionally, matching
				-- Version 6's own reasoning -- this session outlives this
				-- migration and the bypass must not.
				EXEC sp_set_session_context @key = N'cross_tenant', @value = NULL;
			`,
			// Reverses to the Version 7 shape -- idx_ingested_events_unprocessed
			// was never touched by Up, so Down does not recreate it. This is for
			// a migration applied and immediately rolled back in development,
			// matching every other Down in this file -- not for undoing a
			// deployment that has taken dispatch-sweep traffic under the new
			// column.
			Down: `
				DROP INDEX IF EXISTS idx_ingested_events_dispatch;
				ALTER TABLE ingested_events DROP COLUMN IF EXISTS dispatch_processed;
			`,
			DownMySQL: `
				SET @idx := (SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND index_name = 'idx_ingested_events_dispatch');
				SET @ddl := IF(@idx > 0, 'DROP INDEX idx_ingested_events_dispatch ON ingested_events', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;

				SET @col := (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'ingested_events' AND column_name = 'dispatch_processed');
				SET @ddl := IF(@col > 0, 'ALTER TABLE ingested_events DROP COLUMN dispatch_processed', 'DO 0');
				PREPARE stmt FROM @ddl; EXECUTE stmt; DEALLOCATE PREPARE stmt;
			`,
			DownMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_ingested_events_dispatch' AND object_id = OBJECT_ID('ingested_events'))
				DROP INDEX idx_ingested_events_dispatch ON ingested_events;

				-- dispatch_processed carries a DEFAULT on this dialect (Up's
				-- "BIT NOT NULL DEFAULT 0"), backed by an unnamed default
				-- constraint -- the same obstacle every other MSSQL column
				-- drop in this file hits, and the same fix: find it by
				-- (table, column) in sys.default_constraints and drop it by
				-- name first. Tier 1 Gate caught this uncaught: DROP COLUMN
				-- failed with "The object 'DF__ingested___dispa__...' is
				-- dependent on column 'dispatch_processed' (5074)", and
				-- because RunDownMigrations had already dropped the index
				-- and recorded nothing rolled back, the subsequent recovery
				-- Up left the schema short one index versus a clean install
				-- -- this pair was never actually safely recoverable.
				DECLARE @ieDefaults NVARCHAR(MAX) = ''
				SELECT @ieDefaults = @ieDefaults + 'ALTER TABLE ingested_events DROP CONSTRAINT [' + dc.name + ']' + CHAR(59) + ' '
					FROM sys.default_constraints dc
					JOIN sys.columns c ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
					WHERE dc.parent_object_id = OBJECT_ID('ingested_events')
					  AND c.name = 'dispatch_processed'
				IF @ieDefaults <> '' EXEC(@ieDefaults);

				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('ingested_events') AND name = 'dispatch_processed')
				ALTER TABLE ingested_events DROP COLUMN dispatch_processed;
			`,
		},
	}
}
