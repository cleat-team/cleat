-- cleat#1980: typed invocation, part 1. See migrations/postgres/005 for the
-- full rationale; this is the same column on the MySQL dialect.
ALTER TABLE workflow_defs ADD COLUMN entry_point_schemas json DEFAULT NULL;
