-- cleat migration 094 (mysql): an org suspension column with no reader
--
-- cleat#1943. See
-- migrations/postgres/095_an_org_suspension_column_with_no_reader.sql for the
-- full reasoning: the `suspended` column on orgs has no reader and no writer
-- (org suspension was never built), and it contradicts the org table's
-- "identity only" design. Drop it so the table is what its own comment claims.

ALTER TABLE orgs DROP COLUMN suspended;
