package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
)

// gosec G101: usage text, not a credential -- see retiresecret.go's identical
// finding.
//
//nolint:gosec // G101
const retireDeploymentSecretUsage = `Usage: cleatctl --db <dsn> retire-deployment-secret --name <name> [--dry-run]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

Stops a deployment secret resolving: a plugin's next Get fails with the same
"not found" error as a name that was never set. Needs no master key --
retiring changes metadata only, never the encrypted value.

Reversible: run set-deployment-secret again for the same name to make it live
again.

Flags:
  --name <name>   Deployment secret name to retire.
  --dry-run       Show what would change, change nothing.
`

// runRetireDeploymentSecret mirrors retire-secret exactly, minus the tenant
// argument. See retiresecret.go's doc comment for why this needs no master
// key and why it looks the row up before touching it.
func runRetireDeploymentSecret(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("retire-deployment-secret", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, "%s", retireDeploymentSecretUsage) }

	name := fs.String("name", "", "deployment secret name")
	dryRun := fs.Bool("dry-run", false, "show what would be retired, change nothing")

	if err := fs.Parse(args); err != nil {
		osExit(1)
		return
	}
	if fs.NArg() != 0 || *name == "" {
		fmt.Fprintf(os.Stderr, "%s", retireDeploymentSecretUsage)
		osExit(1)
		return
	}

	// No master key: DeploymentSecretMeta and RetireDeploymentSecret touch
	// disabled_at only.
	store := engine.NewDeploymentSecretStore(db, d.name, nil)

	exists, disabledAt, err := store.DeploymentSecretMeta(ctx, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if !exists {
		fmt.Printf("No deployment secret named %q in this database.\n", *name)
		fmt.Println("If you expected a match, check that --db points at the deployment that has it.")
		return
	}
	if disabledAt.Valid {
		fmt.Printf("Deployment secret %q is already retired, at %s. Nothing to do.\n",
			*name, disabledAt.Time.Format("2006-01-02 15:04:05 MST"))
		return
	}
	if *dryRun {
		fmt.Printf("Would retire deployment secret %q.\n", *name)
		fmt.Println("--dry-run: no change made.")
		return
	}

	n, err := store.RetireDeploymentSecret(ctx, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: retire: %v\n", err)
		osExit(1)
		return
	}
	if n == 0 {
		fmt.Println("Deployment secret was retired concurrently by someone else. End state is correct.")
		return
	}

	fmt.Printf("Retired deployment secret %q.\n", *name)
	fmt.Println("Effective immediately: any lookup now fails with the same error as a name that was never set.")
	fmt.Printf("Run set-deployment-secret again to make it live again:\n  printf %%s \"$NEW_VALUE\" | cleatctl --db \"$DSN\" set-deployment-secret --name %s\n", *name)
}
