package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
)

// rlsPosture is what the connection behind --db can see.
//
// cleatctl asks cluster-wide questions -- "how many workflows are there, by
// status", "what versions are deployed" -- so it needs a connection that
// row-level security does not apply to. That requirement has never been stated
// anywhere, and the credential an operator is most likely to have to hand is
// the application's, which is exactly the one that cannot answer them.
//
// cleat#1184. The asymmetry is worth spelling out because it is the opposite of
// what the other binary wants: since cleat#1204, cleat-worker REFUSES TO START
// on a connection that bypasses RLS, by default. So an operator who follows the
// worker's error message to create cleat_app has thereby created the credential
// that makes cleatctl answer with one tenant's data.
type rlsPosture int

const (
	// rlsExempt: the role is a superuser or carries BYPASSRLS, so no policy
	// applies to it anywhere. This is what cleatctl needs.
	rlsExempt rlsPosture = iota
	// rlsSubject: policies apply to this connection. Reads are scoped to
	// whatever tenant the caller set -- for cleatctl's store-mediated commands
	// that is the hardcoded zero tenant, and for its raw statements it is
	// nothing at all, which raises.
	rlsSubject
	// rlsUnprotected: no policy applies, but because the DATABASE is not
	// enforcing rather than because the role is privileged -- policies missing,
	// row-level security switched off, or a table owner without FORCE.
	// cleatctl's reads will work. That is not good news and check-db says so.
	rlsUnprotected
	// rlsUnknown: the catalogue could not be interrogated.
	rlsUnknown
	// rlsNotApplicable: this dialect has no row-level security at all, so
	// there is no posture to have. MySQL. Distinct from rlsUnknown, which
	// means the question was asked and not answered -- reporting "could not
	// determine" for a database that cannot have the property is how cleat
	// #1646 happened.
	rlsNotApplicable
)

// rlsPostureOf classifies the connection.
//
// The polarity is inverted from cleat-worker's use of the same function and
// that needs care rather than a `!`. engine.CheckRLSEnforced returns the reasons
// RLS would NOT be applied, and an empty slice means it is enforced -- which is
// the GOOD case for a worker and the bad one here.
//
// Only "superuser" and "bypassrls" mean the role itself is exempt. The other
// three kinds say the database is not protecting anything, which is a different
// fact with a different remedy, so they are not folded together.
// rlsPostureFn is the seam the tests replace, in the same style as osExit.
//
// runCheckDB's tests drive a POSITIONAL mock driver: it discards the query
// string and returns the next scripted result, so every test enumerates the
// exact sequence of statements the command issues. Two catalogue queries added
// mid-function shift every script by two, and the failure is a scan-arity
// error from an unrelated assertion -- which teaches the next person that
// adding a query to this command is expensive, and is how a command ends up
// never gaining a check it needs.
var rlsPostureFn = rlsPostureOf

func rlsPostureOf(ctx context.Context, db *sql.DB, dialectName string) (rlsPosture, []engine.RLSBypassReason, error) {
	switch dialectName {
	case "mysql":
		// MySQL has no row-level security. auth/tenant_store.go already
		// records this; there is nothing to detect and nothing to warn about.
		return rlsNotApplicable, nil, nil
	case "mssql":
		return mssqlPostureOf(ctx, db)
	}
	reasons, err := engine.CheckRLSEnforced(ctx, db)
	if err != nil {
		return rlsUnknown, nil, err
	}
	for _, r := range reasons {
		if r.Kind == "superuser" || r.Kind == "bypassrls" {
			return rlsExempt, reasons, nil
		}
	}
	if len(reasons) > 0 {
		return rlsUnprotected, reasons, nil
	}
	return rlsSubject, nil, nil
}

// mssqlPostureOf classifies a SQL Server connection.
//
// THE QUESTION IS NOT THE SAME ONE. On PostgreSQL the check asks whether the
// ROLE is exempt -- superuser or BYPASSRLS -- and those are properties of the
// role that hold everywhere. SQL Server has no such thing: a security policy
// applies to sysadmin, db_owner and dbo alike, which migrations/mssql/012 says
// in its own comments and which is measured rather than inherited here.
//
// Measured against a migrated database, one login varied:
//
//	sa (IS_SRVROLEMEMBER('sysadmin') = 1)   IS_ROLEMEMBER('cleat_admin') = 0
//	unprivileged login, not a member        IS_ROLEMEMBER('cleat_admin') = 0
//	the SAME login, added to cleat_admin    IS_ROLEMEMBER('cleat_admin') = 1
//
// The first row is why a ported superuser check would be wrong: the most
// privileged principal on the server is still subject. The third is the
// control -- without it, 0 everywhere is indistinguishable from a function
// that always answers 0.
//
// IS_ROLEMEMBER is used because it ANSWERS FROM AN UNPRIVILEGED CONNECTION,
// which is the property the alternative lacks: the same login reading
// sys.sql_modules sees 0 rows, so a check built on metadata visibility cannot
// tell "no policy" from "may not look".
func mssqlPostureOf(ctx context.Context, db *sql.DB) (rlsPosture, []engine.RLSBypassReason, error) {
	// NULL rather than 0/1 means the role does not exist or is not visible,
	// which is not the same as "subject" and must not be reported as it.
	var member sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT IS_ROLEMEMBER('cleat_admin')`).Scan(&member); err != nil {
		return rlsUnknown, nil, fmt.Errorf("check RLS: read cleat_admin membership: %w", err)
	}
	if !member.Valid {
		// No such role: this database has not had migrations/mssql/012
		// applied, so no policy is installed either and nothing is filtering.
		return rlsUnprotected, []engine.RLSBypassReason{{
			Kind:   "no-admin-role",
			Detail: "dbo.cleat_admin does not exist, so migrations/mssql/012 has not been applied and no tenant filter is installed",
		}}, nil
	}
	if member.Int64 == 1 {
		return rlsExempt, nil, nil
	}
	return rlsSubject, nil, nil
}

// warnIfTenantScoped prints one line to stderr when --db points at a connection
// row-level security applies to.
//
// A warning rather than a refusal, deliberately. revoke-api-key reads
// admin.tenant_api_keys, which carries no policy, so it works correctly under
// an application role -- and it is a credential-rotation command someone runs
// during an incident. Refusing to start would take it away to protect commands
// it has nothing to do with.
//
// Measured 2026-09-10 on a database migrated to schema 57 with non-empty
// tables, one flag varied:
//
//	versions list    superuser: Total versions: 2   cleat_app: Total versions: 1
//
// exit 0 both times. That silent 1-of-2 is what this line exists to explain
// before it is read as an answer.
func warnIfTenantScoped(ctx context.Context, db *sql.DB, dialectName string) {
	posture, _, err := rlsPostureFn(ctx, db, dialectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not determine whether this connection is "+
			"subject to row-level security: %v\n", err)
		return
	}
	if posture != rlsSubject {
		return
	}
	if dialectName == "mssql" {
		fmt.Fprint(os.Stderr, rlsSubjectWarningMSSQL)
		return
	}
	fmt.Fprint(os.Stderr, rlsSubjectWarning)
}

// rlsSubjectWarning names the consequence rather than the mechanism. An
// operator who wanted to be told about pg_roles would not be reading it.
const rlsSubjectWarning = "warning: --db points at a connection that row-level security applies to, so\n" +
	"         this tool can see only one tenant's rows. Commands that ask cluster-wide\n" +
	"         questions answer with a subset and do not say so -- `versions list`\n" +
	"         reported 1 of 2 deployed workflows in the case this warning was written\n" +
	"         for. Commands issuing raw statements fail instead, with\n" +
	"         \"cleat.tenant_id is not set\" (P0001).\n" +
	"\n" +
	"         cleatctl needs a role that is a superuser or carries BYPASSRLS: the owner\n" +
	"         DSN, not the cleat_app role cleat-worker wants. The two binaries need\n" +
	"         different credentials on purpose -- cleat-worker refuses to start on the\n" +
	"         connection cleatctl requires.\n" +
	"\n"

// rlsSubjectWarningMSSQL is the same warning with the remedy that exists on
// SQL Server, which is a different one.
//
// The PostgreSQL text sends the reader to "a superuser or BYPASSRLS". Neither
// exists here: a security policy applies to sysadmin, db_owner and dbo alike.
// Measured on a migrated database, sa reads IS_ROLEMEMBER('cleat_admin') = 0
// while IS_SRVROLEMEMBER('sysadmin') = 1, so following the PostgreSQL advice
// -- connect as the most privileged principal available -- changes nothing and
// the operator is left believing they fixed it. cleat#1646.
const rlsSubjectWarningMSSQL = "warning: --db points at a connection that row-level security applies to, so\n" +
	"         this tool can see only one tenant's rows. Commands that ask cluster-wide\n" +
	"         questions answer with a subset and do not say so, and commands issuing\n" +
	"         raw statements read an empty table rather than failing -- a DELETE\n" +
	"         against it removes nothing and reports success.\n" +
	"\n" +
	"         SQL Server has no superuser exemption: the filter applies to sysadmin,\n" +
	"         db_owner and dbo alike. cleatctl needs a login that is a member of\n" +
	"         dbo.cleat_admin (migrations/mssql/012_admin_role.sql), which sa is NOT\n" +
	"         by default and cannot be granted -- dbo may not be added to a role.\n" +
	"\n"
