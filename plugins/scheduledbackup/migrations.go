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
	}
}
