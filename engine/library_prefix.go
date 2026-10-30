package engine

// cleat#3629, owner decision D59, part 2: the refusals the library name prefix exists for.
//
// A library's members are named "<library>.<name>" (part 1). Two rules keep a tenant's own
// definitions and a library's apart, on the strength of that prefix:
//
//	a tenant's DEPLOY refuses any name that starts with a published library's prefix, at any version
//	an INSTALL refuses when the tenant already holds its OWN name under the library's prefix
//
// Both compare IN SQL, with each database's own equality for names. PostgreSQL and SQL Server both
// compare them exactly (SQL Server since migration 011, cleat#3635: the name columns are
// Latin1_General_100_BIN2), so `ACME.checkout` is a different workflow from `acme.checkout` and cannot
// shadow anything on either. Before 011 SQL Server's columns were SQL_Latin1_General_CP1_CI_AS and
// `ACME.checkout` WAS `acme.checkout` there. A Go strings.HasPrefix would not use the database's idea of
// "the same name", and LIKE would treat the `_` that a library name may contain as a wildcard.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
// check selected from the table and failed every deploy through SQL Server's cleat_admin login. The comparison
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

// LibraryPrefixClash is one tenant's workflow name under a library's prefix, with the versions it holds.
type LibraryPrefixClash struct {
	TenantID string
	Name     string
	Versions []int
}

// libraryPrefixQueryer is what the clash reader needs: a *sql.Tx inside a publish, or a *sql.DB for the
// sharded pre-pass that runs before any shard is written.
type libraryPrefixQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// libraryPrefixClashes returns every tenant's own workflow names under the library's prefix, with the
// versions each holds. The reader is a SECURITY DEFINER function or an EXECUTE AS OWNER procedure
// (migration 010), because the publisher cannot read tenant rows; it sees only tenants that have an
// admin.tenants row unless its owner bypasses row-level security (PostgreSQL).
func libraryPrefixClashes(ctx context.Context, q libraryPrefixQueryer, d Dialect, library string) ([]LibraryPrefixClash, error) {
	var query string
	switch d {
	case DialectPostgres:
		query = `SELECT tenant_id::text, name, versions FROM library_clashing_names($1)`
	case DialectMSSQL:
		query = `EXEC dbo.library_clashing_names @p1`
	default:
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, query, libraryMemberPrefix(library))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LibraryPrefixClash
	for rows.Next() {
		var c LibraryPrefixClash
		var versions string
		if err := rows.Scan(&c.TenantID, &c.Name, &versions); err != nil {
			return nil, err
		}
		for _, v := range strings.Split(versions, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("reading the versions %q of %s: %w", versions, c.Name, err)
			}
			c.Versions = append(c.Versions, n)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// maxClashesListed bounds how much of a clash list goes into one error message.
const maxClashesListed = 100

// clashRefusal is the publish refusal for a non-empty clash list. It lists each tenant and name, as
// owner decision D59-b asks, so the operator can rename them first.
func clashRefusal(library string, clashes []LibraryPrefixClash) error {
	var b strings.Builder
	shown := clashes
	if len(shown) > maxClashesListed {
		shown = shown[:maxClashesListed]
	}
	for _, c := range shown {
		vs := make([]string, len(c.Versions))
		for i, v := range c.Versions {
			vs[i] = fmt.Sprint(v)
		}
		fmt.Fprintf(&b, "\n  tenant %s: %s (version %s)", c.TenantID, c.Name, strings.Join(vs, ", "))
	}
	if more := len(clashes) - len(shown); more > 0 {
		fmt.Fprintf(&b, "\n  ... and %d more", more)
	}
	return publishRefused("library %q cannot be published: %d workflow name(s) already held by tenants start with its prefix %q, "+
		"and an installed library would run beside them (owner decision D59-b). Rename or remove these, then publish again:%s",
		library, len(clashes), libraryMemberPrefix(library), b.String())
}

// refusePublishThatClashes refuses a publish when any tenant already holds names under the library's
// prefix. It runs inside the publish transaction, after the exclusive library-names lock: every deploy
// that passed its check has committed by then, so the reader sees it, and a deploy that starts after
// waits for this publish and is refused.
func refusePublishThatClashes(ctx context.Context, tx *sql.Tx, d Dialect, library string) error {
	clashes, err := libraryPrefixClashes(ctx, tx, d, library)
	if err != nil {
		return fmt.Errorf("checking %q against the tenants' own workflow names: %w", library, err)
	}
	if len(clashes) == 0 {
		return nil
	}
	return clashRefusal(library, clashes)
}

// CheckLibraryPrefixClashes is the same question asked without a transaction or a lock. A sharded
// publish asks it of every shard before it writes the first, so a clash on a later shard does not leave
// the earlier ones published; the check inside each shard's own publish stays authoritative.
func CheckLibraryPrefixClashes(ctx context.Context, db *sql.DB, d Dialect, library string) error {
	clashes, err := libraryPrefixClashes(ctx, db, d, library)
	if err != nil {
		return fmt.Errorf("checking %q against the tenants' own workflow names: %w", library, err)
	}
	if len(clashes) == 0 {
		return nil
	}
	return clashRefusal(library, clashes)
}
