-- cleat#3552 (B2b-1): an operator key can be narrowed to a scope, and carries the
-- name of the principal it was issued to. See migrations/postgres/004 for the
-- full reasoning; this is the same change.
--
-- MySQL DDL is not transactional and has no ADD COLUMN IF NOT EXISTS, so both
-- columns go in ONE ALTER TABLE: a single statement is atomic here, which is what
-- leaves no half-applied state for a re-run to trip over.
ALTER TABLE operator_api_keys
  ADD COLUMN scopes VARCHAR(255) NOT NULL DEFAULT 'admin',
  ADD COLUMN principal VARCHAR(128) NULL;
