-- cleat#3355: entry_point_schemas (005_entry_point_schemas.sql) was added
-- with no ISJSON check, while PostgreSQL declares it jsonb
-- (migrations/postgres/006_entry_point_schemas.sql). Every other mirrored
-- JSON column on this table carries one; this one was missed.
--
-- Nullable, so NULL-tolerant like ck_workflow_defs_dag_spec immediately above
-- it in 001_schema.sql -- the other nullable JSON column on this table, added
-- for the identical reason ("no schema computed" is a real, distinct state
-- from "computed and empty"). WITH NOCHECK for the same reason dag_spec's
-- does: existing rows predate this column's JSON-producing callers and must
-- not be forced through a validation pass to keep their current (NULL) value.
--
-- Guarded through sys.check_constraints, the same shape
-- 004_workflow_defs_exposure_class.sql's CHECK addition uses -- a bare ALTER
-- would fail "There is already an object named ..." on a second run, and
-- this file (like every file above the baseline) is replayed by
-- migration.NewRunner's schema_migrations dedupe only, not by SQL Server
-- itself refusing a duplicate.
IF NOT EXISTS (SELECT 1 FROM sys.check_constraints WHERE name = 'ck_workflow_defs_entry_point_schemas')
    ALTER TABLE dbo.workflow_defs WITH NOCHECK ADD CONSTRAINT ck_workflow_defs_entry_point_schemas
        CHECK ([entry_point_schemas] IS NULL OR isjson([entry_point_schemas]) = 1);
GO
