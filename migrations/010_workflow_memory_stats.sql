-- Migration 010: workflow memory statistics for memory-aware concurrency control.
--
-- Two-table design:
--   1. workflow_memory_samples  – individual execution samples (rolling window).
--      Used by the dashboard API to compute min/avg/max/deciles via
--      PostgreSQL percentile_cont().
--   2. workflow_memory_stats     – single EWMA row per def_name for fast
--      in-process estimates ("should I claim this workflow?").

-- Individual memory samples per workflow execution (rolling window).
CREATE TABLE IF NOT EXISTS workflow_memory_samples (
    id            BIGSERIAL PRIMARY KEY,
    def_name      TEXT NOT NULL,
    sample_bytes  BIGINT NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_mem_samples_def
    ON workflow_memory_samples (def_name, recorded_at DESC);

-- EWMA summary for fast in-process estimates.
-- Updated on each sample insert via ON CONFLICT DO UPDATE.
-- alpha = 0.3 gives roughly a 10-sample effective window.
CREATE TABLE IF NOT EXISTS workflow_memory_stats (
    def_name      TEXT PRIMARY KEY,
    mean_bytes    DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_count  INTEGER NOT NULL DEFAULT 0,
    alpha         DOUBLE PRECISION NOT NULL DEFAULT 0.3,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
