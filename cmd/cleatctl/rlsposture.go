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

func rlsPostureOf(ctx context.Context, db *sql.DB) (rlsPosture, []engine.RLSBypassReason, error) {
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
func warnIfTenantScoped(ctx context.Context, db *sql.DB) {
	posture, _, err := rlsPostureFn(ctx, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not determine whether this connection is "+
			"subject to row-level security: %v\n", err)
		return
	}
	if posture != rlsSubject {
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
