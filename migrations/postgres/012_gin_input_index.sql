-- Migration 012: Add GIN index on workflow_instances.input for full-text JSONB search.
-- Enables efficient JSONB containment queries (e.g., WHERE input @> '{"field":"value"}').
CREATE INDEX IF NOT EXISTS idx_instances_input_gin
  ON workflow_instances
  USING GIN (input jsonb_path_ops);
