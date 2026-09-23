package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// ---------------------------------------------------------------------------
// reseal-secrets command
// ---------------------------------------------------------------------------
//
// Why this exists. cleat#1991: a tenant secret is sealed under a master key, and
// until this there was no way to change that key without re-entering every
// plaintext -- which nobody has, because nothing reads a secret back out
// (docs/how-to/use-secrets.md). This command decrypts every row under the key its
// key_version names and re-encrypts it under the CURRENT key, so the previous key
// can be removed from the ring.
//
// It is the second half of a rotation, and the order matters. The operator
// procedure (docs/how-to/use-secrets.md) is: deploy every worker with the new key
// current and the old key previous; run this; confirm it reports nothing left;
// only then remove the previous key. specs/CleatKeyRotation.tla checks that
// procedure, and shows why each step is there: a worker that starts without a key
// a row needs refuses to start (the boot check), and a write is refused while any
// live worker cannot open it.
//
// EVERY DIALECT, and it needs no administrative role. It reads tenant by tenant
// under each tenant's own context, suspended tenants included, so it works on a
// connection row-level security applies to and on SQL Server, which has no bypass
// role. The single unscoped read this tool used to lean on cannot see
// tenant_secrets on either of those (cleat#2123).
//
// The ring comes from the ENVIRONMENT and not a flag, for the reason
// engine.MasterKeyFromEnv gives: a flag is visible in `ps`.

const resealUsage = `usage: cleatctl --db <dsn> reseal-secrets [--dry-run]

Re-encrypts every tenant secret that is not sealed under the current master key,
so the previous key can be removed. Online: secrets keep resolving throughout.

  --dry-run   read and verify everything, write nothing

Reads the key ring from the environment, exactly as the workers do:

  CLEAT_SECRET_MASTER_KEY                   the current key
  CLEAT_SECRET_MASTER_KEY_VERSION           its key_version (default 1)
  CLEAT_SECRET_MASTER_KEY_PREVIOUS          the key being retired
  CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION  its key_version (required with it)

Run it only after EVERY worker has been deployed with the new key in its ring.

Exit status is non-zero while anything is left: a secret this ring cannot open
(reported with its tenant, name and key_version, and never touched), or one that
changed while the sweep ran (run it again). A dry run that found work is also
non-zero. So it can be run in a loop and its exit code trusted.
`

func runResealSecrets(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("reseal-secrets", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", resealUsage) }
	dryRun := fs.Bool("dry-run", false, "read and verify everything, write nothing")

	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "error: reseal-secrets takes no arguments, got %q\n\n%s", fs.Args(), resealUsage)
		osExit(2)
		return
	}

	ring, err := engine.SecretKeyRingFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if ring == nil {
		fmt.Fprintf(os.Stderr, "error: CLEAT_SECRET_MASTER_KEY is not set.\n\n"+
			"reseal-secrets needs the current key and, while a rotation is in flight, the\n"+
			"previous one -- the same ring the workers hold.\n")
		osExit(1)
		return
	}

	store := engine.NewSecretStoreWithRing(db, d.name, ring)
	res, err := store.ResealSecrets(ctx, *dryRun)
	if err != nil {
		// What was done before the failure is real, so report it before exiting.
		printResealSecrets(ring, res, *dryRun)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	printResealSecrets(ring, res, *dryRun)

	// Non-zero while anything is left, so this can be looped on. A dry run that
	// found work is a report that the database is not converged.
	if !res.Converged() || (*dryRun && res.Resealed > 0) {
		osExit(1)
	}
}

// printResealSecrets reports every outcome, and every field is printed even when
// zero: a sweep that says "done" without its counts is indistinguishable from
// one that matched no rows. It prints names and versions and never a plaintext
// or key material.
func printResealSecrets(ring *engine.KeyRing, res engine.SecretReseal, dryRun bool) {
	verb := "resealed:"
	if dryRun {
		verb = "would reseal:"
	}
	fmt.Printf("key ring:              current key_version %d, holds %v\n", ring.Current().Version, ring.Versions())
	fmt.Printf("tenants examined:      %d\n", res.Tenants)
	fmt.Printf("secrets examined:      %d\n", res.Rows)
	fmt.Printf("already current:       %d\n", res.Current)
	fmt.Printf("%-22s %d\n", verb, res.Resealed)
	fmt.Printf("changed while running: %d\n", res.Changed)
	fmt.Printf("unreadable:            %d\n", len(res.Unreadable))
	for _, u := range res.Unreadable {
		fmt.Printf("  UNREADABLE tenant=%s name=%s key_version=%d: %s\n",
			u.TenantID, u.Name, u.KeyVersion, strings.TrimSpace(u.Reason))
	}
	if res.Changed > 0 {
		fmt.Printf("%d secret(s) changed between being read and being written; nothing was written for them.\n"+
			"Run reseal-secrets again.\n", res.Changed)
	}
}
