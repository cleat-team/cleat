package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
)

// gosec G101: usage text, not a credential -- see retiredeploymentsecret.go's
// identical finding.
//
//nolint:gosec // G101
const resealDeploymentSecretsUsage = `usage: cleatctl --db <dsn> reseal-deployment-secrets [--dry-run]

Re-encrypts every deployment secret that is not sealed under the current
master key (the same ring set-deployment-secret and tenant secrets use), so
the previous key can be removed.

  --dry-run   read and verify everything, write nothing

Reads the key ring from the environment, exactly as reseal-secrets does:

  CLEAT_SECRET_MASTER_KEY                   the current key
  CLEAT_SECRET_MASTER_KEY_VERSION           its key_version (default 1)
  CLEAT_SECRET_MASTER_KEY_PREVIOUS          the key being retired
  CLEAT_SECRET_MASTER_KEY_PREVIOUS_VERSION  its key_version (required with it)

Exit status is non-zero while anything is left: a deployment secret this ring
cannot open (reported with its name and key_version, never touched), or one
that changed while the sweep ran (run it again). A dry run that found work is
also non-zero.
`

// runResealDeploymentSecrets mirrors reseal-secrets, minus the per-tenant
// loop: deployment_secrets has no tenant dimension (engine/deployment_secrets.go).
func runResealDeploymentSecrets(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("reseal-deployment-secrets", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", resealDeploymentSecretsUsage) }
	dryRun := fs.Bool("dry-run", false, "read and verify everything, write nothing")

	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "error: reseal-deployment-secrets takes no arguments, got %q\n\n%s", fs.Args(), resealDeploymentSecretsUsage)
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
			"reseal-deployment-secrets needs the current key and, while a rotation is in\n"+
			"flight, the previous one -- the same ring tenant secrets use.\n")
		osExit(1)
		return
	}

	store := engine.NewDeploymentSecretStore(db, d.name, ring)
	res, err := store.ResealDeploymentSecrets(ctx, *dryRun)
	if err != nil {
		printResealDeploymentSecrets(ring, res, *dryRun)
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	printResealDeploymentSecrets(ring, res, *dryRun)

	if !res.Converged() || (*dryRun && res.Resealed > 0) {
		osExit(1)
	}
}

func printResealDeploymentSecrets(ring *engine.KeyRing, res engine.DeploymentSecretReseal, dryRun bool) {
	verb := "resealed:"
	if dryRun {
		verb = "would reseal:"
	}
	fmt.Printf("key ring:              current key_version %d, holds %v\n", ring.Current().Version, ring.Versions())
	fmt.Printf("secrets examined:      %d\n", res.Rows)
	fmt.Printf("already current:       %d\n", res.Current)
	fmt.Printf("%-22s %d\n", verb, res.Resealed)
	fmt.Printf("changed while running: %d\n", res.Changed)
	fmt.Printf("unreadable:            %d\n", len(res.Unreadable))
	for _, u := range res.Unreadable {
		fmt.Printf("  UNREADABLE name=%s key_version=%d: %s\n", u.Name, u.KeyVersion, u.Reason)
	}
	if res.Changed > 0 {
		fmt.Printf("%d secret(s) changed between being read and being written; nothing was written for them.\n"+
			"Run reseal-deployment-secrets again.\n", res.Changed)
	}
}
