-- cleat migration 062 (postgres): a run can lower its own limits
--
-- cleat#1187. tenant_settings (039) lets a TENANT tighten three bounds below
-- the operator's flags. The decision this issue records asks for the same at
-- the next level down -- "one worker can be running workflows from multiple
-- tenants, so there should be a per-tenant setting. And a per-run override."
-- These are the columns that override lives in.
--
-- WHY COLUMNS AND NOT A REQUEST FIELD. The value has to survive re-claim and
-- replay. A run whose worker dies is claimed again by another, and a durable
-- execution replays its whole history; if the override lived only in the start
-- request, a workflow's limits would silently change the first time either
-- happened. The row is the only place that is true for.
--
-- NULLABLE, and the distinction is load-bearing in the same way 039 documents:
-- NULL means "no override, resolve to the tier above", while a value means
-- "this run asks for at most this". Zero is not usable for either, because
-- zero is how ClampToCeiling already spells "unset" -- so a column that stored
-- 0 for "no override" would be indistinguishable from one that stored 0 for
-- "unbounded", and the second is exactly the escalation the clamp exists to
-- prevent.
--
-- THE CHECKS ARE A PRIVILEGE BOUNDARY, NOT INPUT VALIDATION, copied in intent
-- from 039. A run may only ever TIGHTEN: resolution is
-- ClampToCeiling(run, ClampToCeiling(tenant, operator)), so a larger value is
-- clamped away rather than honoured. What the CHECK forbids is the shape that
-- clamping cannot save us from -- a non-positive number, which would read as
-- "unset" one layer up and quietly widen the bound instead of narrowing it.
--
-- No index. These columns are read only for a row already being claimed by id,
-- never searched on.

-- Pin the creation target; see the note in 001_schema.sql. The default
-- search_path is "$user", public, and 001 creates a schema called "cleat"
-- while the shipped compose connects as POSTGRES_USER=cleat, so unqualified
-- names below would resolve against the wrong schema.
SET search_path = public;

ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS run_wasm_instance_timeout_ms  BIGINT,
    ADD COLUMN IF NOT EXISTS run_wasm_wall_clock_ceiling_ms BIGINT,
    ADD COLUMN IF NOT EXISTS run_host_retry_budget_ms       BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_wi_run_instance_timeout_positive') THEN
        ALTER TABLE workflow_instances ADD CONSTRAINT ck_wi_run_instance_timeout_positive
            CHECK (run_wasm_instance_timeout_ms IS NULL OR run_wasm_instance_timeout_ms > 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_wi_run_wall_clock_positive') THEN
        ALTER TABLE workflow_instances ADD CONSTRAINT ck_wi_run_wall_clock_positive
            CHECK (run_wasm_wall_clock_ceiling_ms IS NULL OR run_wasm_wall_clock_ceiling_ms > 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_wi_run_retry_budget_positive') THEN
        ALTER TABLE workflow_instances ADD CONSTRAINT ck_wi_run_retry_budget_positive
            CHECK (run_host_retry_budget_ms IS NULL OR run_host_retry_budget_ms > 0);
    END IF;
END $$;

COMMENT ON COLUMN workflow_instances.run_wasm_instance_timeout_ms IS
    'cleat#1187: this run''s own bound on guest execution, clamped to the tenant setting and then to the operator flag. NULL = no override.';
COMMENT ON COLUMN workflow_instances.run_wasm_wall_clock_ceiling_ms IS
    'cleat#1187: this run''s own wall-clock ceiling. NULL = no override.';
COMMENT ON COLUMN workflow_instances.run_host_retry_budget_ms IS
    'cleat#1187: this run''s own host-retry budget. NULL = no override.';
