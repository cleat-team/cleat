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
			Down: `
				DROP TABLE IF EXISTS task_queue;
			`,
		},
		{
			Version: 2,
			Up: `ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS def_name TEXT;
			     ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS input JSONB;
			     ALTER TABLE task_queue ADD COLUMN IF NOT EXISTS run_id TEXT;`,
			Down: `ALTER TABLE task_queue DROP COLUMN IF EXISTS run_id;
			       ALTER TABLE task_queue DROP COLUMN IF EXISTS input;
			       ALTER TABLE task_queue DROP COLUMN IF EXISTS def_name;`,
		},
	}
}
