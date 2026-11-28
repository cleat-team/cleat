-- cleat#3629 (owner decision D59, part 2 of 3): which published library, if any, owns a workflow name.
-- See migrations/postgres/009 for the full reasoning; this is the same change.
--
-- The deploying login (a member of the application or admin role) has no SELECT on the library
-- schema, so the prefix check cannot read library.bundles itself; it called it directly in the first
-- version and every deploy through such a login failed with "The SELECT permission was denied on the
-- object 'bundles'". This scalar function answers for one name: the name of the library whose prefix
-- it starts with, or NULL. EXECUTE AS OWNER makes it read the table as the schema's owner, so no grant
-- on the library schema is needed, and nothing else of the library is visible through it.
--
-- The comparison is the database's own: LEFT(name, LEN(library) + 1) = library + N'.' with the
-- LIBRARY column as the right-hand side of LEN, so a library name is measured and a candidate name is
-- only cut to it. The collation is the column's, so while workflow names are case-insensitive here
-- (owner decision D60, cleat#3635, makes them binary) the check is exactly as wide as the database's
-- idea of "the same name". No LIKE: "_" is a wildcard.
CREATE OR ALTER FUNCTION dbo.library_owning_prefix (@name NVARCHAR(255))
RETURNS NVARCHAR(255)
WITH EXECUTE AS OWNER
AS
BEGIN
    RETURN (
        SELECT TOP 1 b.library
        FROM library.bundles AS b
        WHERE LEFT(@name, LEN(b.library) + 1) = b.library + N'.'
        ORDER BY b.library
    );
END;
GO

GRANT EXECUTE ON dbo.library_owning_prefix TO PUBLIC;
GO
