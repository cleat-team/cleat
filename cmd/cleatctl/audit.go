package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugins/auditlog"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// audit verify (cleat#2047)
// ---------------------------------------------------------------------------
//
// Recomputes a tenant's audit-log hash chain and reports the first place it fails to
// verify. It reads and never writes.
//
// WHAT A CLEAN RESULT MEANS is stated in docs/reference/audit-log.md and is narrower
// than "the log is intact": rows were not edited or removed by anyone who did not also
// rewrite the chain head and the recorded floor, and nothing outside the database
// anchors that. It does not say every request was recorded.
//
// Exit status is three-valued, and the two non-zero values must not be the same:
//
//	0  every chain verified
//	1  a chain did not verify -- a FINDING; the output names the first break
//	2  the check could not establish what it was measuring (bad usage, the audit
//	   tables are absent, a query failed) -- a failure of the check, NOT a finding
//	   about the log
//
// A run that could not read a tenant exits 2 even if other tenants verified, because
// its answer is incomplete; the tenants it did read are still printed.
//
// The audit tables are control-plane on every dialect (the plugin's migrations run
// against the base database, as tenant_api_keys does), so this uses the --db
// connection directly rather than a per-tenant database on MySQL.
//
// DBA-only, like drop-tenant and revoke-api-key: --all-tenants crosses tenants, and
// cleat's HTTP auth has no notion of a platform operator to gate that on. A tenant
// verifies its own chain over HTTP (GET /audit/verify).

const auditUsage = `Usage: cleatctl audit verify (--tenant <uuid> | --all-tenants) [--json]

Recomputes a tenant's audit-log hash chain and reports the first place it does not
verify. Reads only.

Flags:
  --tenant <uuid>   Verify one tenant's chain.
  --all-tenants     Verify every tenant that has a chain, including one whose head
                    row is missing.
  --json            Print the reports as a JSON array.

Exit status:
  0  every chain verified
  1  a chain did not verify (a finding; the first break is named)
  2  the check could not be completed (usage, missing tables, a failed query).
     This is a failure of the check, not a finding about the log.

A clean result means the rows were not edited or removed by anyone who did not also
rewrite the chain head; it does not mean every request was recorded. See
docs/reference/audit-log.md.
`

// auditVerifyChain is auditlog.VerifyChain, held in a variable so a test can make one
// tenant's read fail: the run's status when only SOME tenants could be read is a rule of
// this command, and nothing else can produce that state on demand.
var auditVerifyChain = auditlog.VerifyChain

func runAudit(ctx context.Context, db *sql.DB, d dialect, args []string) {
	if len(args) < 1 || args[0] != "verify" {
		fmt.Fprint(os.Stderr, auditUsage)
		osExit(2)
		return
	}
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, auditUsage) }
	tenantFlag := fs.String("tenant", "", "verify one tenant's chain")
	all := fs.Bool("all-tenants", false, "verify every tenant that has a chain")
	asJSON := fs.Bool("json", false, "print the reports as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		osExit(2)
		return
	}
	if (*tenantFlag == "") == !*all {
		fmt.Fprint(os.Stderr, "error: exactly one of --tenant and --all-tenants is required\n\n"+auditUsage)
		osExit(2)
		return
	}

	pdb := &engine.SQLDBAdapter{DB: db, Dialect: d.query}
	var tenants []uuid.UUID
	if *all {
		var err error
		tenants, err = auditlog.ChainedTenants(ctx, pdb, d.query)
		if err != nil {
			fmt.Fprintf(os.Stderr, "UNMEASURED: %v\n"+
				"  This is a failure of the check, not a finding about the log. Is the audit-log plugin\n"+
				"  enabled, and its migrations applied to this database?\n", err)
			osExit(2)
			return
		}
	} else {
		id, err := uuid.Parse(*tenantFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --tenant %q is not a UUID\n", *tenantFlag)
			osExit(2)
			return
		}
		tenants = []uuid.UUID{id}
	}

	var (
		reports                []auditlog.ChainReport
		ok, broken, unmeasured int
	)
	for _, t := range tenants {
		rep, err := auditVerifyChain(ctx, pdb, d.query, t)
		if err != nil {
			unmeasured++
			fmt.Fprintf(os.Stderr, "UNMEASURED tenant %s: %v\n", t, err)
			continue
		}
		reports = append(reports, rep)
		if rep.OK() {
			ok++
		} else {
			broken++
		}
	}

	if *asJSON {
		if reports == nil {
			reports = []auditlog.ChainReport{}
		}
		out, _ := json.MarshalIndent(reports, "", "  ")
		fmt.Println(string(out))
	} else {
		for _, rep := range reports {
			printChainReport(rep)
		}
	}
	// The denominator is stated beside the verdict, so "all clean" cannot be read off a
	// run that looked at nothing.
	fmt.Fprintf(os.Stderr, "verified %d of %d tenant chain(s): %d ok, %d broken, %d unmeasured\n",
		ok+broken, len(tenants), ok, broken, unmeasured)

	switch {
	case unmeasured > 0:
		fmt.Fprint(os.Stderr, "UNMEASURED: the run could not read every tenant. That is a failure of the check, not a finding about the log.\n")
		osExit(2)
	case broken > 0:
		osExit(1)
	}
}

func printChainReport(rep auditlog.ChainReport) {
	tail := ""
	if rep.Unchained > 0 {
		tail = fmt.Sprintf(" (%d row(s) predate the chain and are not covered by it)", rep.Unchained)
	}
	if rep.OK() {
		fmt.Printf("OK      tenant %s  %d row(s) verified, seq %d..%d%s\n",
			rep.TenantID, rep.Checked, rep.FloorSeq+1, rep.HeadSeq, tail)
		return
	}
	fmt.Printf("BROKEN  tenant %s  %s at seq %d: %s%s\n",
		rep.TenantID, rep.Break.Kind, rep.Break.Seq, rep.Break.Detail, tail)
}
