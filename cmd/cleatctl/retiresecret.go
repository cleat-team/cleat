package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// retire-secret command
// ---------------------------------------------------------------------------
//
// Why this exists. cleat#1989: the column tenant_secrets.disabled_at has
// existed since migration 081/069/073 (one per dialect), engine.GetSecret now
// refuses a row it is set on the same way it refuses a missing one, and until
// this command NOTHING set it -- `git grep -nE "UPDATE tenant_secrets" -- '*.go'`
// found only the ciphertext update in PutSecret. So a secret retired by hand
// (a raw UPDATE against the database) was the only way to revoke one, and
// there was no supported way to do it at all. A secret you cannot revoke is
// the most basic rotation gap: after a leak, the operator's first move is to
// stop the old value being used, the same shape as revoke-api-key.go.
//
// UNLIKE revoke-api-key, this is ported to ALL THREE dialects from the start
// (see cmd/cleatctl/every_subcommand_declares_its_dialects_test.go's
// unrestrictedSubcommands entry): tenant_secrets is a plain, per-tenant table
// on every dialect, not one that lives in an admin.* schema MySQL lacks --
// engine.GetSecret/PutSecret already work on all three, and retireSecretStmt
// follows the same per-dialect pattern.
//
// No master key required. Retiring is a metadata change -- it never reads or
// writes ciphertext -- so it must not be blocked on CLEAT_SECRET_MASTER_KEY
// being available right now, which is precisely when an operator cutting off
// a leaked secret cannot afford to wait.
//
// Reversible, the same way revoke-api-key's disabled_at is: re-running
// set-secret for the same name clears it (putSecretUpdateStmt, cleat#1989).
// There is deliberately no separate "revive" command -- set-secret already
// exists, an operator reviving a secret has the value in hand to re-set it
// with, and a second command doing the same UPDATE would just be a second
// place disabled_at could be cleared from.

// gosec G101 reports this constant as "potential hardcoded credentials". It
// is the --help text, flagged for containing the word `secret` next to a
// string literal -- the same false positive revokeapikey.go's usage text
// already carries, for the same reason. There is no credential in it.
//
//nolint:gosec // G101: usage text, not a credential -- see revokeapikey.go's identical finding.
const retireSecretUsage = `Usage: cleatctl --db <dsn> retire-secret <tenant-uuid> --name <name> [--dry-run]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

Stops a secret resolving via ${secret:NAME}: any lookup after this fails with
the same "not found" error as a name that was never set. Needs no master key --
retiring changes metadata only, never the encrypted value.

Reversible: run set-secret again for the same name to make it live again.

Flags:
  --name <name>   Secret name to retire, matching [A-Za-z0-9_.-]{1,128}.
  --dry-run       Show what would change, change nothing.

Examples:
  cleatctl --db "$DSN" retire-secret 00000000-0000-0000-0000-000000000000 --name openai
  cleatctl --db "$DSN" retire-secret --name openai --dry-run 00000000-0000-0000-0000-000000000000
`

func runRetireSecret(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("retire-secret", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", retireSecretUsage) }

	name := fs.String("name", "", "secret name, matching [A-Za-z0-9_.-]{1,128}")
	dryRun := fs.Bool("dry-run", false, "show what would be retired, change nothing")

	// parseFlagsAnywhere, not fs.Parse -- set-secret needed this for the same
	// reason (cleat#1933): the tenant UUID is a positional operand, and the
	// flag package stops parsing at the first one it sees, so
	// `retire-secret <uuid> --name openai` -- the order this command's own
	// usage text prints -- would otherwise reach here with *name still "".
	operands, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		osExit(1)
		return
	}
	if len(operands) < 1 || *name == "" {
		fmt.Fprintf(os.Stderr, "%s", retireSecretUsage)
		osExit(1)
		return
	}

	tenantID, err := uuid.Parse(operands[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %q is not a tenant UUID: %v\n", operands[0], err)
		osExit(1)
		return
	}
	// See setsecret.go's comment on the same call: on SQL Server, RLS applies
	// to every principal since migrations/mssql/075, and this puts the one
	// tenant this command touches into engine's existing SESSION_CONTEXT
	// mechanism (engine/plugindb_tenant.go) rather than relying on a bypass
	// that no longer exists by default.
	ctx = tenantctx.With(ctx, tenantID)

	// No master key: SecretMeta and RetireSecret touch disabled_at only.
	store, err := engine.NewSecretStore(db, d.name, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	// Always look the row up before touching it, the same as revoke-api-key:
	// an operator who mistyped the name should find out by seeing "no such
	// secret", not by later discovering the wrong integration stopped
	// working.
	exists, disabledAt, err := store.SecretMeta(ctx, tenantID.String(), *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if !exists {
		fmt.Printf("No secret named %q for tenant %s in this database.\n", *name, tenantID)
		fmt.Println("If you expected a match, check that --db points at the deployment that has it.")
		return
	}
	if disabledAt.Valid {
		fmt.Printf("Secret %q for tenant %s is already retired, at %s. Nothing to do.\n",
			*name, tenantID, disabledAt.Time.Format("2006-01-02 15:04:05 MST"))
		return
	}
	if *dryRun {
		fmt.Printf("Would retire secret %q for tenant %s.\n", *name, tenantID)
		fmt.Println("--dry-run: no change made.")
		return
	}

	n, err := store.RetireSecret(ctx, tenantID.String(), *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: retire: %v\n", err)
		osExit(1)
		return
	}
	if n == 0 {
		// Lost a race with a concurrent retire (or set-secret deleted and
		// re-created it, though nothing does that today). Same end state, so
		// not an error, but say so rather than claiming credit.
		fmt.Println("Secret was retired concurrently by someone else. End state is correct.")
		return
	}

	fmt.Printf("Retired secret %q for tenant %s.\n", *name, tenantID)
	fmt.Println("Effective immediately: any lookup now fails with the same error as a name that was never set.")
	fmt.Printf("Run set-secret again to make it live again:\n  printf %%s \"$NEW_VALUE\" | cleatctl --db \"$DSN\" set-secret %s --name %s\n", tenantID, *name)
}
