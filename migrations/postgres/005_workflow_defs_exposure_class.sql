-- cleat#1986. Every workflow definition gets an exposure class, one of a
-- closed set of three: 'auth' (the default -- reachable from the HTTP API,
-- authentication required, today's behaviour), 'internal' (not reachable from
-- the external HTTP surface at all), or 'public' (reachable without
-- authentication, gated separately on an operator opt-in).
--
-- DEFAULT 'auth' on an ADD COLUMN backfills every existing row to the
-- currently-secure behaviour -- a deployment that has never heard of exposure
-- classes keeps its workflows exactly as reachable as they are today. See
-- cleat-internal/per-tenant-middleware-design-2026-09-18.md's revision for why
-- this is a class on the definition rather than per-tenant middleware
-- selection.
ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS exposure TEXT NOT NULL DEFAULT 'auth';

-- A CHECK, not an enum type: an enum requires ALTER TYPE ... ADD VALUE outside
-- a transaction block on older PostgreSQL, which this migration runner's
-- per-file transaction (migration/runner.go) cannot do. Postgres has no
-- `ADD CONSTRAINT IF NOT EXISTS`, so the guard is a DO block over pg_constraint
-- -- the same shape plugins/webhookingest/migrations.go uses for MySQL/MSSQL's
-- missing conditional DDL, applied here because Postgres's own gap is in a
-- different place (constraints, not columns).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'ck_workflow_defs_exposure'
    ) THEN
        ALTER TABLE workflow_defs
            ADD CONSTRAINT ck_workflow_defs_exposure
            CHECK (exposure IN ('public', 'auth', 'internal'));
    END IF;
END;
$$;
