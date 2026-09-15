package kvstore

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for the key-value store. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS kv_store (
					tenant_id   UUID NOT NULL,
					key         TEXT NOT NULL,
					value       JSONB NOT NULL DEFAULT 'null',
					version     INTEGER NOT NULL DEFAULT 1,
					created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
					PRIMARY KEY (tenant_id, key)
				);
			`,
			UpMySQL: `
				CREATE TABLE IF NOT EXISTS kv_store (
					tenant_id   CHAR(36) NOT NULL,
					` + "`key`" + `       VARCHAR(255) NOT NULL,
					value       JSON NOT NULL DEFAULT ('null'),
					version     INT NOT NULL DEFAULT 1,
					created_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					updated_at  TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
					PRIMARY KEY (tenant_id, ` + "`key`" + `)
				);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'kv_store')
				CREATE TABLE kv_store (
					tenant_id   UNIQUEIDENTIFIER NOT NULL,
					[key]       NVARCHAR(255) NOT NULL,
					value       NVARCHAR(MAX) NOT NULL DEFAULT ('null'),
					version     INT NOT NULL DEFAULT 1,
					created_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					updated_at  DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
					PRIMARY KEY (tenant_id, [key])
				);
			`,
			Down: `
				DROP TABLE IF EXISTS kv_store;
			`,
		},
		{
			// Tenant isolation for kv_store. cleat#1277.
			//
			// A separate version rather than a TenantScoped on v1, because
			// v1 is already recorded as applied everywhere kvstore runs and
			// a recorded migration never runs again -- editing it would
			// protect new databases and leave every existing one open.
			//
			// kvstore is the first plugin to take this, and it qualifies
			// because every one of its access sites is request-scoped: all
			// four handlers read the tenant from the request context and
			// already answer 401 "tenant required" without one. A plugin
			// that also sweeps across tenants from a background loop cannot
			// adopt this yet -- the policy fails closed and those sweeps
			// have no tenant in context.
			//
			// Up is empty on purpose: the policy is emitted by the runtime
			// from TenantScoped. On MySQL and SQL Server this version is
			// recorded and does nothing, which is what the field documents.
			Version:      2,
			TenantScoped: []string{"kv_store"},
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
			// This is migrations/mysql/070 applied to kvstore, including the
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
			Version:         3,
			Up:              "",
			DialectSpecific: "MySQL only: converting this plugin's JSON columns to LONGTEXT. PostgreSQL's JSONB preserves a large number already and SQL Server has always used NVARCHAR(MAX) here, so neither has anything to do and an arm for them would be a statement that must not exist. cleat#1622.",
			UpMySQL: `
				ALTER TABLE kv_store
					MODIFY value LONGTEXT NOT NULL DEFAULT ('null');

				ALTER TABLE kv_store
					ADD CONSTRAINT ck_kv_store_value CHECK (JSON_VALID(value));
			`,
			// Reversal is MySQL-only because the change is. It restores the
			// JSON type and with it the narrowing -- a value stored intact
			// while this migration was applied is rewritten by the ALTER
			// itself, so this is lossy and is only here because a migration
			// that writes SQL must be reversible.
			DownMySQL: `
				ALTER TABLE kv_store
					DROP CONSTRAINT ck_kv_store_value;

				ALTER TABLE kv_store
					MODIFY value JSON NOT NULL DEFAULT ('null');
			`,
		},
	}
}
