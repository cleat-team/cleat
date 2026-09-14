-- cleat migration 078 (postgres): a workflow timeout is per-tenant and per-run
--
-- cleat#1117, and the last of the four execution limits to stop being a bare
-- worker flag. 039 gave tenant_settings three columns and 062 gave
-- workflow_instances the matching run-level three; --max-workflow-duration was
-- not carried across either time. These are its two columns.
--
-- WHY IT COULD NOT STAY A FLAG. --max-workflow-duration is a property of the
-- worker PROCESS, and a worker process is not a tenancy boundary: one worker
-- serves many tenants, so a process-wide deadline applies one tenant's
-- operational policy to another tenant's workflows. That is a multi-tenancy
-- defect rather than a granularity inconvenience, which is the reason the
-- repository owner's decision on cleat#1117 asks for both tiers rather than
-- only the per-run one that was originally proposed.
--
-- SCOPE, because the column name invites the wrong reading: despite "workflow"
-- in the name this bounds ONE EXECUTION SEGMENT, not a workflow's whole
-- lifetime. executor.go applies it to the per-invocation context, so a workflow
-- that suspends and resumes gets a fresh deadline each time. That is
-- pre-existing behaviour of --max-workflow-duration and cleat#1117 does not
-- change it -- the issue is about WHO may set the bound, not what it spans.
--
-- NULLABLE, with NULL meaning "no override, resolve to the tier above", and
-- the CHECK forbidding non-positive values. Both copied in intent from 039 and
-- 062 -- zero is how ClampToCeiling already spells "unset", so a column that
-- stored 0 for "no override" would be indistinguishable from one storing 0 for
-- "unbounded", and the second is the escalation the clamp exists to refuse.
--
-- NOTE THE OPERATOR DEFAULT IS 0 AND THAT IS NOT A CONTRADICTION. The flag
-- defaults to 0 meaning "no limit", and ClampToCeiling reads a non-positive
-- CEILING as "operator unbounded, take the tier below". So on a deployment
-- that never set the flag, a tenant's value stands alone -- which is the
-- common case and the point of the feature. The asymmetry is only that 0 means
-- "unbounded" for the operator's flag and "unset" for these columns; the CHECK
-- is what keeps the second from ever being written.
--
-- No index. Read only for a row already being fetched by id, never searched on.

-- Unqualified names resolve through search_path, which migration.Runner sets
-- to the configured schema before applying this file. See 062's header.

ALTER TABLE tenant_settings
    ADD COLUMN IF NOT EXISTS max_workflow_duration_ms BIGINT;

ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS run_max_workflow_duration_ms BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_ts_max_workflow_duration_positive') THEN
        ALTER TABLE tenant_settings ADD CONSTRAINT ck_ts_max_workflow_duration_positive
            CHECK (max_workflow_duration_ms IS NULL OR max_workflow_duration_ms > 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_wi_run_max_workflow_duration_positive') THEN
        ALTER TABLE workflow_instances ADD CONSTRAINT ck_wi_run_max_workflow_duration_positive
            CHECK (run_max_workflow_duration_ms IS NULL OR run_max_workflow_duration_ms > 0);
    END IF;
END $$;

COMMENT ON COLUMN tenant_settings.max_workflow_duration_ms IS
    'cleat#1117: this tenant''s bound on WHOLE-workflow wall clock, clamped to --max-workflow-duration. NULL = no override.';
COMMENT ON COLUMN workflow_instances.run_max_workflow_duration_ms IS
    'cleat#1117: this run''s own bound on WHOLE-workflow wall clock, clamped to the tenant setting and then the operator flag. NULL = no override.';
