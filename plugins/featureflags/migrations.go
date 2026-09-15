package featureflags

import "github.com/cleat-team/cleat/plugin"

// Migrations returns the database schema for feature flags. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS feature_flags (
					tenant_id          UUID NOT NULL,
					id                 UUID PRIMARY KEY,
					key                TEXT NOT NULL,
					name               TEXT,
					description        TEXT,
					enabled            BOOLEAN NOT NULL DEFAULT false,
					rules              JSONB NOT NULL DEFAULT '[]',
					rollout_percentage INTEGER NOT NULL DEFAULT 0,
					created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
					UNIQUE (tenant_id, key)
				);

				CREATE INDEX IF NOT EXISTS idx_feature_flags_tenant_key
					ON feature_flags (tenant_id, key);
			`,
			UpMySQL: "\n" +
				"\t\t\t\tCREATE TABLE IF NOT EXISTS feature_flags (\n" +
				"\t\t\t\t\ttenant_id          CHAR(36) NOT NULL,\n" +
				"\t\t\t\t\tid                 CHAR(36) PRIMARY KEY,\n" +
				"\t\t\t\t\t`key`              VARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\t`name`             VARCHAR(255),\n" +
				"\t\t\t\t\tdescription        TEXT,\n" +
				"\t\t\t\t\tenabled            TINYINT(1) NOT NULL DEFAULT 0,\n" +
				"\t\t\t\t\trules              JSON NOT NULL DEFAULT ('[]'),\n" +
				"\t\t\t\t\trollout_percentage INT NOT NULL DEFAULT 0,\n" +
				"\t\t\t\t\tcreated_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),\n" +
				"\t\t\t\t\tupdated_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),\n" +
				"\t\t\t\t\tUNIQUE (tenant_id, `key`)\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tCREATE INDEX idx_feature_flags_tenant_key\n" +
				"\t\t\t\t\tON feature_flags (tenant_id, `key`);\n" +
				"\t\t\t",
			UpMSSQL: "\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'feature_flags')\n" +
				"\t\t\t\tCREATE TABLE feature_flags (\n" +
				"\t\t\t\t\ttenant_id          UNIQUEIDENTIFIER NOT NULL,\n" +
				"\t\t\t\t\tid                 UNIQUEIDENTIFIER PRIMARY KEY,\n" +
				"\t\t\t\t\t[key]              NVARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\t[name]             NVARCHAR(MAX),\n" +
				"\t\t\t\t\tdescription        NVARCHAR(MAX),\n" +
				"\t\t\t\t\tenabled            BIT NOT NULL DEFAULT 0,\n" +
				"\t\t\t\t\trules              NVARCHAR(MAX) NOT NULL DEFAULT ('[]'),\n" +
				"\t\t\t\t\trollout_percentage INT NOT NULL DEFAULT 0,\n" +
				"\t\t\t\t\tcreated_at         DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),\n" +
				"\t\t\t\t\tupdated_at         DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),\n" +
				"\t\t\t\t\tUNIQUE (tenant_id, [key])\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_feature_flags_tenant_key' AND object_id = OBJECT_ID('feature_flags'))\n" +
				"\t\t\t\tCREATE INDEX idx_feature_flags_tenant_key\n" +
				"\t\t\t\t\tON feature_flags (tenant_id, [key]);\n" +
				"\t\t\t",
			Down: `
				DROP TABLE IF EXISTS feature_flags;
			`,
		},
		{
			// Tenant isolation for feature_flags. cleat#1512.
			//
			// A NEW VERSION, NOT A TenantScoped ON v1. A recorded migration
			// never runs again, so editing v1 would protect databases created
			// after this lands and leave every existing one open -- the same
			// reason kvstore's v2 exists rather than an edit to its v1.
			//
			// Up is empty on purpose: the policy is emitted by the runtime from
			// TenantScoped, so there is no SQL to write and none that could be
			// dialect-specific. On MySQL and SQL Server this version is recorded
			// and installs nothing, because neither has row-level security.
			//
			// WHY featureflags QUALIFIES, which is a different question from
			// "does it compile". The policy calls cleat.assert_tenant_set(),
			// which RAISEs when no tenant is in scope, so every access site has
			// to have one. There are two kinds here and both do:
			//
			//   - the six HTTP handlers in routes.go run on r.Context(), which
			//     the auth middleware has already put the tenant into;
			//   - evaluate_flag in host_functions.go is a HOST CALL, and that
			//     path carried no tenant until cleat#1492 bridged the
			//     workflow's own tenant into tenantctx at the PluginCall
			//     boundary. It reaches the database through plugin.PluginDB,
			//     whose SQLDBAdapter.QueryRow calls beginTenantTx, so the
			//     bridged value is what sets cleat.tenant_id.
			//
			// That second bullet is why this could not have been done before
			// 2026-09-13 and is worth stating: a host-call access site is a
			// third category, and the rollout table in cleat#1512 classifies
			// plugins only by whether they have a background loop.
			//
			// featureflags has no background loop, so plugin.AcrossAllTenants
			// is not needed anywhere -- nothing here reads across tenants.
			Version:      2,
			TenantScoped: []string{"feature_flags"},
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
			// This is migrations/mysql/070 applied to featureflags, including the
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
				ALTER TABLE feature_flags
					MODIFY rules LONGTEXT NOT NULL DEFAULT ('[]');

				ALTER TABLE feature_flags
					ADD CONSTRAINT ck_feature_flags_rules CHECK (JSON_VALID(rules));
			`,
			// Reversal is MySQL-only because the change is. It restores the
			// JSON type and with it the narrowing -- a value stored intact
			// while this migration was applied is rewritten by the ALTER
			// itself, so this is lossy and is only here because a migration
			// that writes SQL must be reversible.
			DownMySQL: `
				ALTER TABLE feature_flags
					DROP CONSTRAINT ck_feature_flags_rules;

				ALTER TABLE feature_flags
					MODIFY rules JSON NOT NULL DEFAULT ('[]');
			`,
		},
	}
}
