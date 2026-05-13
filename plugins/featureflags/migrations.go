package featureflags

import "github.com/cleat-team/cleat/internal/plugin"

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
	}
}
