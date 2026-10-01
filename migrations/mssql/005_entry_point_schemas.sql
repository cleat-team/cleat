-- cleat#1980: typed invocation, part 1. See migrations/postgres/006 for the
-- full rationale; this is the same column on the MSSQL dialect. nvarchar(max)
-- is this table's existing spelling for an untyped JSON document (dag_spec,
-- plugin_deps), since SQL Server has no native JSON column type.
ALTER TABLE dbo.workflow_defs ADD entry_point_schemas nvarchar(max) NULL;
