-- cleat migration 036 (mssql): validate workflow_defs.plugin_deps as JSON.
--
-- plugin_deps is the odd one out. Every other dialect enforces the column's
-- shape at the database:
--
--     postgres   plugin_deps JSONB NOT NULL      -- rejects invalid JSON
--     mysql      plugin_deps JSON NOT NULL       -- rejects invalid JSON
--     mssql      plugin_deps NVARCHAR(MAX)       -- accepts anything
--
-- and SQL Server's own schema already applies exactly this pattern to its other
-- JSON columns -- ck_plugin_defs_config, ck_workflow_instances_input,
-- ck_workflow_instances_result, ck_workflow_instances_query_state,
-- ck_workflow_signals_payload, all in 001_schema.sql. plugin_deps was simply
-- missed. This is not a new convention, it is the existing one applied to the
-- column that skipped it.
--
-- It is not academic. Until the write fix that landed alongside this,
-- MSSQLStore.DeployWorkflowDef passed the marshalled JSON as []byte, which
-- go-mssqldb binds as VARBINARY; the implicit conversion into NVARCHAR(MAX)
-- reinterpreted the UTF-8 bytes as UTF-16, so {"llm":"1.2.0"} was stored as
-- mojibake. A validating column type would have rejected that write on the
-- first attempt, the way JSONB and JSON did on the other two dialects -- which
-- is precisely why they never had the bug.
--
-- WITH NOCHECK, AND THAT IS LOAD-BEARING, NOT TIDINESS.
--
-- Every plugin_deps row written by SQL Server before the write fix is mangled,
-- and mangled text is not JSON -- measured 2026-09-01 against a live server:
--
--     ISJSON('{"llm":"1.2.0"}')  = 1
--     ISJSON('<the mojibake>')   = 0
--
-- so a plain ALTER TABLE ... ADD CONSTRAINT, which validates existing rows,
-- would FAIL on every existing deployment and block the upgrade. WITH NOCHECK
-- enforces the constraint on new and updated rows while leaving the historical
-- ones alone, which matches the recovery path already chosen for the read side:
-- the read logs and returns an empty map, and each definition self-heals on its
-- next deploy.
--
-- A consequence worth knowing rather than discovering: the constraint is
-- therefore *untrusted* (sys.check_constraints.is_not_trusted = 1). SQL Server
-- will not use an untrusted constraint for query optimisation, which costs
-- nothing here -- nothing filters on plugin_deps -- and
-- TestPluginDepsCheckConstraintIsUntrusted pins it so that a later "cleanup"
-- to WITH CHECK cannot silently reintroduce the failed upgrade.
--
-- Guarded by IF NOT EXISTS because the shipped .sql files are promised
-- idempotent: docker-compose.cluster.yml mounts them into initdb.d, the docs
-- tell operators to re-run them, and TestShippedSchema_IsIdempotent applies
-- every file twice.

IF NOT EXISTS (
    SELECT 1 FROM sys.check_constraints
    WHERE name = 'ck_workflow_defs_plugin_deps'
      AND parent_object_id = OBJECT_ID('dbo.workflow_defs')
)
BEGIN
    ALTER TABLE workflow_defs WITH NOCHECK
        ADD CONSTRAINT ck_workflow_defs_plugin_deps CHECK (ISJSON(plugin_deps) = 1);
END;
