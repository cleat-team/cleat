-- cleat#953, second half: a BURST of signals was slept through.
--
-- 046/047 added signal_seq and signal_seq_at_claim, which detect "a delivery
-- arrived DURING my segment". A burst arrives BEFORE the claim, so the stamp is
-- taken at the already-moved value, nothing changes during the segment, and
-- finalize schedules the full deadline over deliveries still sitting in
-- workflow_signals. Measured on the shipped code: three delivered, one
-- consumed, two left, wake in 60s.
--
-- A NEW FILE rather than an edit to 046/047, which are already applied. A
-- migration that has run does not run again, so a column added by editing a
-- shipped file never appears on any database that has seen it -- and the
-- symptom would be `column "signal_consumed_seq" does not exist` on every
-- claim, in production, on exactly the deployments that are up to date.
--
-- WHAT SEPARATES A BURST WORTH DRAINING FROM A SIGNAL THAT WOULD SPIN IS
-- PROGRESS. A segment that consumed something can consume again, so waking it
-- is productive and bounded by the queue depth. A segment that consumed
-- nothing has already shown it wants nothing that is there -- which is the
-- unrelated-pending-signal case that ruled out "wake if any row exists".
--
-- Cost is one wasted wake per drained burst: the segment after the last
-- consume wakes, finds nothing, consumes nothing, and sleeps. That terminates
-- because the second clause needs a consume to fire, and is asserted rather
-- than reasoned about -- the mechanism this replaces also terminated correctly
-- for a single signal, which is how it shipped.
ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS signal_consumed_seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE workflow_instances
    ADD COLUMN IF NOT EXISTS signal_consumed_at_claim BIGINT NOT NULL DEFAULT 0;
