package engine

// cleat#3629, owner decision D59, part 2: the refusals the library name prefix exists for.
//
// A library's members are named "<library>.<name>" (part 1). Two rules keep a tenant's own
// definitions and a library's apart, on the strength of that prefix:
//
//	a tenant's DEPLOY refuses any name that starts with a published library's prefix, at any version
//	an INSTALL refuses when the tenant already holds its OWN name under the library's prefix
//
// Both compare IN SQL, with each database's own equality for names. SQL Server compares names
// case-insensitively (the column is SQL_Latin1_General_CP1_CI_AS) and PostgreSQL does not, so on SQL
// Server `ACME.checkout` IS `acme.checkout` and must be refused, while on PostgreSQL it is a different
// workflow and cannot shadow anything. A Go strings.HasPrefix would let the first through, and LIKE
// would treat the `_` that a library name may contain as a wildcard.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrWorkflowNameUnderLibraryPrefix is matched by errors.Is for a deploy refused because its name
// starts with a published library's prefix.
var ErrWorkflowNameUnderLibraryPrefix = errors.New("workflow name is under a published library's prefix")

// WorkflowNameUnderLibraryPrefixError names the workflow and the library whose prefix it is under.
type WorkflowNameUnderLibraryPrefixError struct {
	Name    string
	Library string
}

func (e *WorkflowNameUnderLibraryPrefixError) Error() string {
	return fmt.Sprintf("refusing to deploy workflow %q: library %q is published, and names starting with %q belong to it (owner decision D59). "+
		"Deploy this workflow under a name that does not start with that prefix, or install the library and use its workflows", e.Name, e.Library, libraryMemberPrefix(e.Library))
}

// Is reports a match against ErrWorkflowNameUnderLibraryPrefix.
func (e *WorkflowNameUnderLibraryPrefixError) Is(target error) bool {
	return target == ErrWorkflowNameUnderLibraryPrefix
}

// LibraryPrefixHeldError is an install refused because the tenant already holds definitions of its
// OWN under the library's prefix. It matches ErrLibraryDefinitionCollision, so the install route
// answers it as it answers any other definition in the way (409), and the message says which names.
type LibraryPrefixHeldError struct {
	Library string
	Names   []string
}

func (e *LibraryPrefixHeldError) Error() string {
	return fmt.Sprintf("refusing to install library %q: this tenant already has workflows of its own under the library's prefix %q (%s), "+
		"and an installed library would run beside them (owner decision D59). Those workflows keep running; "+
		"they must be renamed or removed before this library can be installed", e.Library, libraryMemberPrefix(e.Library), strings.Join(e.Names, ", "))
}

// Is reports a match against ErrLibraryDefinitionCollision.
func (e *LibraryPrefixHeldError) Is(target error) bool {
	return target == ErrLibraryDefinitionCollision
}

// libraryNamesLockKey is the PostgreSQL advisory-lock key for "the set of library names" (the bytes
// of "cleat_ln"), and libraryNamesLockResource the SQL Server application-lock resource. Every
// deploy and every publish names the same one.
const (
	libraryNamesLockKey      int64 = 0x636c6561745f6c6e
	libraryNamesLockResource       = "cleat.library_names"
	// libraryNamesLockWaitMS bounds a wait on the lock (SQL Server). A holder is a single
	// transaction, so a wait this long means something is wrong, and it is an error and not a hang.
	libraryNamesLockWaitMS = 30000
)

// lockLibraryNames takes the transaction-scoped lock over "the set of library names": SHARED by every
// deploy before its prefix check and held to its commit, EXCLUSIVE by a publish before it inserts.
//
// The prefix rules are a check followed by a write, and without the lock the checks race the writes:
// a deploy can pass before the library exists, the library can be published, an install can look at
// the tenant's own names before the deploy commits, and then both exist. Shared locks do not block
// one another, so deploys stay concurrent; a publish waits for the deploys in flight, and a deploy
// that starts after waits for the publish and then sees the library. MySQL has no libraries (D41).
func lockLibraryNames(ctx context.Context, tx *sql.Tx, d Dialect, exclusive bool) error {
	switch d {
	case DialectPostgres:
		fn := "pg_advisory_xact_lock_shared"
		if exclusive {
			fn = "pg_advisory_xact_lock"
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SELECT %s(%d)", fn, libraryNamesLockKey)); err != nil {
			return fmt.Errorf("taking the library-names lock: %w", err)
		}
	case DialectMSSQL:
		mode := "Shared"
		if exclusive {
			mode = "Exclusive"
		}
		var rc int
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`DECLARE @r int;
EXEC @r = sp_getapplock @Resource = N'%s', @LockMode = N'%s', @LockOwner = N'Transaction', @LockTimeout = %d;
SELECT @r;`, libraryNamesLockResource, mode, libraryNamesLockWaitMS)).Scan(&rc)
		if err != nil {
			return fmt.Errorf("taking the library-names lock: %w", err)
		}
		if rc < 0 { // -1 timeout, -2 cancelled, -3 deadlock victim, -999 parameter or call error
			return fmt.Errorf("taking the library-names lock: sp_getapplock returned %d", rc)
		}
	}
	return nil
}

// refuseNameUnderLibraryPrefix refuses a deploy of a name under a published library's prefix, at any
// version. The question goes to a SECURITY DEFINER function (migration 009), not to library.bundles
// itself: the deploying role is not allowed to read the library schema, and the first version of this
// check selected from the table and failed every deploy through a per-tenant login. The comparison
// inside the function is the database's own (starts_with on PostgreSQL; LEFT/LEN with the library as
// the right-hand side on SQL Server), never LIKE, where the "_" a library name may hold is a wildcard,
// and never in Go.
func refuseNameUnderLibraryPrefix(ctx context.Context, tx *sql.Tx, d Dialect, name string) error {
	var q string
	switch d {
	case DialectPostgres:
		q = `SELECT library_owning_prefix($1)`
	case DialectMSSQL:
		q = `SELECT dbo.library_owning_prefix(@p1)`
	default:
		return nil
	}
	var library sql.NullString
	if err := tx.QueryRowContext(ctx, q, name).Scan(&library); err != nil {
		return fmt.Errorf("checking %q against the published libraries' prefixes: %w", name, err)
	}
	if !library.Valid {
		return nil
	}
	return &WorkflowNameUnderLibraryPrefixError{Name: name, Library: library.String}
}

// refuseOwnNamesUnderPrefix refuses an install when the tenant holds names of its OWN (not installed
// ones) under the library's prefix, at any version. Rows already there are grandfathered: they keep
// running, and only the install is refused.
func refuseOwnNamesUnderPrefix(ctx context.Context, tx *sql.Tx, d Dialect, tenantID, library string) error {
	prefix := libraryMemberPrefix(library)
	var q string
	switch d {
	case DialectPostgres:
		q = `SELECT DISTINCT name FROM workflow_defs WHERE tenant_id = $1 AND library IS NULL AND starts_with(name, $2) ORDER BY name LIMIT 5`
	case DialectMSSQL:
		q = `SELECT DISTINCT TOP 5 name FROM workflow_defs WHERE tenant_id = @p1 AND library IS NULL AND LEFT(name, LEN(@p2)) = @p2 ORDER BY name`
	default:
		return nil
	}
	rows, err := tx.QueryContext(ctx, q, tenantID, prefix)
	if err != nil {
		return fmt.Errorf("checking the tenant's own workflows against %q: %w", prefix, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("reading the tenant's own workflow names: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) > 0 {
		return &LibraryPrefixHeldError{Library: library, Names: names}
	}
	return nil
}
