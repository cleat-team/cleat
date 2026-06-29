ALTER TABLE workflow_tags ADD COLUMN IF NOT EXISTS canary_weight INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_tags DROP CONSTRAINT IF EXISTS ck_wf_tags_canary_weight;
ALTER TABLE workflow_tags ADD CONSTRAINT ck_wf_tags_canary_weight CHECK (canary_weight >= 0 AND canary_weight <= 100);
