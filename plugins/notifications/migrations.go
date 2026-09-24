package notifications

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for webhook storage and delivery
// tracking. Tables are idempotent (IF NOT EXISTS) and safe to run multiple
// times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS webhook_config (
					tenant_id   UUID NOT NULL,
					id          UUID PRIMARY KEY,
					url         TEXT NOT NULL,
					secret      TEXT NOT NULL DEFAULT '',
					events      JSONB NOT NULL DEFAULT '[]',
					enabled     BOOLEAN NOT NULL DEFAULT true,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS webhook_delivery (
					id               UUID PRIMARY KEY,
					webhook_id       UUID NOT NULL REFERENCES webhook_config(id),
					event_type       TEXT NOT NULL,
					payload          JSONB NOT NULL DEFAULT '{}',
					status           TEXT NOT NULL DEFAULT 'pending',
					attempt_count    INTEGER NOT NULL DEFAULT 0,
					last_attempt_at  TIMESTAMPTZ,
					next_attempt_at  TIMESTAMPTZ,
					delivered_at     TIMESTAMPTZ,
					response_code    INTEGER,
					response_body    TEXT,
					created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_webhook_config_tenant ON webhook_config(tenant_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_delivery_webhook ON webhook_delivery(webhook_id);
				CREATE INDEX IF NOT EXISTS idx_webhook_delivery_status ON webhook_delivery(status, next_attempt_at);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS webhook_config (
					tenant_id   CHAR(36) NOT NULL,
					id          CHAR(36) PRIMARY KEY,
					url         TEXT NOT NULL,
					secret      VARCHAR(900) NOT NULL DEFAULT '',
					events      JSON NOT NULL DEFAULT ('[]'),
					enabled     TINYINT(1) NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE TABLE IF NOT EXISTS webhook_delivery (
					id               CHAR(36) PRIMARY KEY,
					webhook_id       CHAR(36) NOT NULL REFERENCES webhook_config(id),
					event_type       TEXT NOT NULL,
					payload          JSON NOT NULL DEFAULT ('{}'),
					` + "`status`" + `         VARCHAR(255) NOT NULL DEFAULT 'pending',
					attempt_count    INT NOT NULL DEFAULT 0,
					last_attempt_at  TIMESTAMP(6) NULL,
					next_attempt_at  TIMESTAMP(6) NULL,
					delivered_at     TIMESTAMP(6) NULL,
					response_code    INT,
					response_body    TEXT,
					created_at       TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE INDEX idx_webhook_config_tenant ON webhook_config(tenant_id);
				CREATE INDEX idx_webhook_delivery_webhook ON webhook_delivery(webhook_id);
				CREATE INDEX idx_webhook_delivery_status ON webhook_delivery(` + "`status`" + `, next_attempt_at);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_config')
				CREATE TABLE webhook_config (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					id          UNIQUEIDENTIFIER PRIMARY KEY,
					url         NVARCHAR(MAX) NOT NULL,
					secret      NVARCHAR(MAX) NOT NULL DEFAULT '',
					events      NVARCHAR(MAX) NOT NULL DEFAULT ('[]'),
					enabled     BIT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'webhook_delivery')
				CREATE TABLE webhook_delivery (
					id               UNIQUEIDENTIFIER PRIMARY KEY,
					webhook_id       UNIQUEIDENTIFIER NOT NULL REFERENCES webhook_config(id),
					event_type       NVARCHAR(MAX) NOT NULL,
					payload          NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),
					[status]         NVARCHAR(255) NOT NULL DEFAULT 'pending',
					attempt_count    INT NOT NULL DEFAULT 0,
					last_attempt_at  DATETIMEOFFSET,
					next_attempt_at  DATETIMEOFFSET,
					delivered_at     DATETIMEOFFSET,
					response_code    INT,
					response_body    NVARCHAR(MAX),
					created_at       DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_config_tenant' AND object_id = OBJECT_ID('webhook_config'))
				CREATE INDEX idx_webhook_config_tenant ON webhook_config(tenant_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_delivery_webhook' AND object_id = OBJECT_ID('webhook_delivery'))
				CREATE INDEX idx_webhook_delivery_webhook ON webhook_delivery(webhook_id);
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_webhook_delivery_status' AND object_id = OBJECT_ID('webhook_delivery'))
				CREATE INDEX idx_webhook_delivery_status ON webhook_delivery([status], next_attempt_at);
			`,
			Down: `
				DROP TABLE IF EXISTS webhook_delivery;
				DROP TABLE IF EXISTS webhook_config;
			`,
		},
		{
			// Tenant isolation for webhook_config. cleat#1512.
			//
			// A NEW VERSION, NEVER AN EDIT TO v1. A recorded migration never
			// runs again, so editing v1 would protect databases created after
			// this lands and leave every existing one open.
			//
			// Up and Down are empty on purpose. The runtime emits ENABLE /
			// FORCE / the policy from TenantScoped (plugin.applyTenantScoping),
			// using cleat.tenant_row_is_visible so a sweep that named itself
			// through plugin.AcrossAllTenants is admitted and an unmarked one
			// still fails closed. On MySQL and SQL Server this version is
			// recorded and installs nothing, which is what the field means.
			//
			// webhook_delivery IS NOT HERE, AND CANNOT BE. It has no tenant_id
			// column -- it is scoped through webhook_id REFERENCES
			// webhook_config(id) -- and the policy this field emits is
			// cleat.tenant_row_is_visible(tenant_id), so declaring it would
			// fail the migration outright on "column tenant_id does not exist".
			// Its isolation therefore still rests entirely on the joins and
			// predicates in hand-written SQL, which is the thing cleat#1512
			// exists to stop relying on. That is a gap this change does not
			// close and does not widen; it needs a tenant_id column, which is a
			// data migration rather than a policy, and it is filed separately
			// rather than smuggled in here.
			Version:      2,
			TenantScoped: []string{"webhook_config"},
		},
		{
			// Privileges for the cross-tenant delivery loop on
			// webhook_delivery. cleat#1490.
			//
			// A NEW VERSION, NEVER AN EDIT TO v2: a recorded migration never
			// runs again, so editing v2 would grant on databases created after
			// this lands and leave every existing one failing.
			//
			// WHAT BROKE WITHOUT IT. Run() marks itself cross-tenant, which now
			// means SET LOCAL ROLE cleat_sweep, and a role switch changes the
			// privilege set for every table in the transaction, not only the
			// ones carrying a policy. Measured:
			//
			//	pq: permission denied for table webhook_delivery (42501)
			//
			// THIS IS A GRANT, NOT THE SCOPING v2 SAYS IS STILL MISSING.
			// background.go:56 already records that webhook_delivery "has no
			// policy", and v2's comment records that giving it one needs a
			// tenant_id column and a data migration. Neither is done here.
			// This only restores the delivery loop's ability to reach a table
			// it has always written; the scoping gap is unchanged and still
			// filed separately.
			Version:     3,
			SweepTables: []string{"webhook_delivery"},
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
			// This is migrations/mysql/070 applied to notifications, including the
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
				ALTER TABLE webhook_delivery
					MODIFY payload LONGTEXT NOT NULL DEFAULT ('{}');

				ALTER TABLE webhook_delivery
					ADD CONSTRAINT ck_webhook_delivery_payload CHECK (JSON_VALID(payload));
			`,
			// Reversal is MySQL-only because the change is. It restores the
			// JSON type and with it the narrowing -- a value stored intact
			// while this migration was applied is rewritten by the ALTER
			// itself, so this is lossy and is only here because a migration
			// that writes SQL must be reversible.
			DownMySQL: `
				ALTER TABLE webhook_delivery
					DROP CONSTRAINT ck_webhook_delivery_payload;

				ALTER TABLE webhook_delivery
					MODIFY payload JSON NOT NULL DEFAULT ('{}');
			`,
		},
		{
			// webhook_config.secret moves into tenant secrets, sealed under the
			// same envelope encryption every other tenant secret uses. cleat#1992.
			//
			// secret_configured REPLACES it rather than merely accompanying it.
			// This row can no longer answer "does this webhook have a signing
			// secret" by looking at its own secret column -- that value now lives
			// in a different store, envelope-encrypted, unreadable to a WHERE
			// clause -- so the row carries the answer itself instead. Without
			// this, deliver() would have to infer "no secret configured" from a
			// Secrets.Get failure, and it cannot tell that apart from "a secret
			// WAS configured but the lookup broke" -- plugin code cannot import
			// engine, so it has no way to check engine.ErrSecretNotFound
			// specifically (see plugin/secrets.go's Secrets.Get doc comment).
			// deliver() (background.go) honours the marker: unset means sign
			// with an empty key, exactly today's behaviour for a webhook created
			// with no secret (the old column defaulted to '', and Reveal() on
			// that is ""); set means the secret MUST be readable, and any lookup
			// failure fails the delivery attempt outright (an ordinary retry --
			// the same outcome a broken webhook_config lookup already produces)
			// rather than silently downgrading to an empty-key signature nobody
			// configured.
			//
			// NO BACKFILL: 0.3.0 requires a fresh database, with no upgrade path
			// from v0.2.0 (cleat#2058, owner decision 3), so no deployment ever
			// has an existing plaintext secret to move -- ADD then DROP is
			// unconditionally correct, not a shortcut taken because a real
			// migration is hard.
			Version: 5,
			Up: `
				ALTER TABLE webhook_config ADD COLUMN IF NOT EXISTS secret_configured BOOLEAN NOT NULL DEFAULT false;
				ALTER TABLE webhook_config DROP COLUMN IF EXISTS secret;
			`,
			UpMySQL: `
				ALTER TABLE webhook_config ADD COLUMN secret_configured TINYINT(1) NOT NULL DEFAULT 0;
				ALTER TABLE webhook_config DROP COLUMN secret;
			`,
			// v1's secret column carries DEFAULT '', which SQL Server backs with
			// an unnamed default constraint -- unlike pd_config.routing_key and
			// dd_config.api_key (this plugin's #1992 siblings), whose columns
			// were declared with no DEFAULT and so never had one. DROP COLUMN
			// refuses while any object still depends on the column (error
			// 5074), so the constraint has to be found by its parent
			// (object_id, column_id) and dropped by name before the column can
			// go -- the same move plugins/tenantquota/migrations.go uses.
			//
			// NO BEGIN/END, on purpose: plugin.splitStatements shreds a
			// migration's SQL into separate exec calls on every literal ';',
			// with no awareness of T-SQL block structure -- a BEGIN in one
			// fragment and its END in another is two batches, neither valid on
			// its own ("Incorrect syntax near ')'", the unmatched EXEC's
			// close-paren, measured running this as a BEGIN/END block against
			// MSSQL). Each statement below has to be independently complete.
			// A missing 'secret' column makes the DECLARE/SELECT above set
			// @dfname to NULL rather than error, so the constraint-drop step
			// needs no existence guard of its own; only the column DROP does.
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_config') AND name = 'secret_configured')
				ALTER TABLE webhook_config ADD secret_configured BIT NOT NULL DEFAULT 0;

				DECLARE @dfname sysname
				SELECT @dfname = dc.name
					FROM sys.default_constraints dc
					JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id
					WHERE dc.parent_object_id = OBJECT_ID('webhook_config') AND c.name = 'secret'
				IF @dfname IS NOT NULL EXEC('ALTER TABLE webhook_config DROP CONSTRAINT [' + @dfname + ']');

				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_config') AND name = 'secret')
				ALTER TABLE webhook_config DROP COLUMN secret;
			`,
			// Down restores the SCHEMA, not the data -- ordinary for a DROP
			// COLUMN reversal (the value is gone from webhook_config the moment
			// Up runs; it now lives in tenant secrets, a different store). No
			// attempt to recover secret_configured's state into the restored
			// column either: existing rows have nothing to put there.
			Down: `
				ALTER TABLE webhook_config ADD COLUMN IF NOT EXISTS secret TEXT NOT NULL DEFAULT '';
				ALTER TABLE webhook_config DROP COLUMN IF EXISTS secret_configured;
			`,
			DownMySQL: `
				ALTER TABLE webhook_config ADD COLUMN secret VARCHAR(900) NOT NULL DEFAULT '';
				ALTER TABLE webhook_config DROP COLUMN secret_configured;
			`,
			// Mirrors Up's DROP COLUMN secret above -- secret_configured's own
			// DEFAULT 0 gets an unnamed constraint too, and no BEGIN/END for
			// the same splitStatements reason given on UpMSSQL.
			DownMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_config') AND name = 'secret')
				ALTER TABLE webhook_config ADD secret NVARCHAR(MAX) NOT NULL DEFAULT '';

				DECLARE @dfname sysname
				SELECT @dfname = dc.name
					FROM sys.default_constraints dc
					JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id
					WHERE dc.parent_object_id = OBJECT_ID('webhook_config') AND c.name = 'secret_configured'
				IF @dfname IS NOT NULL EXEC('ALTER TABLE webhook_config DROP CONSTRAINT [' + @dfname + ']');

				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_config') AND name = 'secret_configured')
				ALTER TABLE webhook_config DROP COLUMN secret_configured;
			`,
		},
		{
			// webhook_config gets a soft-delete marker. cleat#2220.
			//
			// handleDeleteWebhook (routes.go) used to hard-delete the row.
			// webhook_delivery.webhook_id REFERENCES webhook_config(id) with no
			// ON DELETE action (v1), so deleting a webhook with any delivery
			// history hit a foreign key violation on PostgreSQL and SQL Server
			// outright, and silently orphaned the delivery rows on MySQL, where
			// the same inline REFERENCES clause creates no real constraint at
			// all -- see v7's comment for how that was confirmed. Soft-deleting
			// instead means the config row survives (so nothing it is
			// referenced by ever needs a CASCADE to fire from this path) and a
			// webhook's delivery history is preserved rather than destroyed the
			// moment the webhook itself is removed.
			//
			// A NEW VERSION, NEVER AN EDIT TO v1: a recorded migration never
			// runs again, so editing v1 would add this column only for
			// databases created after this lands.
			Version: 6,
			Up:      `ALTER TABLE webhook_config ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;`,
			// MySQL has no ADD COLUMN IF NOT EXISTS -- the same idempotency
			// hazard and the same PREPARE/EXECUTE/DEALLOCATE guard as
			// migrations/mysql/055_a_run_records_when_a_worker_began_executing_it.sql
			// and plugins/webhookingest/migrations.go's own deleted_at column
			// (v8 there): a crash between this ALTER succeeding and
			// plugin_migrations recording version 6 would otherwise leave a
			// worker that repeats the ALTER on every subsequent boot and fails
			// permanently with ERROR 1060 (42S21) Duplicate column name
			// 'deleted_at'.
			UpMySQL: `
				SET @col := (
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = 'webhook_config' AND column_name = 'deleted_at'
				);
				SET @ddl := IF(@col = 0,
					'ALTER TABLE webhook_config ADD COLUMN deleted_at TIMESTAMP(6) NULL',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_config') AND name = 'deleted_at')
				ALTER TABLE webhook_config ADD deleted_at DATETIMEOFFSET;
			`,
			Down:      `ALTER TABLE webhook_config DROP COLUMN IF EXISTS deleted_at;`,
			DownMySQL: `ALTER TABLE webhook_config DROP COLUMN deleted_at;`,
			DownMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('webhook_config') AND name = 'deleted_at')
				ALTER TABLE webhook_config DROP COLUMN deleted_at;
			`,
		},
		{
			// webhook_delivery.webhook_id gets ON DELETE CASCADE. cleat#2222.
			//
			// admin.drop_tenant hard-deletes webhook_config directly (it is
			// TenantScoped, v2 above) -- webhook_delivery is not and cannot be
			// (v2's own comment: it has no tenant_id column), so nothing ever
			// deletes its rows for a dropped tenant except a cascade from
			// webhook_config. Without ON DELETE CASCADE that hard delete hit
			// the same foreign key shape #2199 fixed for webhookingest:
			//
			//   postgres: update or delete on table "webhook_config" violates
			//     foreign key constraint "webhook_delivery_webhook_id_fkey"
			//     (23503)
			//   mssql: The DELETE statement conflicted with the REFERENCE
			//     constraint (547)
			//
			// Confirmed with a fresh v1 schema and no rows: pg_constraint shows
			// conrelid='webhook_delivery', contype='f',
			// conname='webhook_delivery_webhook_id_fkey' -- Postgres's default
			// <table>_<column>_fkey naming, deterministic from v1's column
			// definition, which names no CONSTRAINT of its own.
			// sys.foreign_keys shows one auto-named constraint on SQL Server
			// (e.g. FK__webhook_d__webho__<hash>) -- NOT deterministic, so the
			// UpMSSQL arm below has to look it up rather than name it.
			// information_schema.table_constraints on MySQL shows NONE at all:
			// v1's inline `webhook_id CHAR(36) NOT NULL REFERENCES
			// webhook_config(id)` is accepted syntax but creates no enforced
			// foreign key on this dialect -- MySQL's inline column-level
			// REFERENCES clause requires an explicit FOREIGN KEY clause to
			// actually be enforced, which is exactly why cleat#2222 observed
			// MySQL silently orphaning rows rather than refusing the delete the
			// way PostgreSQL and SQL Server do. So the MySQL arm below adds a
			// real constraint for the first time; the PostgreSQL and SQL
			// Server arms replace an existing one.
			//
			// This does not change #2220's soft-delete behaviour:
			// handleDeleteWebhook never issues a hard DELETE on webhook_config,
			// so this cascade never fires from that path, and a webhook's
			// delivery history survives its own deletion exactly as v6's
			// comment describes. It fires only from admin.drop_tenant's hard
			// delete (a full tenant purge), which is the case it exists to
			// fix.
			//
			// A NEW VERSION, NEVER AN EDIT TO v1, for the same reason v6 is.
			Version: 7,
			Up: `
				ALTER TABLE webhook_delivery DROP CONSTRAINT IF EXISTS webhook_delivery_webhook_id_fkey;
				ALTER TABLE webhook_delivery ADD CONSTRAINT webhook_delivery_webhook_id_fkey
					FOREIGN KEY (webhook_id) REFERENCES webhook_config(id) ON DELETE CASCADE;
			`,
			// Guarded the same way migrations/mysql/078's tenants_org_id_fk is:
			// MySQL raises ER_DUP_KEYNAME/ER_FK_DUP_NAME on a re-add rather
			// than silently no-op-ing, and there is no ADD CONSTRAINT IF NOT
			// EXISTS to lean on.
			//
			// ADD FOREIGN KEY validates every existing row and refuses the
			// migration if an orphan is already present -- fine here, because
			// 0.3.0 requires a fresh database (#2058 decision 3: no upgrade
			// path from v0.2.0, and #2059 compacts the migration set before
			// the tag), so no database this ever runs against can already
			// hold a pre-v7 orphan from the old hard-delete path.
			UpMySQL: `
				SET @fk := (
					SELECT COUNT(*) FROM information_schema.table_constraints
					WHERE constraint_schema = DATABASE() AND table_name = 'webhook_delivery'
					  AND constraint_name = 'webhook_delivery_webhook_id_fkey'
				);
				SET @ddl := IF(@fk = 0,
					'ALTER TABLE webhook_delivery ADD CONSTRAINT webhook_delivery_webhook_id_fkey FOREIGN KEY (webhook_id) REFERENCES webhook_config(id) ON DELETE CASCADE',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;
			`,
			// The auto-generated constraint name is not deterministic (see the
			// comment above), so it has to be looked up rather than named --
			// the same DECLARE/SELECT/EXEC idiom the default-constraint drops
			// elsewhere in this file and in plugins/webhookingest/migrations.go
			// already use, and for the same reason: NO BEGIN/END, because
			// plugin.splitStatements shreds this migration's SQL into separate
			// exec calls on every literal ';' with no awareness of T-SQL block
			// structure, so a BEGIN in one fragment and its END in another
			// would be two invalid batches.
			//
			// The name filter (`fk.name <> '...cascade'`) makes this
			// idempotent without a separate guard: the first run finds the
			// original auto-named constraint (excluded name does not match it)
			// and drops it; the second run finds nothing (the original is gone
			// and the replacement is excluded by name), so @fkname stays NULL
			// and no drop is attempted. The ADD below is guarded by existence
			// directly.
			UpMSSQL: `
				DECLARE @fkname sysname
				SELECT @fkname = fk.name
					FROM sys.foreign_keys fk
					WHERE fk.parent_object_id = OBJECT_ID('webhook_delivery')
					  AND fk.referenced_object_id = OBJECT_ID('webhook_config')
					  AND fk.name <> 'fk_webhook_delivery_webhook_id_cascade'
				IF @fkname IS NOT NULL EXEC('ALTER TABLE webhook_delivery DROP CONSTRAINT [' + @fkname + ']');

				IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = 'fk_webhook_delivery_webhook_id_cascade')
				ALTER TABLE webhook_delivery
					ADD CONSTRAINT fk_webhook_delivery_webhook_id_cascade
					FOREIGN KEY (webhook_id) REFERENCES webhook_config(id) ON DELETE CASCADE;
			`,
			// Down restores a plain, unnamed-action reference on Postgres
			// (matching v1's own text) and drops MySQL's constraint back to
			// v1's unenforced state. Neither restores the exact original SQL
			// Server auto-generated name -- impossible, since it was never
			// recorded -- but a plain NO ACTION constraint under a fixed name
			// is functionally equivalent to what v1 created, which is the same
			// "restores the schema, not the exact original object identity"
			// reasoning v5's Down above already documents for this file.
			Down: `
				ALTER TABLE webhook_delivery DROP CONSTRAINT IF EXISTS webhook_delivery_webhook_id_fkey;
				ALTER TABLE webhook_delivery ADD CONSTRAINT webhook_delivery_webhook_id_fkey
					FOREIGN KEY (webhook_id) REFERENCES webhook_config(id);
			`,
			DownMySQL: `
				SET @fk := (
					SELECT COUNT(*) FROM information_schema.table_constraints
					WHERE constraint_schema = DATABASE() AND table_name = 'webhook_delivery'
					  AND constraint_name = 'webhook_delivery_webhook_id_fkey'
				);
				SET @ddl := IF(@fk > 0,
					'ALTER TABLE webhook_delivery DROP FOREIGN KEY webhook_delivery_webhook_id_fkey',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;
			`,
			DownMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = 'fk_webhook_delivery_webhook_id_cascade')
				ALTER TABLE webhook_delivery DROP CONSTRAINT fk_webhook_delivery_webhook_id_cascade;

				IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE parent_object_id = OBJECT_ID('webhook_delivery') AND referenced_object_id = OBJECT_ID('webhook_config'))
				ALTER TABLE webhook_delivery
					ADD CONSTRAINT fk_webhook_delivery_webhook_id
					FOREIGN KEY (webhook_id) REFERENCES webhook_config(id);
			`,
		},
	}
}
