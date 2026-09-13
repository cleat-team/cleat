package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// portedOn records, per subcommand, which dialects its SQL has actually been
// written for.
//
// # WHY A REFUSAL AND NOT JUST A DRIVER
//
// cleat#1316 reads as "cleatctl hardcodes the postgres driver", and selecting
// the driver in main.go does make every subcommand CONNECT on all three. That
// is not the same as making them work, and for one of them the difference is
// destructive: drop-tenant issues 17 statements against `admin.*`, and MySQL
// has no `admin` schema -- schema and database are one namespace there, so
// `admin.tenants` is read as a database called `admin` and fails with
// "Error 1049 (42000): Unknown database 'admin'" (auth/tenant_store.go states
// the full matrix).
//
// A drop-tenant that connects and then fails on statement 9 of 17 has half
// deleted a tenant. That is strictly worse than the refusal it replaced, and
// it is the shape cleat's own notes call out: a partial implementation is
// worse than an absent one wherever a caller distinguishes
// present-and-broken from absent.
//
// So the driver is selected for everything, and each subcommand states what it
// has been ported for. The unported ones refuse UP FRONT, naming the dialect
// and what does work, instead of discovering it mid-transaction.
//
// A subcommand absent from this map is unrestricted -- it issues no SQL of its
// own, or goes entirely through engine's store interface, which is already
// dialect-aware.
var portedOn = map[string][]string{
	// Ported: the post-incident tools cleat#1316 is about.
	//
	// debug issues no SQL of its own -- it goes through the store interface --
	// so it needed the driver and nothing else. replay carries one statement,
	// whose PostgreSQL casts have explicit arms. check-db is per-dialect
	// throughout, since information_schema does not agree across the three.
	"replay":   {"postgres", "mysql", "mssql"},
	"debug":    {"postgres", "mysql", "mssql"},
	"check-db": {"postgres", "mysql", "mssql"},

	// Not ported. These carry unqualified `admin.` SQL, which is correct on
	// PostgreSQL and SQL Server and wrong on MySQL, plus $N placeholders that
	// have not been routed through plugin.Rebind.
	//
	// They are listed with the dialects they are KNOWN to work on rather than
	// omitted, so that adding a dialect here is a deliberate act with a test
	// behind it, and so the refusal message can say what does work.
	"drop-tenant":        {"postgres"},
	"revoke-api-key":     {"postgres"},
	"set-tenant-setting": {"postgres"},
	"deploy":             {"postgres"},
	"versions":           {"postgres"},
}

// requirePortedFor exits with a clear message when cmd has not been written for
// this dialect.
//
// It is called BEFORE the subcommand runs and before any statement is issued,
// which is the whole point: the failure it replaces is a partial one.
func requirePortedFor(cmd string, d dialect) {
	supported, restricted := portedOn[cmd]
	if !restricted {
		return
	}
	for _, s := range supported {
		if s == d.name {
			return
		}
	}
	sorted := append([]string(nil), supported...)
	sort.Strings(sorted)
	fmt.Fprintf(os.Stderr,
		"error: `cleatctl %s` has not been ported to %s.\n\n"+
			"It is written for: %s.\n\n"+
			"This is a refusal rather than an attempt because %s issues SQL that is\n"+
			"not portable as written, and failing partway through would leave the\n"+
			"database in a state no one asked for. `replay`, `debug` and `check-db`\n"+
			"do work on %s -- those are the post-incident tools (cleat#1316).\n",
		cmd, d.name, strings.Join(sorted, ", "), cmd, d.name)
	osExit(1)
}
