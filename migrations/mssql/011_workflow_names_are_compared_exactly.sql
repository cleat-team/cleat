-- cleat#3635 (owner D60): workflow-definition names are compared EXACTLY on SQL Server, as they already
-- are on PostgreSQL. Before this, 'Acme.checkout' = 'acme.checkout' was true here: every name column
-- carried the database's case-insensitive default collation.
--
-- Binary, as 008 did for plugin_name: Latin1_General_100_BIN2. 008 CREATED a table with that collation; this
-- migration ALTERS columns that already sit under primary keys, foreign keys, indexes and a check
-- constraint, so it is a drop / alter / recreate.
--
-- THE WHOLE FILE RUNS IN ONE TRANSACTION (migration/runner.go applyMigration: every GO batch on one tx, the
-- version recorded, then Commit) and SQL Server DDL is transactional. If any batch fails, EVERYTHING rolls
-- back -- the preflight, the drops, the alters -- and no version is recorded. Nothing is left behind.
-- The price is lock time: ALTER COLUMN and the key rebuilds hold schema-modification locks on these tables
-- until commit, and workflow_defs' clustered key is (tenant_id, name, version), so converting `name` rewrites a
-- table that holds the WASM bytes. Run it in a maintenance window; see the release note for the measured size.
--
-- WHY FOREIGN KEYS FORCE A GROUP: SQL Server will not create a foreign key between columns of different
-- collations, so these move together or not at all:
--   workflow_defs.name <- workflow_instances.def_name, workflow_routing.workflow_name, workflow_tags.workflow_name
--   workflow_defs.(library, name) -> library.members.(library, name) <- library.bundle_members.(library, name)
--   library.bundle_members.library -> library.bundles.library
-- Comparing a converted column with an unconverted one in a join raises error 468, so the columns the engine
-- compares by value move too (workflow_memory_samples/stats.def_name, tenant_library_installs.library).
--
-- THE HAZARD THIS MIGRATION REFUSES TO CREATE. Today the case-insensitive foreign key accepts a child that
-- matches its parent only by case: an instance whose def_name is 'checkout' under a definition named
-- 'Checkout'. Measured on a fresh database before this was written. Once the columns are binary that child is an
-- orphan and the key cannot be recreated -- and a lookup for it would silently stop matching. The preflight
-- below finds every such row, names a few, and stops. ("Refuse on duplicate names" would be vacuous: the unique
-- keys are case-insensitive today and binary only relaxes uniqueness, so a duplicate cannot exist.)
--
-- ROW-LEVEL SECURITY HIDES THE ROWS FROM THIS SESSION. The tenant filter is `tenant_id = SESSION_CONTEXT('tenant_id')`
-- with no exemption for a privileged login: measured, `sa` (db_owner) sees 0 rows of workflow_defs without it. A
-- preflight that just counted would pass on every database. So it sets the context for each tenant in turn, and
-- every foreign key is recreated WITH CHECK, which validates ALL rows regardless of any filter, as the backstop
-- for a tenant the loop cannot see.
--
-- EVERY STORED DEFINITION NAME IS CONVERTED, not only the ones a join compares (owner D60): workflow_schedules.def_name,
-- idempotency_keys.def_name, event_history.child_name and admin.plugin_dep_overrides.definition_name carry no key,
-- index or foreign key, and converting them is metadata-only. Measured on a 1.8 GB database (1M instances, 1M
-- event_history rows with child_name set, 1M memory samples) the whole file took a median of 6.9 s over nine runs, and
-- leaving the four columns out made no difference that the run-to-run spread could resolve; the CHANGELOG entry
-- carries the figures and their limits.
--
-- WHICH STORED-ONLY COLUMNS ARE CHECKED, AND WHY (decided with the reviewer's measurement, cleat#3642):
--   * workflow_schedules.def_name: CHECKED above. It is looked up (a due schedule starts its workflow by this name).
--   * idempotency_keys.def_name: not checked. It is only selected and compared in Go with `!=`, which is exact before
--     and after; no SQL predicate reads it.
--   * event_history.child_name: not checked. It is written and read back; no SQL predicate reads it, and replay
--     compares it in Go.
--   * admin.plugin_dep_overrides.definition_name: not checked. It is an append-only AUDIT record of each use of the
--     plugin-dependency override (engine/plugin_dep_overrides.go only inserts); nothing looks it up to authorize a
--     deploy, so a case variant "stops applying" nothing. A person querying the table by name now needs the exact spelling.
--   Re-derive: git grep -n -E 'plugin_dep_overrides|child_name|idempotency_keys' -- '*.go' ':!*_test.go' and read the predicates.
--
-- THE SCHEDULE CHECK HAS NO BACKSTOP, AND THE FOUR FOREIGN-KEYED KINDS DO. Those are recreated WITH CHECK, which reads
-- every row whatever tenant it is in, so a case-only orphan anywhere refuses the migration. A schedule has no foreign
-- key, and the loop that checks it visits only the tenants listed in admin.tenants plus the default tenant. A
-- case-only schedule in a tenant with NO admin.tenants row therefore passes this migration SILENTLY and stops firing
-- afterwards. MEASURED, and pinned as a known limitation by the migration test (a case-only schedule in the unlisted
-- tenant: the migration applies, and the schedule's join to its definition is 0 afterwards); if this is ever closed,
-- that assertion flips on purpose.
--
-- NOT CONVERTED: the tables plugins create at boot (the scheduler's schedules, event_subscriptions, task_queue).
-- Core migrations cannot name a table that may not exist yet, so those def-name columns stay case-insensitive until the
-- plugin migrations convert them on their own (carried as a follow-up, not forgotten).

-- 1. Preflight: refuse on anything this migration cannot do safely. Reads the catalog and rows, changes nothing.
--
-- (a) FAIL CLOSED ON AN OBJECT THIS FILE DOES NOT KNOW ABOUT. If a deployment added an index, key, constraint
-- or schema-bound object on one of these columns, ALTER COLUMN would fail on it, or worse, the recreate below
-- would drop its definition. Name them and stop; the operator drops or recreates them deliberately.
DECLARE @cand TABLE (sch sysname COLLATE DATABASE_DEFAULT, tbl sysname COLLATE DATABASE_DEFAULT, col sysname COLLATE DATABASE_DEFAULT);
INSERT @cand VALUES
 (N'dbo',N'workflow_defs',N'name'),(N'dbo',N'workflow_defs',N'library'),
 (N'dbo',N'workflow_instances',N'def_name'),(N'dbo',N'workflow_routing',N'workflow_name'),(N'dbo',N'workflow_tags',N'workflow_name'),
 (N'dbo',N'tenant_library_installs',N'library'),(N'dbo',N'workflow_memory_samples',N'def_name'),(N'dbo',N'workflow_memory_stats',N'def_name'),
 (N'library',N'members',N'library'),(N'library',N'members',N'name'),
 (N'library',N'bundle_members',N'library'),(N'library',N'bundle_members',N'name'),(N'library',N'bundles',N'library'),
 (N'dbo',N'workflow_schedules',N'def_name'),(N'dbo',N'idempotency_keys',N'def_name'),(N'dbo',N'event_history',N'child_name'),(N'admin',N'plugin_dep_overrides',N'definition_name');
DECLARE @known TABLE (nm sysname COLLATE DATABASE_DEFAULT);
INSERT @known VALUES
 (N'pk_workflow_defs'),(N'idx_defs_active'),(N'idx_defs_library'),(N'idx_defs_tenant_name_version'),
 (N'pk_workflow_tags'),(N'pk_tenant_library_installs'),(N'pk_workflow_memory_stats'),
 (N'idx_mem_samples_def'),(N'idx_memory_samples_tenant_def'),
 (N'pk_bundle_members'),(N'pk_bundles'),(N'pk_members'),
 (N'fk_instances_def'),(N'fk_workflow_routing_def'),(N'fk_workflow_tags_def'),
 (N'fk_workflow_defs_library'),(N'fk_bundle_members_member'),(N'fk_bundle_members_bundle'),
 (N'ck_workflow_defs_bytes_or_library');
DECLARE @unknown nvarchar(max) = N'';
SELECT @unknown = @unknown + CHAR(10) + N'  ' + x.what
FROM (
  -- indexes, keys and unique constraints containing a candidate column
  SELECT DISTINCT N'index/key ' + CAST(i.name AS nvarchar(200)) COLLATE DATABASE_DEFAULT + N' on ' + c.sch + N'.' + c.tbl AS what
  FROM @cand c
  JOIN sys.tables t ON t.name = c.tbl COLLATE DATABASE_DEFAULT AND SCHEMA_NAME(t.schema_id) = c.sch COLLATE DATABASE_DEFAULT
  JOIN sys.columns col ON col.object_id = t.object_id AND col.name = c.col COLLATE DATABASE_DEFAULT
  JOIN sys.index_columns ic ON ic.object_id = t.object_id AND ic.column_id = col.column_id
  JOIN sys.indexes i ON i.object_id = t.object_id AND i.index_id = ic.index_id
  WHERE i.name IS NOT NULL AND i.name COLLATE DATABASE_DEFAULT NOT IN (SELECT nm FROM @known)
  UNION
  -- foreign keys on either side of a candidate column
  SELECT DISTINCT N'foreign key ' + CAST(fk.name AS nvarchar(200)) COLLATE DATABASE_DEFAULT + N' on ' + c.sch + N'.' + c.tbl
  FROM @cand c
  JOIN sys.tables t ON t.name = c.tbl COLLATE DATABASE_DEFAULT AND SCHEMA_NAME(t.schema_id) = c.sch COLLATE DATABASE_DEFAULT
  JOIN sys.columns col ON col.object_id = t.object_id AND col.name = c.col COLLATE DATABASE_DEFAULT
  JOIN sys.foreign_key_columns fkc ON (fkc.parent_object_id = t.object_id AND fkc.parent_column_id = col.column_id)
                                   OR (fkc.referenced_object_id = t.object_id AND fkc.referenced_column_id = col.column_id)
  JOIN sys.foreign_keys fk ON fk.object_id = fkc.constraint_object_id
  WHERE fk.name COLLATE DATABASE_DEFAULT NOT IN (SELECT nm FROM @known)
  UNION
  -- check constraints that name a candidate column
  SELECT DISTINCT N'check constraint ' + CAST(cc.name AS nvarchar(200)) COLLATE DATABASE_DEFAULT + N' on ' + c.sch + N'.' + c.tbl
  FROM @cand c
  JOIN sys.tables t ON t.name = c.tbl COLLATE DATABASE_DEFAULT AND SCHEMA_NAME(t.schema_id) = c.sch COLLATE DATABASE_DEFAULT
  JOIN sys.check_constraints cc ON cc.parent_object_id = t.object_id AND CHARINDEX(N'[' + c.col COLLATE DATABASE_DEFAULT + N']', cc.definition) > 0
  WHERE cc.name COLLATE DATABASE_DEFAULT NOT IN (SELECT nm FROM @known)
  UNION
  -- computed columns, and views / functions / policies bound to a candidate column
  SELECT DISTINCT N'schema-bound ' + CAST(o.type_desc AS nvarchar(60)) COLLATE DATABASE_DEFAULT + N' ' + CAST(o.name AS nvarchar(200)) COLLATE DATABASE_DEFAULT + N' on ' + c.sch + N'.' + c.tbl
  FROM @cand c
  JOIN sys.tables t ON t.name = c.tbl COLLATE DATABASE_DEFAULT AND SCHEMA_NAME(t.schema_id) = c.sch COLLATE DATABASE_DEFAULT
  JOIN sys.columns col ON col.object_id = t.object_id AND col.name = c.col COLLATE DATABASE_DEFAULT
  JOIN sys.sql_expression_dependencies d ON d.referenced_id = t.object_id AND d.referenced_minor_id = col.column_id AND d.is_schema_bound_reference = 1
  JOIN sys.objects o ON o.object_id = d.referencing_id
  WHERE o.name COLLATE DATABASE_DEFAULT NOT IN (SELECT nm FROM @known)
  UNION
  -- user-created statistics (the automatic ones are dropped by ALTER COLUMN)
  SELECT DISTINCT N'statistics ' + CAST(s.name AS nvarchar(200)) COLLATE DATABASE_DEFAULT + N' on ' + c.sch + N'.' + c.tbl
  FROM @cand c
  JOIN sys.tables t ON t.name = c.tbl COLLATE DATABASE_DEFAULT AND SCHEMA_NAME(t.schema_id) = c.sch COLLATE DATABASE_DEFAULT
  JOIN sys.columns col ON col.object_id = t.object_id AND col.name = c.col COLLATE DATABASE_DEFAULT
  JOIN sys.stats_columns sc ON sc.object_id = t.object_id AND sc.column_id = col.column_id
  JOIN sys.stats s ON s.object_id = t.object_id AND s.stats_id = sc.stats_id AND s.user_created = 1
) x;
IF LEN(@unknown) > 0
BEGIN
  DECLARE @m1 nvarchar(max) = N'cleat#3635: workflow names become exact (binary), but these objects depend on the columns and are not part of the shipped schema, so the migration cannot rebuild them without losing their definition. Drop or recreate them deliberately, then run the migration again:' + @unknown;
  THROW 51350, @m1, 1;
END
GO

-- Every column used to BUILD a message below carries COLLATE DATABASE_DEFAULT. This file can run over a database it
-- has already converted (--migrate-only repairing a missing tracking row), where `library` is binary and
-- `bundle_version` is not; two different implicit collations in one expression are error 4191, which would
-- replace the readable refusal with an opaque failure.
-- (b) CASE-ONLY ORPHANS. A child row that matches its parent only by case. Under the case-insensitive collation
-- the foreign key accepted it; under the binary one it is an orphan. Checked per tenant because row-level
-- security shows this session nothing without the tenant context (see the header).
DECLARE @t uniqueidentifier, @report nvarchar(max) = N'', @found int, @ex nvarchar(max);
DECLARE @BIN nvarchar(40) = N'Latin1_General_100_BIN2';
DECLARE tenants CURSOR LOCAL FAST_FORWARD FOR
  SELECT tenant_id FROM admin.tenants UNION SELECT CAST('00000000-0000-0000-0000-000000000000' AS uniqueidentifier);
BEGIN TRY
  OPEN tenants;
  FETCH NEXT FROM tenants INTO @t;
  WHILE @@FETCH_STATUS = 0
  BEGIN
    EXEC sp_set_session_context N'tenant_id', @t;

    SELECT @found = COUNT(*), @ex = STRING_AGG(CAST(e.nm AS nvarchar(max)), N', ')
    FROM (SELECT TOP (3) N'''' + c.def_name COLLATE DATABASE_DEFAULT + N'''@' + CAST(c.def_version AS nvarchar(12)) AS nm FROM dbo.workflow_instances c
          WHERE NOT EXISTS (SELECT 1 FROM dbo.workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.version = c.def_version
                              AND p.name COLLATE Latin1_General_100_BIN2 = c.def_name COLLATE Latin1_General_100_BIN2)) e;
    IF @found > 0 SET @report += CHAR(10) + N'  workflow_instances.def_name -> workflow_defs.name, tenant ' + CAST(@t AS nvarchar(40)) + N': ' + @ex;

    SELECT @found = COUNT(*), @ex = STRING_AGG(CAST(e.nm AS nvarchar(max)), N', ')
    FROM (SELECT TOP (3) N'''' + c.workflow_name COLLATE DATABASE_DEFAULT + N'''@' + CAST(c.target_version AS nvarchar(12)) AS nm FROM dbo.workflow_routing c
          WHERE NOT EXISTS (SELECT 1 FROM dbo.workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.version = c.target_version
                              AND p.name COLLATE Latin1_General_100_BIN2 = c.workflow_name COLLATE Latin1_General_100_BIN2)) e;
    IF @found > 0 SET @report += CHAR(10) + N'  workflow_routing.workflow_name -> workflow_defs.name, tenant ' + CAST(@t AS nvarchar(40)) + N': ' + @ex;

    SELECT @found = COUNT(*), @ex = STRING_AGG(CAST(e.nm AS nvarchar(max)), N', ')
    FROM (SELECT TOP (3) N'''' + c.workflow_name COLLATE DATABASE_DEFAULT + N'''@' + CAST(c.version AS nvarchar(12)) AS nm FROM dbo.workflow_tags c
          WHERE NOT EXISTS (SELECT 1 FROM dbo.workflow_defs p WHERE p.tenant_id = c.tenant_id AND p.version = c.version
                              AND p.name COLLATE Latin1_General_100_BIN2 = c.workflow_name COLLATE Latin1_General_100_BIN2)) e;
    IF @found > 0 SET @report += CHAR(10) + N'  workflow_tags.workflow_name -> workflow_defs.name, tenant ' + CAST(@t AS nvarchar(40)) + N': ' + @ex;

    -- a definition that names a library member (library IS NULL is a plain definition: the key ignores it)
    SELECT @found = COUNT(*), @ex = STRING_AGG(CAST(e.nm AS nvarchar(max)), N', ')
    FROM (SELECT TOP (3) N'''' + c.library COLLATE DATABASE_DEFAULT + N'''/''' + c.name COLLATE DATABASE_DEFAULT + N'''@' + CAST(c.version AS nvarchar(12)) AS nm FROM dbo.workflow_defs c
          WHERE c.library IS NOT NULL AND NOT EXISTS (SELECT 1 FROM library.members m WHERE m.version = c.version
                              AND m.library COLLATE Latin1_General_100_BIN2 = c.library COLLATE Latin1_General_100_BIN2
                              AND m.name COLLATE Latin1_General_100_BIN2 = c.name COLLATE Latin1_General_100_BIN2)) e;
    IF @found > 0 SET @report += CHAR(10) + N'  workflow_defs.(library, name) -> library.members.(library, name), tenant ' + CAST(@t AS nvarchar(40)) + N': ' + @ex;

    -- A STORED-ONLY column that is still LOOKED UP: a schedule starts its workflow by the def_name it stores, which
    -- handleCreateSchedule keeps exactly as the caller typed it and never checks against a definition. A schedule
    -- for 'checkout' under a definition named 'Checkout' works today and fails at every tick once the lookup is
    -- exact (cleat-review-69 measured the join: 1 before, 0 after, with this migration reporting success). Refuse a
    -- row that matches a definition the way the OLD collation compares (the database default, forced so a re-run over
    -- converted columns still sees it) and matches none exactly. A schedule that names no definition in ANY case was
    -- already broken and is not this migration's to refuse.
    -- Through a narrow temp table of this tenant's definition NAMES, indexed both ways: the first version of this
    -- check compared workflow_schedules to the wide workflow_defs rows directly, the optimiser chose a scan of
    -- workflow_defs per schedule row, and the file took 105 to 140 s instead of about 6 on 100,000 schedules. A seek per
    -- schedule row is what makes it cheap.
    IF OBJECT_ID(N'tempdb..#defnames') IS NOT NULL DROP TABLE #defnames;
    CREATE TABLE #defnames (exact_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL PRIMARY KEY,
                            loose_name nvarchar(255) COLLATE DATABASE_DEFAULT NOT NULL);
    INSERT #defnames (exact_name, loose_name) SELECT DISTINCT name, name FROM dbo.workflow_defs WHERE tenant_id = @t;
    CREATE INDEX ix_defnames_loose ON #defnames (loose_name);
    SELECT @found = COUNT(*), @ex = STRING_AGG(CAST(e.nm AS nvarchar(max)), N', ')
    FROM (SELECT TOP (3) N'''' + c.def_name COLLATE DATABASE_DEFAULT + N''' (schedule ' + c.name COLLATE DATABASE_DEFAULT + N')' AS nm FROM dbo.workflow_schedules c
          WHERE c.tenant_id = @t
            AND EXISTS (SELECT 1 FROM #defnames d WHERE d.loose_name = c.def_name COLLATE DATABASE_DEFAULT)
            AND NOT EXISTS (SELECT 1 FROM #defnames d WHERE d.exact_name = c.def_name COLLATE Latin1_General_100_BIN2)) e;
    DROP TABLE #defnames;
    IF @found > 0 SET @report += CHAR(10) + N'  workflow_schedules.def_name -> workflow_defs.name, tenant ' + CAST(@t AS nvarchar(40)) + N': ' + @ex;

    FETCH NEXT FROM tenants INTO @t;
  END
  CLOSE tenants; DEALLOCATE tenants;
  EXEC sp_set_session_context N'tenant_id', NULL;
END TRY
BEGIN CATCH
  EXEC sp_set_session_context N'tenant_id', NULL;
  THROW;
END CATCH

-- The library tables are not tenant-scoped: no context is needed, and they are checked once.
SELECT @found = COUNT(*), @ex = STRING_AGG(CAST(e.nm AS nvarchar(max)), N', ')
FROM (SELECT TOP (3) N'''' + c.library COLLATE DATABASE_DEFAULT + N'''/''' + c.name COLLATE DATABASE_DEFAULT + N'''@' + CAST(c.version AS nvarchar(12)) AS nm FROM library.bundle_members c
      WHERE NOT EXISTS (SELECT 1 FROM library.members m WHERE m.version = c.version
                          AND m.library COLLATE Latin1_General_100_BIN2 = c.library COLLATE Latin1_General_100_BIN2
                          AND m.name COLLATE Latin1_General_100_BIN2 = c.name COLLATE Latin1_General_100_BIN2)) e;
IF @found > 0 SET @report += CHAR(10) + N'  library.bundle_members.(library, name) -> library.members.(library, name): ' + @ex;

SELECT @found = COUNT(*), @ex = STRING_AGG(CAST(e.nm AS nvarchar(max)), N', ')
FROM (SELECT TOP (3) N'''' + c.library COLLATE DATABASE_DEFAULT + N'''@' + c.bundle_version COLLATE DATABASE_DEFAULT AS nm FROM library.bundle_members c
      WHERE NOT EXISTS (SELECT 1 FROM library.bundles b WHERE b.bundle_version = c.bundle_version
                          AND b.library COLLATE Latin1_General_100_BIN2 = c.library COLLATE Latin1_General_100_BIN2)) e;
IF @found > 0 SET @report += CHAR(10) + N'  library.bundle_members.library -> library.bundles.library: ' + @ex;

IF LEN(@report) > 0
BEGIN
  DECLARE @m2 nvarchar(max) = N'cleat#3635: workflow names become exact (binary). These rows match their parent only by case, which the old case-insensitive keys allowed and the new ones do not (first three per kind and tenant):' + @report
    + CHAR(10) + N'Rename each child to the parent''s exact spelling (or the parent to the child''s), then run the migration again. Nothing was changed.';
  THROW 51351, @m2, 1;
END
GO

-- 2. Drop what depends on the columns: foreign keys first (they reference the keys below), then the check
-- constraint, then the indexes and keys. Each is guarded, so a database that already lost one is still handled.
IF OBJECT_ID(N'dbo.fk_instances_def', N'F') IS NOT NULL ALTER TABLE dbo.workflow_instances DROP CONSTRAINT fk_instances_def;
IF OBJECT_ID(N'dbo.fk_workflow_routing_def', N'F') IS NOT NULL ALTER TABLE dbo.workflow_routing DROP CONSTRAINT fk_workflow_routing_def;
IF OBJECT_ID(N'dbo.fk_workflow_tags_def', N'F') IS NOT NULL ALTER TABLE dbo.workflow_tags DROP CONSTRAINT fk_workflow_tags_def;
IF OBJECT_ID(N'dbo.fk_workflow_defs_library', N'F') IS NOT NULL ALTER TABLE dbo.workflow_defs DROP CONSTRAINT fk_workflow_defs_library;
IF OBJECT_ID(N'library.fk_bundle_members_member', N'F') IS NOT NULL ALTER TABLE library.bundle_members DROP CONSTRAINT fk_bundle_members_member;
IF OBJECT_ID(N'library.fk_bundle_members_bundle', N'F') IS NOT NULL ALTER TABLE library.bundle_members DROP CONSTRAINT fk_bundle_members_bundle;
IF OBJECT_ID(N'dbo.ck_workflow_defs_bytes_or_library', N'C') IS NOT NULL ALTER TABLE dbo.workflow_defs DROP CONSTRAINT ck_workflow_defs_bytes_or_library;
GO
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_active' AND object_id = OBJECT_ID(N'dbo.workflow_defs')) DROP INDEX idx_defs_active ON dbo.workflow_defs;
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_library' AND object_id = OBJECT_ID(N'dbo.workflow_defs')) DROP INDEX idx_defs_library ON dbo.workflow_defs;
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_defs_tenant_name_version' AND object_id = OBJECT_ID(N'dbo.workflow_defs')) DROP INDEX idx_defs_tenant_name_version ON dbo.workflow_defs;
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_mem_samples_def' AND object_id = OBJECT_ID(N'dbo.workflow_memory_samples')) DROP INDEX idx_mem_samples_def ON dbo.workflow_memory_samples;
IF EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'idx_memory_samples_tenant_def' AND object_id = OBJECT_ID(N'dbo.workflow_memory_samples')) DROP INDEX idx_memory_samples_tenant_def ON dbo.workflow_memory_samples;
IF OBJECT_ID(N'dbo.pk_workflow_defs', N'PK') IS NOT NULL ALTER TABLE dbo.workflow_defs DROP CONSTRAINT pk_workflow_defs;
IF OBJECT_ID(N'dbo.pk_workflow_tags', N'PK') IS NOT NULL ALTER TABLE dbo.workflow_tags DROP CONSTRAINT pk_workflow_tags;
IF OBJECT_ID(N'dbo.pk_tenant_library_installs', N'PK') IS NOT NULL ALTER TABLE dbo.tenant_library_installs DROP CONSTRAINT pk_tenant_library_installs;
IF OBJECT_ID(N'dbo.pk_workflow_memory_stats', N'PK') IS NOT NULL ALTER TABLE dbo.workflow_memory_stats DROP CONSTRAINT pk_workflow_memory_stats;
IF OBJECT_ID(N'library.pk_bundle_members', N'PK') IS NOT NULL ALTER TABLE library.bundle_members DROP CONSTRAINT pk_bundle_members;
IF OBJECT_ID(N'library.pk_bundles', N'PK') IS NOT NULL ALTER TABLE library.bundles DROP CONSTRAINT pk_bundles;
IF OBJECT_ID(N'library.pk_members', N'PK') IS NOT NULL ALTER TABLE library.members DROP CONSTRAINT pk_members;
GO

-- 3. The columns. Type, length and nullability are exactly what the catalog held when this was written; only
-- the collation changes.
ALTER TABLE dbo.workflow_defs ALTER COLUMN name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.workflow_defs ALTER COLUMN library nvarchar(128) COLLATE Latin1_General_100_BIN2 NULL;
ALTER TABLE dbo.workflow_instances ALTER COLUMN def_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.workflow_routing ALTER COLUMN workflow_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.workflow_tags ALTER COLUMN workflow_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.tenant_library_installs ALTER COLUMN library nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.workflow_memory_samples ALTER COLUMN def_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.workflow_memory_stats ALTER COLUMN def_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE library.members ALTER COLUMN library nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE library.members ALTER COLUMN name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE library.bundle_members ALTER COLUMN library nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE library.bundle_members ALTER COLUMN name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE library.bundles ALTER COLUMN library nvarchar(128) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.workflow_schedules ALTER COLUMN def_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
ALTER TABLE dbo.idempotency_keys ALTER COLUMN def_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NULL;
ALTER TABLE dbo.event_history ALTER COLUMN child_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NULL;
ALTER TABLE admin.plugin_dep_overrides ALTER COLUMN definition_name nvarchar(255) COLLATE Latin1_General_100_BIN2 NOT NULL;
GO

-- 4. Put back what was dropped, definition for definition (clustered keys first, so each table is rebuilt once).
ALTER TABLE library.members ADD CONSTRAINT pk_members PRIMARY KEY CLUSTERED (library, name, version);
ALTER TABLE library.bundles ADD CONSTRAINT pk_bundles PRIMARY KEY CLUSTERED (library, bundle_version);
ALTER TABLE library.bundle_members ADD CONSTRAINT pk_bundle_members PRIMARY KEY NONCLUSTERED (library, bundle_version, name);
ALTER TABLE dbo.workflow_defs ADD CONSTRAINT pk_workflow_defs PRIMARY KEY CLUSTERED (tenant_id, name, version);
ALTER TABLE dbo.workflow_tags ADD CONSTRAINT pk_workflow_tags PRIMARY KEY CLUSTERED (tenant_id, workflow_name, tag);
ALTER TABLE dbo.tenant_library_installs ADD CONSTRAINT pk_tenant_library_installs PRIMARY KEY CLUSTERED (tenant_id, library);
ALTER TABLE dbo.workflow_memory_stats ADD CONSTRAINT pk_workflow_memory_stats PRIMARY KEY CLUSTERED (tenant_id, def_name);
GO
CREATE NONCLUSTERED INDEX idx_defs_active ON dbo.workflow_defs (name, version DESC);
CREATE NONCLUSTERED INDEX idx_defs_library ON dbo.workflow_defs (library, name, version);
CREATE NONCLUSTERED INDEX idx_defs_tenant_name_version ON dbo.workflow_defs (tenant_id, name, version DESC);
CREATE NONCLUSTERED INDEX idx_mem_samples_def ON dbo.workflow_memory_samples (def_name, recorded_at DESC);
CREATE NONCLUSTERED INDEX idx_memory_samples_tenant_def ON dbo.workflow_memory_samples (tenant_id, def_name, recorded_at DESC);
GO
ALTER TABLE dbo.workflow_defs ADD CONSTRAINT ck_workflow_defs_bytes_or_library
  CHECK (([library] IS NULL AND [wasm_bytes] IS NOT NULL OR [library] IS NOT NULL AND [wasm_bytes] IS NULL));
-- WITH CHECK validates every row whatever the row-level-security filter shows this session, so a tenant the
-- preflight loop could not see is still caught here (and rolls the whole migration back).
ALTER TABLE library.bundle_members WITH CHECK ADD CONSTRAINT fk_bundle_members_bundle
  FOREIGN KEY (library, bundle_version) REFERENCES library.bundles (library, bundle_version);
ALTER TABLE library.bundle_members WITH CHECK ADD CONSTRAINT fk_bundle_members_member
  FOREIGN KEY (library, name, version) REFERENCES library.members (library, name, version);
ALTER TABLE dbo.workflow_defs WITH CHECK ADD CONSTRAINT fk_workflow_defs_library
  FOREIGN KEY (library, name, version) REFERENCES library.members (library, name, version);
ALTER TABLE dbo.workflow_instances WITH CHECK ADD CONSTRAINT fk_instances_def
  FOREIGN KEY (tenant_id, def_name, def_version) REFERENCES dbo.workflow_defs (tenant_id, name, version);
ALTER TABLE dbo.workflow_routing WITH CHECK ADD CONSTRAINT fk_workflow_routing_def
  FOREIGN KEY (tenant_id, workflow_name, target_version) REFERENCES dbo.workflow_defs (tenant_id, name, version);
ALTER TABLE dbo.workflow_tags WITH CHECK ADD CONSTRAINT fk_workflow_tags_def
  FOREIGN KEY (tenant_id, workflow_name, version) REFERENCES dbo.workflow_defs (tenant_id, name, version);
GO
