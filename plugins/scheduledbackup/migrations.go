package scheduledbackup

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for backup configuration and history.
// Tables are idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS backup_config (
					tenant_id      UUID NOT NULL,
					id             UUID PRIMARY KEY,
					name           TEXT,
					cron           TEXT,
					s3_bucket      TEXT,
					s3_prefix      TEXT,
					retention_days INTEGER DEFAULT 30,
					enabled        BOOLEAN DEFAULT true,
					last_run_at    TIMESTAMPTZ,
					next_run_at    TIMESTAMPTZ,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE TABLE IF NOT EXISTS backup_history (
					id             UUID PRIMARY KEY,
					config_id      UUID REFERENCES backup_config(id),
					tenant_id      UUID NOT NULL,
					filename       TEXT,
					size_bytes     BIGINT,
					status         TEXT,
					started_at     TIMESTAMPTZ,
					completed_at   TIMESTAMPTZ,
					error_message  TEXT,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_backup_config_tenant_enabled_next
					ON backup_config (tenant_id, enabled, next_run_at);

				CREATE INDEX IF NOT EXISTS idx_backup_history_tenant_config
					ON backup_history (tenant_id, config_id);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS backup_config (
					tenant_id      CHAR(36) NOT NULL,
					id             CHAR(36) PRIMARY KEY,
					` + "`name`" + `           TEXT,
					cron           TEXT,
					s3_bucket      TEXT,
					s3_prefix      TEXT,
					retention_days INT DEFAULT 30,
					enabled        TINYINT(1) DEFAULT 1,
					last_run_at    TIMESTAMP(6),
					next_run_at    TIMESTAMP(6),
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE TABLE IF NOT EXISTS backup_history (
					id             CHAR(36) PRIMARY KEY,
					config_id      CHAR(36) REFERENCES backup_config(id),
					tenant_id      CHAR(36) NOT NULL,
					filename       TEXT,
					size_bytes     BIGINT,
					` + "`status`" + `       TEXT,
					started_at     TIMESTAMP(6),
					completed_at   TIMESTAMP(6),
					error_message  TEXT,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);

				CREATE INDEX idx_backup_config_tenant_enabled_next
					ON backup_config (tenant_id, enabled, next_run_at);

				CREATE INDEX idx_backup_history_tenant_config
					ON backup_history (tenant_id, config_id);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'backup_config')
				CREATE TABLE backup_config (
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					id             UNIQUEIDENTIFIER PRIMARY KEY,
					[name]         NVARCHAR(MAX),
					cron           NVARCHAR(MAX),
					s3_bucket      NVARCHAR(MAX),
					s3_prefix      NVARCHAR(MAX),
					retention_days INT DEFAULT 30,
					enabled        BIT DEFAULT 1,
					last_run_at    DATETIMEOFFSET,
					next_run_at    DATETIMEOFFSET,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'backup_history')
				CREATE TABLE backup_history (
					id             UNIQUEIDENTIFIER PRIMARY KEY,
					config_id      UNIQUEIDENTIFIER REFERENCES backup_config(id),
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					filename       NVARCHAR(MAX),
					size_bytes     BIGINT,
					[status]       NVARCHAR(MAX),
					started_at     DATETIMEOFFSET,
					completed_at   DATETIMEOFFSET,
					error_message  NVARCHAR(MAX),
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_backup_config_tenant_enabled_next' AND object_id = OBJECT_ID('backup_config'))
				CREATE INDEX idx_backup_config_tenant_enabled_next
					ON backup_config (tenant_id, enabled, next_run_at);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_backup_history_tenant_config' AND object_id = OBJECT_ID('backup_history'))
				CREATE INDEX idx_backup_history_tenant_config
					ON backup_history (tenant_id, config_id);
			`,
			Down: `
				DROP INDEX IF EXISTS idx_backup_history_tenant_config;
				DROP INDEX IF EXISTS idx_backup_config_tenant_enabled_next;
				DROP TABLE IF EXISTS backup_history;
				DROP TABLE IF EXISTS backup_config;
			`,
		},
		{
			// Tenant isolation for both backup tables. cleat#1512.
			//
			// A new version rather than TenantScoped on v1: v1 is recorded
			// everywhere this plugin runs and a recorded migration never runs
			// again, so editing it would protect new databases and leave every
			// existing one open.
			//
			// Both tables: backup_history rows are addressed BY ID on the
			// detached async path, and scoping the config while leaving the
			// history open would protect the lighter of the two.
			//
			// The CLI is converted in this same change. It opens its own
			// *sql.DB and would break the moment these policies exist -- which
			// is what shipping the two separately did to jobqueue in #1511.
			Version:      2,
			TenantScoped: []string{"backup_config", "backup_history"},
		},
		{
			// backup_history.config_id gets ON DELETE CASCADE. cleat#2234.
			//
			// admin.drop_tenant sweeps plugin tables in NAME order on
			// PostgreSQL and SQL Server -- backup_config sorts before
			// backup_history, so the parent is deleted first.
			// backup_history.config_id REFERENCES backup_config(id) (v1,
			// nullable) carries no ON DELETE, so dropping a tenant with backup
			// history failed outright:
			//
			//   postgres: update or delete on table "backup_config" violates
			//     foreign key constraint "backup_history_config_id_fkey" on
			//     table "backup_history" (23503)
			//   mssql: The DELETE statement conflicted with the REFERENCE
			//     constraint "FK__backup_hi__confi__<hash>" ... table
			//     "dbo.backup_history", column 'config_id' (547)
			//
			// Confirmed by cleat-review with a victim tenant carrying one
			// backup_config row and one backup_history row referencing it:
			// the whole drop rolled back on both dialects, leaving
			// admin.tenants, backup_config and backup_history all at 1/1/1
			// for the victim. A bystander tenant with a config but no history
			// dropped cleanly on both -- the FK is only reached when there is
			// a child row to violate it.
			//
			// Same shape as cleat#2222/#2233's webhook_delivery.webhook_id
			// fix: Postgres's default <table>_<column>_fkey naming is
			// deterministic from v1's unnamed column REFERENCES, so the Up
			// arm can name and drop it directly; SQL Server's auto-generated
			// name is not, so the UpMSSQL arm looks it up. MySQL's inline
			// `config_id CHAR(36) REFERENCES backup_config(id)` is accepted
			// syntax but creates no enforced constraint at all on this
			// dialect -- confirmed directly against a fresh MySQL table
			// (information_schema.table_constraints shows only the PRIMARY
			// KEY, the same absence #2233's v7 comment found for
			// webhook_delivery), so the MySQL arm adds a real constraint for
			// the first time rather than replacing one.
			//
			// A NEW VERSION, NEVER AN EDIT TO v1: a recorded migration never
			// runs again, so editing v1 would only fix databases created
			// after this lands.
			Version: 3,
			Up: `
				ALTER TABLE backup_history DROP CONSTRAINT IF EXISTS backup_history_config_id_fkey;
				ALTER TABLE backup_history ADD CONSTRAINT backup_history_config_id_fkey
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE CASCADE;
			`,
			// Guarded the same way plugins/notifications/migrations.go's v7
			// UpMySQL is: MySQL raises ER_DUP_KEYNAME/ER_FK_DUP_NAME on a
			// re-add rather than silently no-op-ing, and there is no ADD
			// CONSTRAINT IF NOT EXISTS to lean on.
			//
			// ADD FOREIGN KEY validates every existing row and refuses the
			// migration if an orphan config_id is already present -- fine
			// here, because 0.3.0 requires a fresh database (#2058 decision
			// 3: no upgrade path from v0.2.0, and #2059 compacts the
			// migration set before the tag), the same reasoning cleat#2233
			// applied to webhook_delivery's identical fix.
			UpMySQL: `
				SET @fk := (
					SELECT COUNT(*) FROM information_schema.table_constraints
					WHERE constraint_schema = DATABASE() AND table_name = 'backup_history'
					  AND constraint_name = 'backup_history_config_id_fkey'
				);
				SET @ddl := IF(@fk = 0,
					'ALTER TABLE backup_history ADD CONSTRAINT backup_history_config_id_fkey FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE CASCADE',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;
			`,
			// The auto-generated constraint name is not deterministic, so it
			// has to be looked up rather than named -- the same idiom
			// plugins/notifications/migrations.go's v7 UpMSSQL uses for the
			// identical shape. NO BEGIN/END: plugin.splitStatements shreds
			// this migration's SQL into separate exec calls on every literal
			// ';' with no awareness of T-SQL block structure, so a BEGIN in
			// one fragment and its END in another would be two invalid
			// batches.
			//
			// The name filter (fk.name <> '...cascade') makes this
			// idempotent without a separate guard: the first run finds the
			// original auto-named constraint (excluded name does not match
			// it) and drops it; the second run finds nothing (the original
			// is gone and the replacement is excluded by name), so @fkname
			// stays NULL and no drop is attempted. The ADD below is guarded
			// by existence directly.
			UpMSSQL: `
				DECLARE @fkname sysname
				SELECT @fkname = fk.name
					FROM sys.foreign_keys fk
					WHERE fk.parent_object_id = OBJECT_ID('backup_history')
					  AND fk.referenced_object_id = OBJECT_ID('backup_config')
					  AND fk.name <> 'fk_backup_history_config_id_cascade'
				IF @fkname IS NOT NULL EXEC('ALTER TABLE backup_history DROP CONSTRAINT [' + @fkname + ']');

				IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = 'fk_backup_history_config_id_cascade')
				ALTER TABLE backup_history
					ADD CONSTRAINT fk_backup_history_config_id_cascade
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE CASCADE;
			`,
			// Down restores a plain, unnamed-action reference on Postgres
			// (matching v1's own text) and drops MySQL's constraint back to
			// v1's unenforced state. Neither restores the exact original SQL
			// Server auto-generated name -- impossible, since it was never
			// recorded.
			Down: `
				ALTER TABLE backup_history DROP CONSTRAINT IF EXISTS backup_history_config_id_fkey;
				ALTER TABLE backup_history ADD CONSTRAINT backup_history_config_id_fkey
					FOREIGN KEY (config_id) REFERENCES backup_config(id);
			`,
			DownMySQL: `
				SET @fk := (
					SELECT COUNT(*) FROM information_schema.table_constraints
					WHERE constraint_schema = DATABASE() AND table_name = 'backup_history'
					  AND constraint_name = 'backup_history_config_id_fkey'
				);
				SET @ddl := IF(@fk > 0,
					'ALTER TABLE backup_history DROP FOREIGN KEY backup_history_config_id_fkey',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;
			`,
			DownMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = 'fk_backup_history_config_id_cascade')
				ALTER TABLE backup_history DROP CONSTRAINT fk_backup_history_config_id_cascade;

				IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE parent_object_id = OBJECT_ID('backup_history') AND referenced_object_id = OBJECT_ID('backup_config'))
				ALTER TABLE backup_history
					ADD CONSTRAINT fk_backup_history_config_id
					FOREIGN KEY (config_id) REFERENCES backup_config(id);
			`,
		},
		{
			// Backups become operator-only. cleat#2247.
			//
			// A tenant request triggered a full-DEPLOYMENT pg_dump -- the
			// dump has no filter and runs against the deployment-wide DSN
			// (pgdump.go), so any tenant creating a backup_config caused
			// every tenant's data to be dumped to disk on that tenant's
			// own schedule. The owner decision: backup configuration
			// moves to an operator surface (cleatctl), and these two
			// tables stop being tenant-owned data.
			//
			// FRESH-DATABASE-ONLY, per #2058 decision 3 (0.3.0 has no
			// upgrade path from 0.2.0): no data migration, and no
			// attempt to preserve rows across the tenant_id drop below.
			//
			// A NEW VERSION, NEVER AN EDIT TO v1/v2: both are recorded on
			// every database that has ever run this plugin, and editing
			// them protects only a database created after this lands.
			// v2's own comment gives the identical reasoning for why IT
			// is a new version rather than a TenantScoped edit to v1.
			//
			// THIS ACTIVELY UNDOES v2, on Postgres, rather than merely
			// omitting TenantScoped going forward: registerTenantScopedTables
			// (plugin/migration.go) only ever sets tenant_scoped = true, so
			// nothing but this migration's own UPDATE can flip the
			// admin.plugin_tables rows v2 created back to false.
			//
			// THE UPDATE IS KEYED ON schema_name AND table_name, NOT
			// plugin_name. plugin.PluginInfo.Name for this plugin is
			// "scheduled-backup" (plugin.go), not "scheduledbackup" --
			// and admin.plugin_tables' primary key is
			// (plugin_name, schema_name, table_name), so a WHERE clause
			// naming the wrong plugin_name matches nothing at all.
			// Measured directly by cleat-review against PostgreSQL
			// 16.14, applying an earlier draft keyed on
			// WHERE plugin_name = 'scheduledbackup': the UPDATE affected
			// 0 rows, both rows stayed tenant_scoped = true, and the
			// NEXT admin.drop_tenant -- for ANY tenant, not one with
			// backup rows -- failed outright:
			//
			//   column "tenant_id" does not exist
			//   DELETE FROM public.backup_config WHERE tenant_id = $1
			//
			// That is a TOTAL admin.drop_tenant outage, not a scoped
			// one: 066_...sql's sweep loop reads every tenant_scoped row
			// unconditionally and would hit this DELETE for every tenant
			// dropped from then on, before touching anything of theirs.
			// Keying on schema_name/table_name instead needs no plugin
			// name at all and cannot go stale the same way a second
			// time.
			//
			// NO DO BLOCK HERE, though a runtime RAISE guarding the
			// UPDATE looks like the obvious belt-and-braces: this file's
			// migrations run through plugin.splitStatements
			// (migration.go), which shreds SQL on every literal ';' with
			// no awareness of $$-quoting -- confirmed by feeding a
			// `DO $$ ... END $$;` block through it directly, which comes
			// back as four invalid fragments, none a valid statement on
			// their own. No migration in this tree uses a DO block for
			// exactly that reason. The regression this would have
			// caught is covered instead by
			// TestSchedulerBackupV4RegistryFlipDoesNotBreakLaterDropTenant
			// (a_v4_migration_leaves_drop_tenant_working_test.go), which
			// reproduces cleat-review's exact scenario end to end.
			//
			// A general invariant over admin.plugin_tables (every
			// tenant_scoped row names a table that still has a tenant_id
			// column, across every plugin, not just this one) would catch
			// the same class for a future plugin. It does not exist yet --
			// tracked as a follow-up, not claimed here.
			//
			// ORDER, on Postgres: policies before ROW LEVEL SECURITY
			// before the indexes before the column before the registry
			// flip. Reversing "policies before RLS" is harmless (a
			// FORCE/ENABLE table with no policy simply denies
			// everything to non-owners, and the migration runs as the
			// table's owner), but dropping the COLUMN before the
			// POLICY is not: the policy's USING/CHECK expression
			// references tenant_id, and PostgreSQL refuses to drop a
			// column a policy depends on.
			Version: 4,
			Up: `
				DROP POLICY IF EXISTS backup_config_tenant_isolation ON backup_config;
				DROP POLICY IF EXISTS backup_config_cross_tenant ON backup_config;
				ALTER TABLE backup_config NO FORCE ROW LEVEL SECURITY;
				ALTER TABLE backup_config DISABLE ROW LEVEL SECURITY;

				DROP POLICY IF EXISTS backup_history_tenant_isolation ON backup_history;
				DROP POLICY IF EXISTS backup_history_cross_tenant ON backup_history;
				ALTER TABLE backup_history NO FORCE ROW LEVEL SECURITY;
				ALTER TABLE backup_history DISABLE ROW LEVEL SECURITY;

				DROP INDEX IF EXISTS idx_backup_config_tenant_enabled_next;
				DROP INDEX IF EXISTS idx_backup_history_tenant_config;

				ALTER TABLE backup_config DROP COLUMN tenant_id;
				ALTER TABLE backup_history DROP COLUMN tenant_id;

				CREATE INDEX IF NOT EXISTS idx_backup_config_enabled_next
					ON backup_config (enabled, next_run_at);
				CREATE INDEX IF NOT EXISTS idx_backup_history_config
					ON backup_history (config_id);

				UPDATE admin.plugin_tables SET tenant_scoped = false
					WHERE schema_name = current_schema()
					  AND table_name IN ('backup_config', 'backup_history');
			`,
			// MySQL has no row-level security and no registry -- the
			// column and its composite indexes are dead weight once
			// nothing scopes by tenant, kept only for a fresh database
			// creating them from scratch under CLEAT_DB_DIALECT=mysql. No
			// IF EXISTS clause covers DROP COLUMN or DROP INDEX on this
			// dialect (unlike Postgres): a re-run raises 1091 "check that
			// column/key exists" rather than no-op-ing, which is "the v8
			// lesson" #2233/#2247 both name -- so both are guarded
			// through information_schema, matching v3's UpMySQL FK guard
			// immediately above.
			UpMySQL: `
				SET @idx := (
					SELECT COUNT(*) FROM information_schema.statistics
					WHERE table_schema = DATABASE() AND table_name = 'backup_config'
					  AND index_name = 'idx_backup_config_tenant_enabled_next'
				);
				SET @ddl := IF(@idx > 0,
					'DROP INDEX idx_backup_config_tenant_enabled_next ON backup_config',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;

				SET @idx := (
					SELECT COUNT(*) FROM information_schema.statistics
					WHERE table_schema = DATABASE() AND table_name = 'backup_history'
					  AND index_name = 'idx_backup_history_tenant_config'
				);
				SET @ddl := IF(@idx > 0,
					'DROP INDEX idx_backup_history_tenant_config ON backup_history',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;

				SET @col := (
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = 'backup_config'
					  AND column_name = 'tenant_id'
				);
				SET @ddl := IF(@col > 0,
					'ALTER TABLE backup_config DROP COLUMN tenant_id',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;

				SET @col := (
					SELECT COUNT(*) FROM information_schema.columns
					WHERE table_schema = DATABASE() AND table_name = 'backup_history'
					  AND column_name = 'tenant_id'
				);
				SET @ddl := IF(@col > 0,
					'ALTER TABLE backup_history DROP COLUMN tenant_id',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;

				SET @idx := (
					SELECT COUNT(*) FROM information_schema.statistics
					WHERE table_schema = DATABASE() AND table_name = 'backup_config'
					  AND index_name = 'idx_backup_config_enabled_next'
				);
				SET @ddl := IF(@idx = 0,
					'CREATE INDEX idx_backup_config_enabled_next ON backup_config (enabled, next_run_at)',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;

				SET @idx := (
					SELECT COUNT(*) FROM information_schema.statistics
					WHERE table_schema = DATABASE() AND table_name = 'backup_history'
					  AND index_name = 'idx_backup_history_config'
				);
				SET @ddl := IF(@idx = 0,
					'CREATE INDEX idx_backup_history_config ON backup_history (config_id)',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;
			`,
			// SQL Server: the SECURITY POLICY must go before the COLUMN
			// it filters on, exactly as on Postgres and for the same
			// underlying reason -- measured directly by cleat-review,
			// dropping the column first fails with
			//
			//   Msg 5074: The object 'backup_config_tenant_isolation'
			//   is dependent on column 'tenant_id'
			//   Msg 4922: ALTER TABLE DROP COLUMN tenant_id failed
			//   because ... is dependent on it
			//
			// dbo.fn_plugin_tenant_filter is NOT dropped: it is the
			// predicate function every plugin's tenant policy binds to
			// (migration.go's mssqlPluginTenantFilter doc comment), and
			// 72 other security-policy predicates in this database
			// depend on it surviving.
			//
			// Once the policy is gone, migrations/mssql/074's
			// sys.columns sweep stops finding these two tables on its
			// own -- it derives the tenant-owned set live rather than
			// from a registry, so dropping the column IS the whole of
			// the PostgreSQL-registry-equivalent fix here, and no
			// analogue of the UPDATE above exists or is needed on this
			// dialect.
			//
			// No BEGIN/END anywhere in this arm, for the same
			// splitStatements reason v3's UpMSSQL gives: a block
			// straddling a ';' this runner treats as a statement
			// boundary produces two invalid batches.
			UpMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'backup_config_tenant_isolation')
				DROP SECURITY POLICY backup_config_tenant_isolation;

				IF EXISTS (SELECT 1 FROM sys.security_policies WHERE name = N'backup_history_tenant_isolation')
				DROP SECURITY POLICY backup_history_tenant_isolation;

				DROP INDEX IF EXISTS idx_backup_config_tenant_enabled_next ON backup_config;
				DROP INDEX IF EXISTS idx_backup_history_tenant_config ON backup_history;

				IF COL_LENGTH('backup_config', 'tenant_id') IS NOT NULL
				ALTER TABLE backup_config DROP COLUMN tenant_id;

				IF COL_LENGTH('backup_history', 'tenant_id') IS NOT NULL
				ALTER TABLE backup_history DROP COLUMN tenant_id;

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_backup_config_enabled_next' AND object_id = OBJECT_ID('backup_config'))
				CREATE INDEX idx_backup_config_enabled_next
					ON backup_config (enabled, next_run_at);

				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_backup_history_config' AND object_id = OBJECT_ID('backup_history'))
				CREATE INDEX idx_backup_history_config
					ON backup_history (config_id);
			`,
			// Irreversible (plugin.Migration field, cleat#2247) rather than a
			// Down: reversing "backups are operator-only" would need to
			// reinstate a tenant_id column with no source of truth for what
			// value each existing row should get (the whole point of this
			// migration is that the column is gone), which is a
			// data-recovery decision, not a mechanical schema reversal like
			// v1-v3's Down arms. Not DialectSpecific: that field justifies a
			// missing Up ARM for one dialect, and this migration has all
			// three (Up, UpMySQL, UpMSSQL) -- what it lacks is a Down, on
			// every dialect, for the same reason on each.
			Irreversible: "cleat#2247: drops backup_config.tenant_id and " +
				"backup_history.tenant_id with no source of truth for a restored value; " +
				"see the comment above this migration for the full reasoning",
		},
		{
			// backup_history.config_id loses ON DELETE CASCADE. cleat#2247,
			// found by TestBackupCommandWorksOnEveryDialect rather than by
			// reading: `cleatctl backup config-delete` prints "its
			// backup_history rows are unaffected" (backup.go), and until
			// this migration that claim was false on every dialect --
			// deleting a backup_config row silently deleted every
			// backup_history row that pointed at it.
			//
			// v3 ADDED that cascade deliberately, for cleat#2234:
			// admin.drop_tenant deletes backup_config before backup_history
			// (alphabetical sweep order), and the FK made a tenant with
			// backup history un-droppable without it. That reasoning no
			// longer holds: since v4 flips both tables to
			// tenant_scoped = false in admin.plugin_tables, drop_tenant's
			// sweep does not touch either table at all any more --
			// TestSchedulerBackupV4RegistryFlipDoesNotBreakLaterDropTenant
			// (a_v4_migration_leaves_drop_tenant_working_test.go) exercises
			// exactly that. So the cascade outlived the one caller that
			// needed it, and cleatctl backup config-delete (cleat#2247,
			// added after v4) inherited a side effect nobody intended for
			// it: an operator deleting a config to stop it running would
			// also erase that config's audit trail of past backup attempts.
			//
			// SET NULL, not a plain drop of the FK: config_id stays
			// enforced against backup_config while a config exists (a
			// history row can still only be created for a real config), and
			// a deleted config's history rows keep every other column --
			// filename, status, timestamps, error_message -- with config_id
			// nulled out rather than the row vanishing. config_id has never
			// been NOT NULL, on any dialect (v1's Up/UpMySQL/UpMSSQL all
			// leave it nullable), so no column change is needed here.
			//
			// A NEW VERSION, NEVER AN EDIT TO v3: v3 is recorded on every
			// database that has run this plugin since cleat#2234, and
			// editing it would only fix a database created after this
			// lands. Same reasoning v2's and v3's own comments give for why
			// each is a new version.
			Version: 5,
			Up: `
				ALTER TABLE backup_history DROP CONSTRAINT IF EXISTS backup_history_config_id_fkey;
				ALTER TABLE backup_history ADD CONSTRAINT backup_history_config_id_fkey
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE SET NULL;
			`,
			// Guarded the same way v3's UpMySQL is: MySQL raises
			// ER_DUP_KEYNAME/ER_FK_DUP_NAME on a re-add rather than
			// silently no-op-ing, and there is no ADD CONSTRAINT IF NOT
			// EXISTS to lean on. The DROP has to run unconditionally before
			// the guarded ADD -- v3's cascade constraint carries the same
			// name this migration re-adds under, so leaving it in place
			// would make the ADD's own guard (checking for that name) see
			// it as already done and skip re-adding it with SET NULL.
			UpMySQL: `
				SET @fk := (
					SELECT COUNT(*) FROM information_schema.table_constraints
					WHERE constraint_schema = DATABASE() AND table_name = 'backup_history'
					  AND constraint_name = 'backup_history_config_id_fkey'
				);
				SET @ddl := IF(@fk > 0,
					'ALTER TABLE backup_history DROP FOREIGN KEY backup_history_config_id_fkey',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;

				ALTER TABLE backup_history ADD CONSTRAINT backup_history_config_id_fkey
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE SET NULL;
			`,
			// v3's UpMSSQL named the cascade constraint explicitly
			// (fk_backup_history_config_id_cascade) rather than relying on
			// an auto-generated name, specifically so a later migration
			// could find it by name instead of looking it up the way v3
			// itself had to for the ORIGINAL auto-named constraint. This is
			// that later migration.
			UpMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = 'fk_backup_history_config_id_cascade')
				ALTER TABLE backup_history DROP CONSTRAINT fk_backup_history_config_id_cascade;

				IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE parent_object_id = OBJECT_ID('backup_history') AND referenced_object_id = OBJECT_ID('backup_config'))
				ALTER TABLE backup_history
					ADD CONSTRAINT fk_backup_history_config_id_setnull
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE SET NULL;
			`,
			// Down restores v3's cascade exactly, on all three dialects.
			Down: `
				ALTER TABLE backup_history DROP CONSTRAINT IF EXISTS backup_history_config_id_fkey;
				ALTER TABLE backup_history ADD CONSTRAINT backup_history_config_id_fkey
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE CASCADE;
			`,
			DownMySQL: `
				SET @fk := (
					SELECT COUNT(*) FROM information_schema.table_constraints
					WHERE constraint_schema = DATABASE() AND table_name = 'backup_history'
					  AND constraint_name = 'backup_history_config_id_fkey'
				);
				SET @ddl := IF(@fk > 0,
					'ALTER TABLE backup_history DROP FOREIGN KEY backup_history_config_id_fkey',
					'DO 0');
				PREPARE stmt FROM @ddl;
				EXECUTE stmt;
				DEALLOCATE PREPARE stmt;

				ALTER TABLE backup_history ADD CONSTRAINT backup_history_config_id_fkey
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE CASCADE;
			`,
			DownMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.foreign_keys WHERE name = 'fk_backup_history_config_id_setnull')
				ALTER TABLE backup_history DROP CONSTRAINT fk_backup_history_config_id_setnull;

				IF NOT EXISTS (SELECT 1 FROM sys.foreign_keys WHERE parent_object_id = OBJECT_ID('backup_history') AND referenced_object_id = OBJECT_ID('backup_config'))
				ALTER TABLE backup_history
					ADD CONSTRAINT fk_backup_history_config_id_cascade
					FOREIGN KEY (config_id) REFERENCES backup_config(id) ON DELETE CASCADE;
			`,
		},
	}
}
