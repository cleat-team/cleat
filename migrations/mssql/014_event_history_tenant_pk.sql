-- cleat#2059 (tail): realign event_history's clustered primary key to
-- (tenant_id, workflow_id, step), matching PostgreSQL's already-realigned,
-- hash-partitioned shape (migrations/postgres/001_schema.sql). See
-- cleat#2059's implementer check for the production queries (in
-- engine/mssql_events.go, engine/mssql_operations.go, engine/mssql_signals_promises.go,
-- engine/mssql_schedules.go) that relied on (workflow_id, step) alone and
-- needed a tenant_id predicate added -- compared directly against a bound
-- parameter, not against another table's column, to satisfy
-- TestMSSQLTenantScopedTablesAreQueriedWithATenantPredicate -- to keep
-- their seek path under this key.
--
-- Guarded like 011_promise_seq_closes_the_mid_segment_wake_gap.sql: a crash
-- between one statement and the runner's schema_migrations write must not
-- fail a re-run of this file, so every step below checks the CURRENT state
-- rather than assuming a fresh database.

-- 1. Realign the clustered primary key. Checked by whether pk_event_history
-- already leads with tenant_id, not merely whether the constraint exists --
-- SQL Server has no "change a clustered index's key order" statement, only
-- drop-and-recreate (which rebuilds the whole table), and this must not
-- re-run that against a key already in the target shape.
IF EXISTS (
    SELECT 1 FROM sys.key_constraints
    WHERE name = 'pk_event_history' AND type = 'PK'
      AND parent_object_id = OBJECT_ID('dbo.event_history')
)
AND NOT EXISTS (
    SELECT 1
    FROM sys.key_constraints kc
    JOIN sys.index_columns ic
      ON ic.object_id = kc.parent_object_id AND ic.index_id = kc.unique_index_id
    JOIN sys.columns c
      ON c.object_id = ic.object_id AND c.column_id = ic.column_id
    WHERE kc.name = 'pk_event_history' AND ic.key_ordinal = 1 AND c.name = 'tenant_id'
)
BEGIN
    ALTER TABLE dbo.event_history DROP CONSTRAINT pk_event_history;
    ALTER TABLE dbo.event_history ADD CONSTRAINT pk_event_history PRIMARY KEY CLUSTERED (tenant_id, workflow_id, step);
END
GO

-- 2. idx_event_history_tenant_wf is now redundant with the clustered
-- primary key above (same three columns, same order).
IF EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE name = 'idx_event_history_tenant_wf' AND object_id = OBJECT_ID('dbo.event_history')
)
    DROP INDEX idx_event_history_tenant_wf ON dbo.event_history;
GO

-- 3. idx_event_history_workflow_fk -- cleat#2059/#2060. Added, not dropped:
-- fk_event_history_workflow's ON DELETE CASCADE validates purely by
-- workflow_id, with no knowledge of tenant_id, so it cannot use
-- pk_event_history's seek path now that tenant_id leads that key. Without
-- this index, deleting a workflow_instances row forces SQL Server to scan
-- for matching event_history rows to rule out cascading, and that scan
-- walks the clustered index from the tenant's first row -- which, for a
-- tenant with thousands of OTHER workflows' events still present (the
-- normal case for a small, interleaved retention-sweep chunk: see
-- mssqlInterleaveChunk's own comment in engine/mssql_schedules.go), crosses
-- SQL Server's ~5,000-lock escalation threshold on every single chunk.
-- Confirmed directly: TestMSSQLRetentionSweepsCauseNoLockEscalation's
-- DeleteCompletedWorkflows and DeleteDeadLetteredWorkflows arms regressed
-- from 0 to ~300 lock escalations (one per mssqlInterleaveChunk=20-sized
-- chunk-pair, 6,000/20) the moment the clustered key gained tenant_id as
-- its leading column -- traced to this exact statement via an Extended
-- Events lock_escalation session, not inferred -- and this index took the
-- measured delta back to 0 across a 2,000-workflow drain.
IF NOT EXISTS (
    SELECT 1 FROM sys.indexes
    WHERE name = 'idx_event_history_workflow_fk' AND object_id = OBJECT_ID('dbo.event_history')
)
    CREATE NONCLUSTERED INDEX idx_event_history_workflow_fk ON dbo.event_history (workflow_id, step);
GO
