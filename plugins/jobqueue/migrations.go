package jobqueue

import "github.com/rcownie/cleat/internal/plugin"

// Migrations returns the database schema for the job queue. Tables are
// idempotent (IF NOT EXISTS) and safe to run multiple times.
func (p *Plugin) Migrations() []plugin.Migration {
	return []plugin.Migration{
		{
			Version: 1,
			Up: `
				CREATE TABLE IF NOT EXISTS task_queue (
					tenant_id    UUID NOT NULL,
					queue_name   TEXT NOT NULL,
					job_id       UUID NOT NULL,
					payload      JSONB NOT NULL DEFAULT '{}',
					status       TEXT NOT NULL DEFAULT 'pending',
					created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
					started_at   TIMESTAMPTZ,
					completed_at TIMESTAMPTZ,
					PRIMARY KEY (tenant_id, queue_name, job_id)
				);

				CREATE INDEX IF NOT EXISTS idx_task_queue_status ON task_queue(tenant_id, status);
				CREATE INDEX IF NOT EXISTS idx_task_queue_created ON task_queue(tenant_id, queue_name, created_at DESC);
			`,
			UpMySQL: "\n" +
				"\t\t\t\tCREATE TABLE IF NOT EXISTS task_queue (\n" +
				"\t\t\t\t\ttenant_id    CHAR(36) NOT NULL,\n" +
				"\t\t\t\t\tqueue_name   VARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\tjob_id       CHAR(36) NOT NULL,\n" +
				"\t\t\t\t\tpayload      JSON NOT NULL DEFAULT ('{}'),\n" +
				"\t\t\t\t\t`status`     VARCHAR(255) NOT NULL DEFAULT 'pending',\n" +
				"\t\t\t\t\tcreated_at   TIMESTAMP(6) NOT NULL DEFAULT NOW(6),\n" +
				"\t\t\t\t\tstarted_at   TIMESTAMP(6),\n" +
				"\t\t\t\t\tcompleted_at TIMESTAMP(6),\n" +
				"\t\t\t\t\tPRIMARY KEY (tenant_id, queue_name, job_id)\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_status ON task_queue(tenant_id, `status`);\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_created ON task_queue(tenant_id, queue_name, created_at DESC);\n" +
				"\t\t\t",
			UpMSSQL: "\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.tables WHERE name = 'task_queue')\n" +
				"\t\t\t\tCREATE TABLE task_queue (\n" +
				"\t\t\t\t\ttenant_id    UNIQUEIDENTIFIER NOT NULL,\n" +
				"\t\t\t\t\tqueue_name   NVARCHAR(255) NOT NULL,\n" +
				"\t\t\t\t\tjob_id       UNIQUEIDENTIFIER NOT NULL,\n" +
				"\t\t\t\t\tpayload      NVARCHAR(MAX) NOT NULL DEFAULT ('{}'),\n" +
				"\t\t\t\t\t[status]     NVARCHAR(MAX) NOT NULL DEFAULT 'pending',\n" +
				"\t\t\t\t\tcreated_at   DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),\n" +
				"\t\t\t\t\tstarted_at   DATETIMEOFFSET,\n" +
				"\t\t\t\t\tcompleted_at DATETIMEOFFSET,\n" +
				"\t\t\t\t\tPRIMARY KEY (tenant_id, queue_name, job_id)\n" +
				"\t\t\t\t);\n" +
				"\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_task_queue_status' AND object_id = OBJECT_ID('task_queue'))\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_status ON task_queue(tenant_id, [status]);\n" +
				"\t\t\t\tIF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = 'idx_task_queue_created' AND object_id = OBJECT_ID('task_queue'))\n" +
				"\t\t\t\tCREATE INDEX idx_task_queue_created ON task_queue(tenant_id, queue_name, created_at DESC);\n" +
				"\t\t\t",
			Down: `
				DROP TABLE IF EXISTS task_queue;
			`,
		},
		{
			Version: 2,
			Up: `ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS def_name TEXT;
			     ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS input JSONB;
			     ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS run_id TEXT;`,
			UpMySQL: `
				ALTER TABLE task_queue ADD COLUMN def_name VARCHAR(255);
				ALTER TABLE task_queue ADD COLUMN input JSON;
				ALTER TABLE task_queue ADD COLUMN run_id VARCHAR(255);
			`,
			UpMSSQL: `
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('task_queue') AND name = 'def_name')
				ALTER TABLE task_queue ADD def_name NVARCHAR(MAX);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('task_queue') AND name = 'input')
				ALTER TABLE task_queue ADD input NVARCHAR(MAX);
				IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE object_id = OBJECT_ID('task_queue') AND name = 'run_id')
				ALTER TABLE task_queue ADD run_id NVARCHAR(MAX);
			`,
			Down: `ALTER TABLE task_queue DROP COLUMN IF EXISTS run_id;
			       ALTER TABLE task_queue DROP COLUMN IF EXISTS input;
			       ALTER TABLE task_queue DROP COLUMN IF EXISTS def_name;`,
		},
	}
}
