package kafkaconnect

import "github.com/cleat-team/cleat/internal/plugin"

// Migrations returns the database schema for Kafka config storage. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS kafka_config (
					tenant_id       UUID NOT NULL,
					id              UUID PRIMARY KEY,
					name            TEXT NOT NULL,
					brokers         TEXT NOT NULL,
					topic           TEXT NOT NULL,
					consumer_group  TEXT NOT NULL DEFAULT 'cleat-consumer',
					enabled         BOOLEAN NOT NULL DEFAULT true,
					created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
					updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
				);

				CREATE INDEX IF NOT EXISTS idx_kafka_config_tenant
					ON kafka_config(tenant_id, created_at DESC);
			`,
			UpMySQL: "\n" +
				"\t\t\t\tCREATE TABLE IF NOT EXISTS kafka_config (\n" +
				"\t\t\t\t\ttenant_id       CHAR(36) NOT NULL,\n" +
				"\t\t\t\t\tid              CHAR(36) PRIMARY KEY,\n" +
				"\t\t\t\t\t`name`          VARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\tbrokers         TEXT NOT NULL,\n" +
				"\t\t\t\t\ttopic           VARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\tconsumer_group  VARCHAR(255) NOT NULL DEFAULT 'cleat-consumer',\n" +
				"\t\t\t\t\tenabled         TINYINT(1) NOT NULL DEFAULT 1,\n" +
				"\t\t\t\t\tcreated_at      TIMESTAMP(6) NOT NULL DEFAULT NOW(6),\n" +
				"\t\t\t\t\tupdated_at      TIMESTAMP(6) NOT NULL DEFAULT NOW(6)\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tCREATE INDEX idx_kafka_config_tenant\n" +
				"\t\t\t\t\tON kafka_config(tenant_id, created_at DESC);\n" +
				"\t\t\t",
			UpMSSQL: "\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'kafka_config')\n" +
				"\t\t\t\tCREATE TABLE kafka_config (\n" +
				"\t\t\t\t\ttenant_id       UNIQUEIDENTIFIER NOT NULL,\n" +
				"\t\t\t\t\tid              UNIQUEIDENTIFIER PRIMARY KEY,\n" +
				"\t\t\t\t\t[name]          NVARCHAR(MAX) NOT NULL,\n" +
				"\t\t\t\t\tbrokers         NVARCHAR(MAX) NOT NULL,\n" +
				"\t\t\t\t\ttopic           NVARCHAR(MAX) NOT NULL,\n" +
				"\t\t\t\t\tconsumer_group  NVARCHAR(MAX) NOT NULL DEFAULT 'cleat-consumer',\n" +
				"\t\t\t\t\tenabled         BIT NOT NULL DEFAULT 1,\n" +
				"\t\t\t\t\tcreated_at      DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),\n" +
				"\t\t\t\t\tupdated_at      DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME()\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_kafka_config_tenant' AND object_id = OBJECT_ID('kafka_config'))\n" +
				"\t\t\t\tCREATE INDEX idx_kafka_config_tenant\n" +
				"\t\t\t\t\tON kafka_config(tenant_id, created_at DESC);\n" +
				"\t\t\t",
			Down: `
				DROP TABLE IF EXISTS kafka_config;
			`,
		},
		{
			Version: 2,
			Up: `
				ALTER TABLE kafka_config ADD COLUMN IF NOT EXISTS event_type TEXT NOT NULL DEFAULT '';
				CREATE INDEX IF NOT EXISTS idx_kafka_config_enabled ON kafka_config(enabled);
			`,
			UpMySQL: `
				ALTER TABLE kafka_config ADD COLUMN event_type VARCHAR(255) NOT NULL DEFAULT '';
				CREATE INDEX idx_kafka_config_enabled ON kafka_config(enabled);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('kafka_config') AND name = 'event_type')
				ALTER TABLE kafka_config ADD event_type NVARCHAR(MAX) NOT NULL DEFAULT '';
				IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_kafka_config_enabled' AND object_id = OBJECT_ID('kafka_config'))
				CREATE INDEX idx_kafka_config_enabled ON kafka_config(enabled);
			`,
			Down: `
				ALTER TABLE kafka_config DROP COLUMN IF EXISTS event_type;
				DROP INDEX IF EXISTS idx_kafka_config_enabled;
			`,
		},
	}
}
