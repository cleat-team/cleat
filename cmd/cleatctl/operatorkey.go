package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cleat-team/cleat/auth"
)

// operator-key command -- cleat#2169.
//
// The credential this manages is the one the worker's auth chain accepts
// (auth.OperatorMiddleware) and no other. Until this command existed the table
// had a reader and no writer, which is the shape the dead-export guard cites as
// auth.TenantStore.RevokeAPIKey's defect: noticed, written about, and given a
// CLI command that issues its own SQL instead of calling it, so the method
// stayed uncalled for months. Every statement here goes THROUGH
// auth.OperatorStore deliberately, so the SQL lives in one place per dialect
// and the uncalled-method shape cannot recur.
//
// It is NOT a tenant-facing command. An operator key can reach /api/admin/* and
// nothing else in the API (auth.OperatorRoutePrefix), and the key it mints
// carries no tenant, so `--all-tenants`-style questions do not arise here the
// way audit.go's comment has to answer them.
//
// gosec G101 reports this constant as "potential hardcoded credentials". It is
// the --help text, flagged for containing the word "key" beside string
// literals -- the same false positive revokeapikey.go's and retiresecret.go's
// usage texts already carry. There is no credential in it.
//
//nolint:gosec // G101: usage text, not a credential -- see revokeapikey.go's identical finding.
const operatorKeyUsage = `Usage: cleatctl --db <dsn> operator-key <create|list|revoke> [flags]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

  operator-key create --description <text> [--expires-in <duration>]
                      [--scope admin|provision] [--principal <name>]
      Mint an operator credential and print it ONCE. Only its SHA-256 hash is
      stored, so a key printed and lost cannot be recovered or re-shown --
      revoke it and mint another.
      --scope narrows what the key may reach. The default, admin, is every
      /api/admin/* route, as every key was before scopes existed. provision
      reaches only tenant provisioning and REQUIRES --principal, the name of the
      service the key is issued to. A key can carry several scopes (repeat the
      flag or comma-separate them).
  operator-key list
      Every credential, newest first, including revoked and expired ones.
      Prints no key material, because none is stored.
  operator-key revoke --key-id <uuid>
      Disable a credential. Disabling is not reversible; mint a new one.

Examples:
  cleatctl --db "$DSN" operator-key create --description "on-call laptop"
  cleatctl --db "$DSN" operator-key create --description "ci" --expires-in 720h
  cleatctl --db "$DSN" operator-key create --description "clew provisioner" --scope provision --principal clew-service
  cleatctl --db "$DSN" operator-key list
  cleatctl --db "$DSN" operator-key revoke --key-id 3f2a1b8c-...
`

func runOperatorKey(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, operatorKeyUsage)
		osExit(2)
		return
	}

	// Built on the GLOBAL --db connection -- the base database, not a
	// tenant-scoped one. That is the same requirement the worker has and for
	// the same reason: an operator key belongs to no tenant, so on MySQL a
	// tenant-scoped pool is a different physical database and every statement
	// would fail against a table that is right there in the base one. cleat#866
	// is that mistake, made once already, for the tenant keys.
	store, err := auth.NewOperatorStoreForDialect(db, d.name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}

	switch args[0] {
	case "create":
		operatorKeyCreate(ctx, store, args[1:])
	case "list":
		operatorKeyList(ctx, store, args[1:])
	case "revoke":
		operatorKeyRevoke(ctx, store, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown operator-key subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, operatorKeyUsage)
		osExit(2)
	}
}

func operatorKeyCreate(ctx context.Context, store *auth.OperatorStore, args []string) {
	fs := flag.NewFlagSet("operator-key create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", operatorKeyUsage) }

	description := fs.String("description", "", "what this credential is for; shown by list and revoke")
	expiresIn := fs.Duration("expires-in", 0, "how long the credential stays valid (0 = never expires)")
	var scopes scopeFlag
	fs.Var(&scopes, "scope", "what the key may reach: admin (default) or provision; repeat or comma-separate for several")
	principal := fs.String("principal", "", "the name of the service the key is issued to; required for the provision scope")

	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}

	// Required rather than defaulting to "". The column has a default, so this
	// is a CLI decision and not a schema one: an operator key is the most
	// powerful credential cleat has, and the moment it matters is the moment
	// someone has to decide whether it can be revoked -- which needs to know
	// what it was for. A blank description makes a list of them
	// indistinguishable, and indistinguishability is what stops a leak being
	// closed.
	if strings.TrimSpace(*description) == "" {
		fmt.Fprint(os.Stderr, "error: --description is required, and should say what this credential is for\n\n")
		fmt.Fprint(os.Stderr, operatorKeyUsage)
		osExit(2)
		return
	}

	var expiresAt *time.Time
	if *expiresIn > 0 {
		t := time.Now().Add(*expiresIn)
		expiresAt = &t
	}

	// Generated here, not in the store: this is the only place the plaintext
	// can be shown, so it has to exist before the INSERT and be printed after.
	raw := auth.GenerateOperatorKey()
	// admin when no --scope was given: that is what a key was before scopes
	// existed, so leaving the flag off changes nothing. Defaulted HERE and not in
	// the store, which refuses an empty scope list on purpose.
	scopeList := []string(scopes)
	if len(scopeList) == 0 {
		scopeList = []string{auth.ScopeAdmin}
	}
	op, err := store.CreateOperatorKey(ctx, raw, auth.OperatorKeySpec{
		Description: *description,
		ExpiresAt:   expiresAt,
		Scopes:      scopeList,
		Principal:   *principal,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not create the operator key: %v\n", err)
		osExit(1)
		return
	}

	fmt.Printf("Operator key created.\n\n")
	fmt.Printf("  key_id:      %s\n", op.KeyID)
	fmt.Printf("  description: %s\n", op.Description)
	fmt.Printf("  scopes:      %s\n", op.Scopes)
	if op.Principal != "" {
		fmt.Printf("  principal:   %s\n", op.Principal)
	}
	if expiresAt != nil {
		fmt.Printf("  expires:     %s\n", expiresAt.UTC().Format(time.RFC3339))
	} else {
		fmt.Printf("  expires:     never\n")
	}
	fmt.Printf("\n  %s\n\n", raw)
	fmt.Printf("This is the only time the key is shown -- only its hash is stored, so it\n")
	fmt.Printf("cannot be recovered. Use it as: Authorization: Bearer <key>\n")
	if op.Scopes == auth.ScopeAdmin {
		fmt.Printf("It is accepted on /api/admin/* and refused everywhere else in the API.\n")
	} else {
		fmt.Printf("It is accepted only on the routes its scope names, and refused everywhere else in the API.\n")
	}
}

// scopeFlag collects --scope. Repeating the flag and comma-separating both work,
// because both are what someone typing it would try.
type scopeFlag []string

func (s *scopeFlag) String() string { return strings.Join(*s, ",") }

func (s *scopeFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

func operatorKeyList(ctx context.Context, store *auth.OperatorStore, args []string) {
	fs := flag.NewFlagSet("operator-key list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", operatorKeyUsage) }
	// Nothing to configure, but parse anyway so an unknown flag is an error
	// rather than silently ignored -- the reason args_test.go exists.
	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}

	keys, err := store.ListOperatorKeys(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not list operator keys: %v\n", err)
		osExit(1)
		return
	}
	if len(keys) == 0 {
		fmt.Printf("No operator keys.\n\nMint one with:\n  cleatctl --db \"$DSN\" operator-key create --description \"what it is for\"\n")
		return
	}

	now := time.Now()
	for _, k := range keys {
		fmt.Printf("%s  %-8s  %-10s  %s\n", k.KeyID, operatorKeyState(k, now), k.Scopes, operatorKeyLabel(k))
	}
}

// operatorKeyLabel is a key's description, with its principal when it has one, so
// a list of keys issued to different services reads as such.
func operatorKeyLabel(k auth.Operator) string {
	if k.Principal == "" {
		return k.Description
	}
	return k.Description + "  [principal: " + k.Principal + "]"
}

// operatorKeyState names a row's state rather than leaving it to be inferred
// from columns the reader has to combine themselves. Revoked and expired are
// reported separately because they are different facts about a key -- one is a
// decision somebody made, the other is the clock -- and a key that is both is
// reported as revoked, which is the more specific of the two.
func operatorKeyState(k auth.Operator, now time.Time) string {
	switch {
	case k.Disabled:
		return "revoked"
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return "expired"
	default:
		return "live"
	}
}

func operatorKeyRevoke(ctx context.Context, store *auth.OperatorStore, args []string) {
	fs := flag.NewFlagSet("operator-key revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", operatorKeyUsage) }

	keyID := fs.String("key-id", "", "key_id (uuid) of the credential to revoke")
	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	if *keyID == "" {
		fmt.Fprint(os.Stderr, "error: --key-id is required\n\n")
		fmt.Fprint(os.Stderr, operatorKeyUsage)
		osExit(2)
		return
	}

	// Show the row before touching it, the same discipline revoke-api-key's
	// comment states: an operator who mistypes an id should find out by seeing
	// the WRONG description here, not by discovering later that the wrong
	// integration stopped working. Read through the same store as the write, so
	// what is displayed and what is changed cannot be two different tables.
	keys, err := store.ListOperatorKeys(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not read the key before revoking it: %v\n", err)
		osExit(1)
		return
	}
	var found *auth.Operator
	for i := range keys {
		if keys[i].KeyID == *keyID {
			found = &keys[i]
			break
		}
	}
	if found == nil {
		fmt.Fprintf(os.Stderr, "error: no operator key with id %s in this database.\n\n"+
			"A revoked key still appears in --list, so this is either a mistyped id or a --db\n"+
			"pointing at a different deployment. Run `cleatctl --db \"$DSN\" operator-key list`.\n", *keyID)
		osExit(1)
		return
	}
	if found.Disabled {
		fmt.Printf("Already revoked: %s (%s)\n", found.KeyID, found.Description)
		return
	}

	if err := store.RevokeOperatorKey(ctx, *keyID); err != nil {
		if errors.Is(err, auth.ErrOperatorKeyNotFound) {
			// The row was live a moment ago and is not now: another revoke won
			// the race. That is the outcome this command wanted, so it is not
			// an error -- and saying so is better than a bare non-zero exit.
			fmt.Printf("Already revoked: %s (%s)\n", found.KeyID, found.Description)
			return
		}
		fmt.Fprintf(os.Stderr, "error: could not revoke the operator key: %v\n", err)
		osExit(1)
		return
	}
	fmt.Printf("Revoked: %s (%s)\n", found.KeyID, found.Description)
}
