-- cleat migration 037 (mssql): finish the ISJSON coverage that 036 started.
--
-- 036 fixed workflow_defs.plugin_deps. This closes the rest of the same gap,
-- against a rule that is checkable rather than a matter of taste:
--
--   **A column PostgreSQL declares JSONB must carry an ISJSON check on
--     SQL Server.**
--
-- That is the right boundary because JSONB is where the project actually
-- commits to a value being JSON. PostgreSQL stores event_history's request,
-- response, signal_payload, child_input, new_input, plugin_input,
-- plugin_output and promise_result as plain TEXT -- a service or plugin may
-- legitimately return something that is not JSON -- so constraining those on
-- SQL Server would reject writes the other dialects accept, an inconsistency
-- in the opposite direction. They are deliberately left alone.
--
-- The six columns below are JSONB on PostgreSQL and were unconstrained here.
-- Derived per table, not by column name: `payload` and `result` each appear on
-- several tables and are checked on some of them, so a name-wise grep reports
-- them as covered. See TestMSSQLValidatesEveryPostgresJSONBColumn, which
-- enforces the rule from now on.
--
-- Nullability is taken from the PostgreSQL declaration: only
-- workflow_instances.plugin_vers is NOT NULL, so the other five tolerate NULL
-- explicitly, matching ck_workflow_instances_result's existing shape.
--
-- WITH NOCHECK, for the reason 036 records: a constraint added with validation
-- fails the whole migration if any existing row violates it, which turns a
-- latent data problem into a blocked upgrade. These columns have no known
-- corruption -- unlike plugin_deps, they were never bound as []byte -- so
-- validating would probably succeed, but "probably" is not worth a failed
-- upgrade on someone else's database. The constraints therefore start
-- untrusted and enforce from here on.
--
-- Idempotent: the shipped .sql files are mounted into initdb.d and
-- TestShippedSchema_IsIdempotent applies every file twice.

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = 'ck_event_history_payload'
                 AND parent_object_id = OBJECT_ID('dbo.event_history'))
BEGIN
    ALTER TABLE event_history WITH NOCHECK
        ADD CONSTRAINT ck_event_history_payload
        CHECK (payload IS NULL OR ISJSON(payload) = 1);
END;

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = 'ck_workflow_defs_dag_spec'
                 AND parent_object_id = OBJECT_ID('dbo.workflow_defs'))
BEGIN
    ALTER TABLE workflow_defs WITH NOCHECK
        ADD CONSTRAINT ck_workflow_defs_dag_spec
        CHECK (dag_spec IS NULL OR ISJSON(dag_spec) = 1);
END;

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = 'ck_workflow_instances_compaction_state'
                 AND parent_object_id = OBJECT_ID('dbo.workflow_instances'))
BEGIN
    ALTER TABLE workflow_instances WITH NOCHECK
        ADD CONSTRAINT ck_workflow_instances_compaction_state
        CHECK (compaction_state IS NULL OR ISJSON(compaction_state) = 1);
END;

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = 'ck_workflow_instances_plugin_vers'
                 AND parent_object_id = OBJECT_ID('dbo.workflow_instances'))
BEGIN
    ALTER TABLE workflow_instances WITH NOCHECK
        ADD CONSTRAINT ck_workflow_instances_plugin_vers
        CHECK (ISJSON(plugin_vers) = 1);
END;

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = 'ck_workflow_instances_allowed_signals'
                 AND parent_object_id = OBJECT_ID('dbo.workflow_instances'))
BEGIN
    ALTER TABLE workflow_instances WITH NOCHECK
        ADD CONSTRAINT ck_workflow_instances_allowed_signals
        CHECK (allowed_signals IS NULL OR ISJSON(allowed_signals) = 1);
END;

IF NOT EXISTS (SELECT 1 FROM sys.check_constraints
               WHERE name = 'ck_workflow_update_requests_result'
                 AND parent_object_id = OBJECT_ID('dbo.workflow_update_requests'))
BEGIN
    ALTER TABLE workflow_update_requests WITH NOCHECK
        ADD CONSTRAINT ck_workflow_update_requests_result
        CHECK (result IS NULL OR ISJSON(result) = 1);
END;
