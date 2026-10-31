-- cleat#3629 (owner decision D59-b, part 3 of 3): which tenants already hold workflow names under a
-- prefix. See migrations/postgres/010 for the full reasoning; this is the same change.
--
-- The tenant filter on workflow_defs (003) reads SESSION_CONTEXT('tenant_id') and has no cross-tenant
-- bypass: the optional cross_tenant_claim.sql adds one, and it is not part of the shipped schema. So
-- the only way to read every tenant's names is one tenant at a time, which is what admin.drop_tenant
-- does: set the session context for each tenant in admin.tenants (plus the default tenant), read, and put
-- the caller's context back, in the CATCH as well, because the caller's connection outlives this call.
-- That can only see tenants with an admin.tenants row.
--
-- EXECUTE AS OWNER so the publisher needs no right on workflow_defs; EXECUTE is granted to
-- cleat_library_publisher alone. Only the application's OWN names count (library IS NULL). The comparison
-- is the database's own: LEFT(name, LEN(@prefix)) = @prefix, the prefix as the right-hand side of LEN,
-- so while workflow names are case-insensitive here (owner decision D60, cleat#3635, makes them binary)
-- the check is exactly as wide as the database's idea of "the same name". No LIKE on the NAMES: "_" is a wildcard.
--
-- THE ARGUMENT IS VALIDATED because the publisher can call this directly, not only through the engine:
-- LEFT(name, LEN('')) = '' is true of every name, so an unchecked prefix lets that role list every tenant's
-- names. It answers only for a library prefix: a library name (the engine's grammar, at most 128
-- characters) followed by ONE ".". LIKE is safe HERE because it tests the argument against a fixed
-- pattern, under a binary collation so the ranges are code-point ranges whatever the database collation.
-- The hyphen a library name may hold is swapped for a letter BEFORE the character-class test, because a
-- hyphen written inside the class was measured to refuse a legitimate name here.
--
-- DELIBERATE RESIDUAL: a validated reader still answers for ANY library-shaped prefix, so the publisher
-- can list the tenants' names under a prefix it guesses ('payroll.') without publishing anything. That is
-- accepted: the publisher is an operator credential (owner decision D40) and D59-b shows the operator the
-- clash list on a refused publish anyway. The grammar check stops the publisher listing names it has NOT
-- guessed; it does not make the publisher unable to enumerate. Tightening it to "the prefix of the library
-- being published in this transaction" would need a marker the engine sets, and the publisher holds the
-- connection, so it could set the same marker.
CREATE OR ALTER PROCEDURE dbo.library_clashing_names (@prefix NVARCHAR(255))
WITH EXECUTE AS OWNER
AS
BEGIN
    SET NOCOUNT ON;
    IF @prefix IS NULL
        OR DATALENGTH(@prefix) > 2 * 129
        OR @prefix COLLATE Latin1_General_BIN2 NOT LIKE N'[A-Za-z0-9]%.'
        OR REPLACE(@prefix COLLATE Latin1_General_BIN2, N'-', N'a') LIKE N'%[^A-Za-z0-9_.]%'
        OR @prefix COLLATE Latin1_General_BIN2 LIKE N'%.%.%'
        THROW 50010, N'library_clashing_names: the argument is not a library prefix (a library name of letters, digits, "_" and "-", then one ".")', 1;
    DECLARE @prior NVARCHAR(4000) = CAST(SESSION_CONTEXT(N'tenant_id') AS NVARCHAR(4000));
    DECLARE @tid UNIQUEIDENTIFIER, @tid_text NVARCHAR(4000);
    CREATE TABLE #clash (tenant_id UNIQUEIDENTIFIER NOT NULL, name NVARCHAR(255) NOT NULL, versions NVARCHAR(MAX) NOT NULL);

    DECLARE clash_tenants CURSOR LOCAL FAST_FORWARD FOR
        SELECT t.tenant_id FROM admin.tenants AS t
        UNION
        SELECT CAST('00000000-0000-0000-0000-000000000000' AS UNIQUEIDENTIFIER);
    BEGIN TRY
        OPEN clash_tenants;
        FETCH NEXT FROM clash_tenants INTO @tid;
        WHILE @@FETCH_STATUS = 0
        BEGIN
            SET @tid_text = CAST(@tid AS NVARCHAR(4000));
            EXEC sp_set_session_context @key = N'tenant_id', @value = @tid_text;
            INSERT INTO #clash (tenant_id, name, versions)
                SELECT @tid, d.name, STRING_AGG(CAST(d.version AS NVARCHAR(20)), N',') WITHIN GROUP (ORDER BY d.version)
                FROM dbo.workflow_defs AS d
                WHERE d.library IS NULL AND LEFT(d.name, LEN(@prefix)) = @prefix
                GROUP BY d.name;
            FETCH NEXT FROM clash_tenants INTO @tid;
        END
        CLOSE clash_tenants;
        DEALLOCATE clash_tenants;
    END TRY
    BEGIN CATCH
        IF CURSOR_STATUS('local', 'clash_tenants') >= 0
        BEGIN
            CLOSE clash_tenants;
            DEALLOCATE clash_tenants;
        END
        EXEC sp_set_session_context @key = N'tenant_id', @value = @prior;
        THROW;
    END CATCH

    EXEC sp_set_session_context @key = N'tenant_id', @value = @prior;
    SELECT LOWER(CONVERT(NVARCHAR(36), tenant_id)) AS tenant_id, name, versions FROM #clash ORDER BY tenant_id, name;
END;
GO

GRANT EXECUTE ON dbo.library_clashing_names TO cleat_library_publisher;
GO
