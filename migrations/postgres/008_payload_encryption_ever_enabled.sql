-- cleat#2324: a worker started WITHOUT --encrypt-sensitive-payloads has no
-- way to recognise a sealed column as ciphertext. engine/encryption.go
-- documents that this is deliberate -- no envelope, no version prefix, see
-- its own doc comment ("The transition needs no envelope") for why one would
-- be strictly worse: a version prefix can always be produced by a random
-- nonce, so it would make an OLD, correctly-sealed row unreadable the first
-- time a false positive fired. So a keyless worker cannot tell a sealed
-- column from plaintext by INSPECTING it.
--
-- This table lets it ask a different, answerable question instead: has ANY
-- worker on this database EVER had a key ring configured. If so, a keyless
-- worker refuses to start rather than silently treating ciphertext as plain
-- data -- measured (cleat#2324): by default the checksum mismatch this
-- produces fails the run (misleadingly, but closed), and
-- --disable-checksum-verification removes that net and the run ends DONE on
-- ciphertext.
--
-- Marked the moment a worker STARTS with a key ring configured, not on the
-- first successful seal: a key present and never yet used is still an
-- operator's declared intent to encrypt, and the failure direction a
-- security check should err toward is "refuses a deploy that turns out to be
-- safe" rather than "misses the window before the first write".
--
-- One possible row, by construction: `singleton` can only ever be `true`, so
-- every caller's question -- "has this ever been marked" -- is the same
-- question as "is this table non-empty", and marking twice is a no-op rather
-- than a constraint violation.
CREATE TABLE IF NOT EXISTS payload_encryption_ever_enabled (
    singleton  BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    enabled_at TIMESTAMPTZ NOT NULL
);

-- EXPLICIT, not left to ALTER DEFAULT PRIVILEGES (001_schema.sql's
-- end-of-file blanket grant for cleat_app): this is the first migration after
-- 001 to create a whole new table rather than alter an existing one, so
-- there is no precedent in this tree for trusting the default-privilege path
-- for a table this load-bearing. No DELETE: a row here is never removed, it
-- is one worker's key ring declaring itself, once.
GRANT SELECT, INSERT ON TABLE payload_encryption_ever_enabled TO cleat_app;
