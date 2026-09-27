package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugins/auditlog"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// audit export (cleat#2047)
// ---------------------------------------------------------------------------
//
// Writes a tenant's audit log as JSON Lines, in the same format GET /audit/export
// serves: one `event` record per row, then a `checkpoint` record. --all-tenants is the
// operator variant, and it exists only here: the owner decided there is no cross-tenant
// HTTP endpoint, so an operator uses the database credential they already hold.
//
// Each tenant's records end with that tenant's own checkpoint line, so a file that
// covers several tenants is several complete exports one after another, and a tenant
// whose export stopped early is the one with no checkpoint.
//
// Exit status: 0 the export completed; 2 it could not be completed (bad usage, the
// audit tables are absent, a query failed part-way). A part-way failure is reported on
// stderr with the tenant and the count written, and the output for that tenant has no
// checkpoint line. There is no exit status 1: an export makes no finding.

func runAuditExport(ctx context.Context, db *sql.DB, d dialect, args []string) {
	fs := flag.NewFlagSet("audit export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, auditUsage) }
	tenantFlag := fs.String("tenant", "", "export one tenant")
	all := fs.Bool("all-tenants", false, "export every tenant that has audit rows")
	fromFlag := fs.String("from", "", "earliest timestamp, RFC 3339, inclusive")
	toFlag := fs.String("to", "", "latest timestamp, RFC 3339, inclusive")
	cursor := fs.String("cursor", "", "resume a single tenant's export after this cursor")
	out := fs.String("out", "", "write to this file instead of standard output")
	if err := fs.Parse(args); err != nil {
		osExit(2)
		return
	}
	usage := func(msg string) {
		fmt.Fprintf(os.Stderr, "error: %s\n\n%s", msg, auditUsage)
		osExit(2)
	}
	if (*tenantFlag == "") == !*all {
		usage("exactly one of --tenant and --all-tenants is required")
		return
	}
	if *cursor != "" && *all {
		usage("--cursor resumes one tenant's export and cannot be used with --all-tenants")
		return
	}
	opts := auditlog.ExportOptions{Cursor: *cursor}
	for name, v := range map[string]struct {
		s   string
		dst **time.Time
	}{"from": {*fromFlag, &opts.From}, "to": {*toFlag, &opts.To}} {
		if v.s == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v.s)
		if err != nil {
			usage(fmt.Sprintf("--%s %q is not an RFC 3339 timestamp", name, v.s))
			return
		}
		*v.dst = &t
	}
	if opts.From != nil && opts.To != nil && opts.To.Before(*opts.From) {
		usage("--to is before --from")
		return
	}

	pdb := &engine.SQLDBAdapter{DB: db, Dialect: d.query}
	var tenants []uuid.UUID
	if *all {
		var err error
		if tenants, err = auditlog.TenantsWithAuditRows(ctx, pdb, d.query); err != nil {
			fmt.Fprintf(os.Stderr, "UNMEASURED: %v\n  This is a failure of the check, not a finding about the log. Is the audit-log plugin\n  enabled, and its migrations applied to this database?\n", err)
			osExit(2)
			return
		}
	} else {
		id, err := uuid.Parse(*tenantFlag)
		if err != nil {
			usage(fmt.Sprintf("--tenant %q is not a UUID", *tenantFlag))
			return
		}
		tenants = []uuid.UUID{id}
	}

	var w io.Writer = os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			osExit(2)
			return
		}
		defer f.Close()
		w = f
	}
	bw := bufio.NewWriter(w)

	done, failed := 0, 0
	for _, t := range tenants {
		var records int64
		err := auditlog.ExportTenant(ctx, pdb, d.query, t, opts, func(line []byte) error {
			records++
			_, err := bw.Write(line)
			return err
		})
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "INCOMPLETE tenant %s: %v\n  %d record(s) were written and there is no checkpoint line for this tenant. "+
				"Resume it with --tenant %s --cursor <the cursor of the last event written>.\n", t, err, records, t)
			continue
		}
		done++
	}
	if err := bw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "error: writing the export: %v\n", err)
		osExit(2)
		return
	}
	fmt.Fprintf(os.Stderr, "exported %d of %d tenant(s), %d incomplete\n", done, len(tenants), failed)
	if failed > 0 {
		osExit(2)
	}
}
