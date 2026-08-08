-- cleat migration 022 (postgres): misfire and overlap policy
--
-- Two questions a scheduled-operations primitive has to answer, and that
-- cleat answered implicitly and differently in different places until now.
--
-- MISFIRE: what a firing missed during an outage means.
--
-- The engine promises at-least-once delivery, so the default is to deliver the
-- backlog: the scheduler advances one interval per tick until it catches up.
-- That cannot be unbounded -- a per-minute schedule down for a day owes 1440
-- firings, and delivering them as fast as the poll loop turns is a
-- self-inflicted stampede against whatever those workflows touch. catch_up_limit
-- is where that stops; beyond it the scheduler resumes at the next future
-- instant and LOGS how many it abandoned, because a silently dropped firing is
-- the exact thing an at-least-once promise is supposed to make impossible.
--
-- 'skip' is for schedules where a late firing is worse than no firing -- a
-- "send the 09:00 digest" job has nothing useful to say at 14:00.
--
-- OVERLAP: what happens when an instant arrives and the previous run from this
-- schedule has not finished.
--
-- The default is 'allow', which is what the scheduler has always done, because
-- changing it would silently alter existing deployments. It is the wrong
-- default for most real schedules and worth knowing about: a job that
-- occasionally takes longer than its interval quietly becomes an unbounded
-- fan-out under 'allow'. 'skip' is the safe choice for anything doing real work.
--
-- last_run_id is what makes 'skip' answerable. Without it there is no way to
-- tell a run this schedule started from any other run of the same definition.

ALTER TABLE workflow_schedules
    ADD COLUMN IF NOT EXISTS misfire_policy TEXT NOT NULL DEFAULT 'catch_up',
    ADD COLUMN IF NOT EXISTS catch_up_limit INTEGER NOT NULL DEFAULT 60,
    ADD COLUMN IF NOT EXISTS overlap_policy TEXT NOT NULL DEFAULT 'allow',
    ADD COLUMN IF NOT EXISTS last_run_id TEXT;

-- Constraints rather than application-only validation: these columns are read
-- by a background loop that has nobody to report a bad value to, so a value it
-- cannot interpret must be impossible to store rather than handled at 03:00.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_schedules_misfire_policy') THEN
        ALTER TABLE workflow_schedules ADD CONSTRAINT ck_schedules_misfire_policy
            CHECK (misfire_policy IN ('catch_up', 'skip'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_schedules_overlap_policy') THEN
        ALTER TABLE workflow_schedules ADD CONSTRAINT ck_schedules_overlap_policy
            CHECK (overlap_policy IN ('allow', 'skip'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_schedules_catch_up_limit') THEN
        ALTER TABLE workflow_schedules ADD CONSTRAINT ck_schedules_catch_up_limit
            CHECK (catch_up_limit >= 0);
    END IF;
END $$;
