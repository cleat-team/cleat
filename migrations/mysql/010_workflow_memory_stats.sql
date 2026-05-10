-- cleat MySQL migration 010: workflow memory statistics
-- Memory-aware concurrency control tables.
--
-- Two-table design:
--   1. workflow_memory_samples  – individual execution samples (rolling window).
--      Used by the dashboard API to compute min/avg/max/deciles.
--   2. workflow_memory_stats     – single EWMA row per def_name for fast
--      in-process estimates ("should I claim this workflow?").
--
-- MySQL differences from PostgreSQL:
--   - BIGSERIAL becomes BIGINT AUTO_INCREMENT
--   - TEXT used as PK becomes VARCHAR(255)
--   - DOUBLE PRECISION becomes DOUBLE
--   - TIMESTAMPTZ becomes TIMESTAMP(6)
--   - now() becomes NOW(6)
--   - DESC in index omitted; MySQL ASC index is sufficient for ORDER BY DESC
--   - Idempotent: CREATE TABLE IF NOT EXISTS

-- Individual memory samples per workflow execution (rolling window).
CREATE TABLE IF NOT EXISTS workflow_memory_samples (
    id                 BIGINT NOT NULL AUTO_INCREMENT,
    def_name           VARCHAR(255) NOT NULL,
    sample_bytes       BIGINT NOT NULL,
    recorded_at        TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Note: DESC omitted from index definition. MySQL ASC index serves
-- ORDER BY recorded_at DESC queries efficiently via backward index scan.
CREATE INDEX idx_mem_samples_def ON workflow_memory_samples (def_name, recorded_at);

-- EWMA summary for fast in-process estimates.
-- Updated on each sample insert via ON DUPLICATE KEY UPDATE.
-- alpha = 0.3 gives roughly a 10-sample effective window.
CREATE TABLE IF NOT EXISTS workflow_memory_stats (
    def_name           VARCHAR(255) NOT NULL,
    mean_bytes         DOUBLE NOT NULL DEFAULT 0,
    sample_count       INTEGER NOT NULL DEFAULT 0,
    alpha              DOUBLE NOT NULL DEFAULT 0.3,
    updated_at         TIMESTAMP(6) NOT NULL DEFAULT NOW(6),
    PRIMARY KEY (def_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
