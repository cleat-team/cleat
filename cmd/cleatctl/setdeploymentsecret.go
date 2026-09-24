package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
)

// runSetDeploymentSecret writes one deployment-wide credential, encrypted.
// Mirrors set-secret exactly, minus the tenant argument (cleat#1992 part 1):
// see setsecret.go's doc comment for why the value is read from stdin/
// --from-file rather than a flag, and why the ring comes from the
// environment. The two differ only in that this store carries no tenant
// dimension at all (engine/deployment_secrets.go), so there is no ctx
// marking and no per-dialect RLS/SESSION_CONTEXT asymmetry to route around.
func runSetDeploymentSecret(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("set-deployment-secret", flag.ContinueOnError)
	name := fs.String("name", "", "deployment secret name, matching [A-Za-z0-9_.-]{1,128}")
	fromFile := fs.String("from-file", "", "read the value from this file instead of stdin")
	if err := fs.Parse(args); err != nil {
		osExit(1)
		return
	}
	if fs.NArg() != 0 || *name == "" {
		printSetDeploymentSecretUsage()
		osExit(1)
		return
	}

	// The SAME ring set-secret reads: deployment secrets share tenant
	// secrets' master key (engine/deployment_secrets.go's doc comment on
	// DeploymentSecretStore explains why), so there is exactly one key an
	// operator generates and rolls out, not two.
	ring, err := engine.SecretKeyRingFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if ring == nil {
		fmt.Fprintf(os.Stderr,
			"error: CLEAT_SECRET_MASTER_KEY is not set.\n\n"+
				"It must be the SAME key the workers use, or they will not be able to read\n"+
				"what this writes. Generate one with:\n\n"+
				"  head -c 32 /dev/urandom | base64\n")
		osExit(1)
		return
	}

	value, err := readSecretValue(*fromFile, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading the value: %v\n", err)
		osExit(1)
		return
	}
	if value == "" {
		fmt.Fprintf(os.Stderr, "error: the value is empty; refusing to store it\n")
		osExit(1)
		return
	}

	store := engine.NewDeploymentSecretStore(db, d.name, ring)
	if err := store.PutDeploymentSecret(ctx, *name, value); err != nil {
		fmt.Fprintf(os.Stderr, "error writing the deployment secret: %v\n", err)
		osExit(1)
		return
	}

	fmt.Printf("wrote deployment secret %q, sealed under key_version %d\n", *name, ring.Current().Version)
}

func printSetDeploymentSecretUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> set-deployment-secret --name <name> [--from-file <path>]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

Reads the value from stdin, or from --from-file. It is never taken from a
flag: a flag value appears in 'ps', in /proc/<pid>/cmdline, and in shell history.

Requires CLEAT_SECRET_MASTER_KEY, the same key tenant secrets use, and
CLEAT_SECRET_MASTER_KEY_VERSION if the current key is not version 1.

Fixed, documented names (docs/how-to/use-deployment-secrets.md), not chosen
here: blobstore.access_key_id, blobstore.secret_access_key,
email.sendgrid_api_key, llm.providers.<provider>.api_key,
slacknotify.signing_secret, scheduledbackup.dsn.

  head -c 32 /dev/urandom | base64          # generate a master key, once
  printf %%s "$SENDGRID_KEY" | cleatctl --db "$DSN" set-deployment-secret --name email.sendgrid_api_key
`)
}
