package datadogexport

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for Datadog export configs. The
// table is idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS dd_config (
					tenant_id      UUID NOT NULL,
					id             UUID PRIMARY KEY,
					name           TEXT,
					api_key        TEXT NOT NULL,
					site           TEXT DEFAULT 'datadoghq.com',
					metrics_prefix TEXT DEFAULT 'cleat',
					enabled        BOOLEAN DEFAULT true,
					created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
				);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS dd_config (
					tenant_id      CHAR(36) NOT NULL,
					id             CHAR(36) PRIMARY KEY,
					` + "`name`" + `         TEXT,
					api_key        TEXT NOT NULL,
					site           VARCHAR(255) DEFAULT 'datadoghq.com',
					metrics_prefix VARCHAR(255) DEFAULT 'cleat',
					enabled        TINYINT(1) DEFAULT 1,
					created_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at     TIMESTAMP(6) NOT NULL DEFAULT NOW(6)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'dd_config')
				CREATE TABLE dd_config (
					tenant_id      UNIQUEIDENTIFIER NOT NULL,
					id             UNIQUEIDENTIFIER PRIMARY KEY,
					[name]         NVARCHAR(MAX),
					api_key        NVARCHAR(MAX) NOT NULL,
					site           NVARCHAR(MAX) DEFAULT 'datadoghq.com',
					metrics_prefix NVARCHAR(MAX) DEFAULT 'cleat',
					enabled        BIT DEFAULT 1,
					created_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at     DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()
				);
			`,
			Down: `
				DROP TABLE IF EXISTS dd_config;
			`,
		},
		{
			Version: 2,
			Up: `
				CREATE TABLE IF NOT EXISTS plugin_lease (
					name       TEXT PRIMARY KEY,
					holder     TEXT NOT NULL,
					expires_at TIMESTAMPTZ NOT NULL
				);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS plugin_lease (
					name       VARCHAR(255) PRIMARY KEY,
					holder     VARCHAR(255) NOT NULL,
					expires_at TIMESTAMP(6) NOT NULL
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'plugin_lease')
				CREATE TABLE plugin_lease (
					name       NVARCHAR(255) PRIMARY KEY,
					holder     NVARCHAR(255) NOT NULL,
					expires_at DATETIMEOFFSET NOT NULL
				);
			`,
			Down: `
				DROP TABLE IF EXISTS plugin_lease;
			`,
		},
		{
			// Tenant isolation for dd_config. cleat#1278.
			//
			// A separate version rather than a TenantScoped on v1: v1 is
			// already recorded as applied wherever datadogexport runs, and a
			// recorded migration never runs again -- editing it would protect
			// new databases and leave every existing one open.
			//
			// ONLY dd_config. plugin_lease (v2) is deliberately NOT listed, and
			// that is the judgement this plugin adds over auditlog's: it has no
			// tenant_id at all. It is a leader-election lease keyed by name,
			// one row for the whole fleet, and it is correct for every worker
			// of every tenant to contend for the same row. A tenant column
			// could be invented for it, and the lease would then elect one
			// leader per tenant, which is not what leader election here means.
			//
			// So a plugin table is one of THREE things, not two:
			//
			//	tenant-scoped   dd_config       -> TenantScoped, policy applies
			//	global by design plugin_lease   -> no TenantScoped, no policy,
			//	                                   and its loops need no bypass
			//	                                   because nothing scopes them
			//	unscoped by oversight           -> the thing cleat#1278 is for
			//
			// The second and third look identical in the schema. The difference
			// is whether a tenant OWNS the rows, and only a reader of the
			// plugin can answer it -- which is why this is a per-plugin
			// judgement and not a sweep.
			//
			// Up is empty on purpose: the policy is emitted by the runtime from
			// TenantScoped. On MySQL and SQL Server this version is recorded and
			// does nothing, which is what the field documents.
			Version:      3,
			TenantScoped: []string{"dd_config"},
		},
		{
			// dd_config.api_key moves into tenant secrets. cleat#1992.
			//
			// No backfill step: cleat#2058 (owner decision 3) settled that
			// 0.3.0 requires a fresh database, with no upgrade path from
			// v0.2.0. There is therefore no deployment where this column
			// holds a value that needs to survive the DROP -- a fresh
			// database never populates api_key in the first place. An
			// earlier version of this comment described a two-step,
			// operator-run migration procedure; that no longer applies and
			// would be actively wrong advice against a fresh install.
			//
			// PER-CONFIG, NOT PER-TENANT NAMING. dd_config is not one row per
			// tenant -- a tenant can have several named Datadog configs (own
			// id, site, metrics_prefix) -- so the secret name is keyed by
			// config id (DatadogAPIKeySecretName, routes.go) rather than a
			// single fixed name per tenant. Confirmed with WS-3 (owner of the
			// Secrets interface and cleat#1992's PR split) before writing this:
			// no downstream assumption of one fixed name per tenant.
			Version: 4,
			Up: `
				ALTER TABLE dd_config DROP COLUMN IF EXISTS api_key;
			`,
			UpMySQL: `
				ALTER TABLE dd_config DROP COLUMN api_key;
			`,
			UpMSSQL: `
				IF EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('dd_config') AND name = 'api_key')
				ALTER TABLE dd_config DROP COLUMN api_key;
			`,
			// Down restores the SCHEMA, not the data -- ordinary for a DROP
			// COLUMN reversal (the value is gone from dd_config the moment Up
			// runs; it now lives in tenant secrets, a different store). No
			// NOT NULL/DEFAULT: existing rows have nothing to put there.
			Down: `
				ALTER TABLE dd_config ADD COLUMN IF NOT EXISTS api_key TEXT;
			`,
			DownMySQL: `
				ALTER TABLE dd_config ADD COLUMN api_key TEXT;
			`,
			DownMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('dd_config') AND name = 'api_key')
				ALTER TABLE dd_config ADD api_key NVARCHAR(MAX);
			`,
		},
	}
}
