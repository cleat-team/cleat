-- Migration 010: workflow memory statistics for memory-aware concurrency control
-- (T-SQL for SQL Server 2017+ / Azure SQL Database)
--
-- Two-table design:
--   1. workflow_memory_samples  -- individual execution samples (rolling window).
--      Used by the dashboard API to compute min/avg/max/deciles.
--   2. workflow_memory_stats     -- single EWMA row per def_name for fast
--      in-process estimates ("should I claim this workflow?").

-- ===========================================================================
-- Table: workflow_memory_samples
-- Individual memory samples per workflow execution (rolling window).
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_memory_samples') AND type = N'U')
CREATE TABLE dbo.workflow_memory_samples (
    id            BIGINT IDENTITY(1,1) NOT NULL,
    def_name      NVARCHAR(255)        NOT NULL,
    sample_bytes  BIGINT               NOT NULL,
    recorded_at   DATETIMEOFFSET       NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_workflow_memory_samples PRIMARY KEY (id)
);

IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_mem_samples_def' AND object_id = OBJECT_ID(N'dbo.workflow_memory_samples'))
    CREATE INDEX idx_mem_samples_def ON dbo.workflow_memory_samples (def_name, recorded_at DESC);

-- ===========================================================================
-- Table: workflow_memory_stats
-- EWMA summary for fast in-process estimates.
-- Updated on each sample insert via MERGE / UPDATE.
-- alpha = 0.3 gives roughly a 10-sample effective window.
-- ===========================================================================
IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE object_id = OBJECT_ID(N'dbo.workflow_memory_stats') AND type = N'U')
CREATE TABLE dbo.workflow_memory_stats (
    def_name      NVARCHAR(255)  NOT NULL,
    mean_bytes    FLOAT(53)      NOT NULL DEFAULT 0,
    sample_count  INT            NOT NULL DEFAULT 0,
    alpha         FLOAT(53)      NOT NULL DEFAULT 0.3,
    updated_at    DATETIMEOFFSET NOT NULL DEFAULT SYSUTCDATETIME(),
    CONSTRAINT pk_workflow_memory_stats PRIMARY KEY (def_name)
);
