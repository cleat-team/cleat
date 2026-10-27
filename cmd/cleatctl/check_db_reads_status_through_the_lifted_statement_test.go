package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestRunCheckDBReportsTheStatusCountOfARealRun pins the CALL SITE of
// instanceStatusCountsSQL, which TestCheckDBCountsStatusFromWorkflowLeases
// does not: that test runs the lifted statement directly, so a runCheckDB that
// went back to an inline `FROM workflow_instances` literal would leave it green
// (cleat#3401, found in review of #3400).
//
// It drives runCheckDB itself, against a real narrowed schema, and reads the
// count back out of what it printed. Since cleat#3245 B5 workflow_instances has
// no status, so the revert is a runtime `column "status" does not exist`, and
// runCheckDB reports it as "INSTANCES: UNREADABLE" rather than failing -- which
// is why the assertion is on the count in the output and not on the absence of
// an error. The status is one no other test writes, so the count is about this
// run alone.
func TestRunCheckDBReportsTheStatusCountOfARealRun(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	seedLeaseRun(t, db, "b3a-callsite-status")

	stdout, stderr := withExitPanicOutput(t, func() {
		runCheckDB(context.Background(), db, dialectPostgres, "", []string{"--verbose"})
	})
	if strings.Contains(stderr, "INSTANCES: UNREADABLE") {
		t.Fatalf("check-db could not read the status counts:\n%s", stderr)
	}
	var byStatus string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "by status:") {
			byStatus = line
		}
	}
	if byStatus == "" {
		t.Fatalf("check-db --verbose printed no `by status:` line:\n%s", stdout)
	}
	var found bool
	for _, part := range strings.Split(strings.TrimPrefix(strings.TrimSpace(byStatus), "by status:"), ",") {
		if strings.TrimSpace(part) == "b3a-callsite-status: 1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("check-db's status counts = %q, want b3a-callsite-status: 1 from workflow_leases", byStatus)
	}
}
