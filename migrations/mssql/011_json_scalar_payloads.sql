-- cleat migration 011 (mssql): accept JSON scalars in payload columns
--
-- Bug: ISJSON(expression), with no second argument, returns 1 only for a JSON
-- object or array. A JSON scalar returns 0:
--
--     ISJSON('"payload-1"') = 0
--     ISJSON('{}')          = 1
--
-- IMPROVEMENT-PLAN 2.60c made all three stores encode a non-JSON signal
-- payload with json.Marshal, which turns `payload-1` into the scalar
-- "payload-1". PostgreSQL's JSONB and MySQL's JSON both accept a scalar, so
-- that fix is right on those two -- and the value it produces is exactly what
-- these two CHECK constraints refuse. So DeliverSignal and CreateUpdateRequest
-- failed on any SQL Server built from 001_schema.sql, and 2.60c was two-thirds
-- fixed. engine/testutil's MSSQL schema declares no CHECK constraint on either
-- column, which is why the suite showed nothing. IMPROVEMENT-PLAN 3.18.
--
-- Fix: ISJSON(payload, VALUE), which accepts any valid JSON value including
-- scalars -- the same set PostgreSQL and MySQL accept through their column
-- types. The constraint keeps doing its job; it stops disagreeing with the
-- other two dialects about what JSON is.
--
-- VERSION FLOOR. The json_type_constraint argument requires SQL Server 2022.
-- README.md and docs/reference/database-backends.md previously said 2017+ and
-- now say 2022+; this migration is why. That claim is also now the one CI
-- tests, which the old one never was -- multi-db-ci.yml has only ever run
-- 2022, so 2017 and 2019 support was asserted and never exercised. A server
-- older than 2022 fails this migration with
--
--     Incorrect syntax near 'VALUE'
--
-- which is a clearer answer than the silent rejection of every signal it
-- replaces.

IF EXISTS (
    SELECT 1 FROM sys.check_constraints
    WHERE name = N'ck_workflow_signals_payload'
      AND parent_object_id = OBJECT_ID(N'dbo.workflow_signals')
)
    ALTER TABLE dbo.workflow_signals DROP CONSTRAINT ck_workflow_signals_payload;
GO

ALTER TABLE dbo.workflow_signals
    ADD CONSTRAINT ck_workflow_signals_payload CHECK (ISJSON(payload, VALUE) = 1);
GO

IF EXISTS (
    SELECT 1 FROM sys.check_constraints
    WHERE name = N'ck_workflow_update_requests_payload'
      AND parent_object_id = OBJECT_ID(N'dbo.workflow_update_requests')
)
    ALTER TABLE dbo.workflow_update_requests DROP CONSTRAINT ck_workflow_update_requests_payload;
GO

ALTER TABLE dbo.workflow_update_requests
    ADD CONSTRAINT ck_workflow_update_requests_payload CHECK (ISJSON(payload, VALUE) = 1);
GO
