package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/google/uuid"
)

// runSetSecret writes one tenant's secret, encrypted.
//
// THE VALUE IS READ FROM STDIN, NOT FROM A FLAG, and that is the whole reason
// this is a separate command rather than an extra flag on set-tenant-setting.
// A flag value is visible in `ps`, in /proc/<pid>/cmdline to any local user, and
// in the operator's shell history -- which is exactly the set of places a
// credential must not be. engine.MasterKeyFromEnv refuses the master key from a
// flag for the same reason.
//
// On PostgreSQL it writes on a connection the row-level policy does not apply
// to, because cleatctl connects as an administrative role -- the documented
// asymmetry in cmd/cleatctl/rlsposture.go. SQL Server has no such role: a
// security policy applies to sysadmin and dbo alike (rlsposture.go,
// mssqlPostureOf), and since migrations/mssql/075 cleat_admin membership does
// not bypass it either -- the disjunction that let a role bypass the filter
// was deliberately removed for the ordinary query's sake. So on SQL Server
// this only worked at all for the tenant the connection's SESSION_CONTEXT
// happened to carry, which for cleatctl was none -- PutSecret's INSERT/UPDATE
// would succeed (no BLOCK predicate stops a write) and the row would be
// immediately invisible to every reader, including the very next GetSecret,
// with no error anywhere (cleat#1989). tenantctx.With below puts the SAME
// tenant this command is writing into engine's existing beginTenantTx/
// setTenantOnTx path (engine/plugindb_tenant.go), which already knows how to
// set SESSION_CONTEXT('tenant_id') on SQL Server -- the "other means" its own
// comment refers to. This is not a cross-tenant bypass and needs no
// cleat_admin membership: the command only ever touches the one tenant named
// on the command line.
func runSetSecret(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("set-secret", flag.ContinueOnError)
	name := fs.String("name", "", "secret name, matching [A-Za-z0-9_.-]{1,128}")
	fromFile := fs.String("from-file", "", "read the value from this file instead of stdin")
	// parseFlagsAnywhere, not fs.Parse: the tenant UUID is args[0], and the flag
	// package stops at the first operand, so `set-secret <uuid> --name openai`
	// -- the order this command's own usage text and docs/how-to/use-secrets.md
	// print -- reached here with *name still "" (cleat#1933).
	operands, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		// Exits rather than returning. fs is ContinueOnError, so Parse has
		// already reported the bad flag and handed control back; returning
		// silently made cleatctl exit 0 on a typo'd flag, which reads as a
		// secret that was written.
		osExit(1)
		return
	}
	if len(operands) < 1 || *name == "" {
		printSetSecretUsage()
		osExit(1)
		return
	}

	tenantID, err := uuid.Parse(operands[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %q is not a tenant UUID: %v\n", operands[0], err)
		osExit(1)
		return
	}
	ctx = tenantctx.With(ctx, tenantID)

	master, err := engine.MasterKeyFromEnv(os.Getenv("CLEAT_SECRET_MASTER_KEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if master == nil {
		fmt.Fprintf(os.Stderr,
			"error: CLEAT_SECRET_MASTER_KEY is not set.\n\n"+
				"It must be the SAME key the workers use, or they will not be able to read\n"+
				"what this writes -- and the failure will arrive later, from inside a plugin\n"+
				"call, as an error that does not mention keys. Generate one with:\n\n"+
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

	store, err := engine.NewSecretStore(db, d.name, master)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		osExit(1)
		return
	}
	if err := store.PutSecret(ctx, tenantID.String(), *name, value); err != nil {
		fmt.Fprintf(os.Stderr, "error writing the secret: %v\n", err)
		osExit(1)
		return
	}

	// Reports the NAME and never any part of the value, not even a length --
	// a length narrows a credential's search space and buys the operator
	// nothing they did not already know.
	fmt.Printf("wrote secret %q for tenant %s\n", *name, tenantID)
	fmt.Printf("reference it from a workflow as ${secret:%s} inside a plugin call argument\n", *name)
}

// readSecretValue takes the value from a file or stdin, trimming exactly one
// trailing newline.
//
// ONE newline, not all trailing whitespace: `printf 'abc' | cleatctl ...` and
// `echo abc | cleatctl ...` should store the same thing, while a credential
// that genuinely ends in whitespace -- some do -- must survive. Trimming
// greedily would corrupt those silently, and the failure would present as a
// third-party service rejecting an apparently-correct key.
func readSecretValue(fromFile string, stdin io.Reader) (string, error) {
	var raw []byte
	var err error
	if fromFile != "" {
		raw, err = os.ReadFile(fromFile)
	} else {
		raw, err = io.ReadAll(bufio.NewReader(stdin))
	}
	if err != nil {
		return "", err
	}
	s := string(raw)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s, nil
}

func printSetSecretUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl --db <dsn> set-secret <tenant-uuid> --name <name> [--from-file <path>]

NOTE: --db is a GLOBAL flag and goes BEFORE the command name.

Reads the secret VALUE from stdin, or from --from-file. It is never taken from a
flag: a flag value appears in 'ps', in /proc/<pid>/cmdline, and in shell history.

Requires CLEAT_SECRET_MASTER_KEY, which must match what the workers use.

  head -c 32 /dev/urandom | base64          # generate a master key, once
  printf %%s "$API_KEY" | cleatctl --db "$DSN" set-secret <uuid> --name openai

A workflow then references it inside a plugin call argument:

  {"api_key": "${secret:openai}"}

The host substitutes the value on the way in to the plugin. The guest never sees
it, and event history records the reference rather than the value.
`)
}
