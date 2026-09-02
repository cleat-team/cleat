package main

// IMPROVEMENT-PLAN 3.15: signal authorization consults a list nothing can
// write.
//
// workflow_instances.allowed_signals had no writer anywhere in the product --
// no store method, no API endpoint, no CLI verb, no SDK call, in any language.
// The check installed by --require-signal-auth denies when the list is empty,
// and that flag defaulted to true, so on a default deployment every
// cross-workflow signal, every plugin-originated signal and every external HTTP
// signal was denied. The documented way to permit one -- "add \"*\" (wildcard)
// to allowed_signals" -- could not be followed.
//
// **It can be followed now.** WorkflowStore.SetAllowedSignalCallers and
// PUT /api/workflows/:id/allowed-signals are the writer; see
// engine/allowed_signals_writer_test.go for the three-dialect store coverage.
// The tests below were written to be revisited at exactly this point, and two
// of them changed:
//
//   - the denial test now asserts denial *and then* the grant, so it says the
//     feature works rather than that it is missing;
//   - the enforcement test sets the list through the supported path instead of
//     raw SQL, and still reads the column back, so it cannot pass on the
//     setter's say-so.
//
// The default is still off. A writer makes the flag usable; it does not make it
// safe to turn on for everyone, because nothing sets allowed_signals when a
// workflow starts -- so flipping the default would deny every signal until an
// operator made a second call per workflow. That is the follow-up, and it is a
// product call rather than a defect.
//
// These tests run the check the worker actually installs, against a real store,
// which is the part that was missing: the only previous coverage was
// engine.TestWithSignalAuthCheck, which passes a stub closure and asserts the
// option plumbing. A test that replaces the thing under test cannot see a
// defect in it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// newSignalAuthStore returns a real PostgreSQL-backed store plus a workflow to
// signal.
func newSignalAuthStore(t *testing.T) (engine.WorkflowStore, string) {
	t.Helper()
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping signal authorization test")
	}
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	store := engine.NewPostgresStore(db)

	ctx := context.Background()
	const defName = "signal-auth-target"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	runID, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`),
		fmt.Sprintf("signal-auth-%d", time.Now().UnixNano()), engine.DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	t.Cleanup(func() {
		testutil.CleanupTestData(t, db, testutil.DialectPostgres, runID)
	})
	return store, runID
}

// TestSignalAuthDeniesUntilConfiguredThenAllows is the feature, end to end,
// through the check the worker actually installs.
//
// It replaces TestSignalAuthDeniesEverySignalOnAWorkflowNobodyCanConfigure,
// which pinned the defect and said in its own failure message that a writer
// landing was the signal to rewrite it. Note that it would NOT have failed on
// its own: a freshly started workflow still denies everyone, because deny-all
// remains the default and that was never the bug. What changed is the second
// half -- there is now a supported way to move off that default -- so the
// assertion has to be extended rather than inverted.
func TestSignalAuthDeniesUntilConfiguredThenAllows(t *testing.T) {
	store, runID := newSignalAuthStore(t)
	check := signalAuthCheckFor(store)
	ctx := context.Background()

	err := check(ctx, runID, "billing-service")
	if err == nil {
		t.Fatal("a freshly started workflow allowed a caller; deny-all is the intended default")
	}
	if !strings.Contains(err.Error(), "no allowed callers configured") {
		t.Errorf("denial reason is %q, want the empty-list one", err)
	}

	if err := store.SetAllowedSignalCallers(ctx, runID, []string{"billing-service"}); err != nil {
		t.Fatalf("SetAllowedSignalCallers: %v", err)
	}
	if err := check(ctx, runID, "billing-service"); err != nil {
		t.Errorf("caller was still denied after being granted: %v", err)
	}
	if err := check(ctx, runID, "fraud-service"); err == nil {
		t.Error("granting billing-service also admitted fraud-service")
	}

	// And revocable, which is the half a grant-only writer would leave out.
	if err := store.SetAllowedSignalCallers(ctx, runID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := check(ctx, runID, "billing-service"); err == nil {
		t.Error("caller is still allowed after the list was cleared")
	}
}

// TestRequireSignalAuthDefaultsOff is the fix: the flag that installs the check
// above is off until the list it reads can be populated.
//
// Asserted on the flag definition rather than on a running worker, because the
// default is the whole of the change and a worker would only tell us what the
// default is by a longer route.
func TestRequireSignalAuthDefaultsOff(t *testing.T) {
	f := flag.Lookup("require-signal-auth")
	if f == nil {
		t.Fatal("require-signal-auth flag is gone; if signal authorization was removed, so should this test be")
	}
	if got := f.DefValue; got != "false" {
		t.Errorf("require-signal-auth defaults to %s, want false -- until something can write "+
			"allowed_signals, defaulting it on denies every cross-workflow signal "+
			"(IMPROVEMENT-PLAN 3.15)", got)
	}
}

// TestSignalAuthStillEnforcesAConfiguredList guards the other direction: the
// mechanism has to keep working, so that whoever gives allowed_signals a writer
// inherits a check that does its job rather than one that rotted while it was
// unreachable.
//
// The list is now set through SetAllowedSignalCallers, the supported path,
// rather than the raw SQL this used before -- but the column is still read back
// directly, so the case cannot pass because the setter and the getter agree
// with each other about something the database does not hold.
func TestSignalAuthStillEnforcesAConfiguredList(t *testing.T) {
	store, runID := newSignalAuthStore(t)
	db := testutil.TestDB(t, testutil.DialectPostgres)
	check := signalAuthCheckFor(store)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		list    []string
		wantCol string // allowed_signals as the column should hold it; "" means SQL NULL
		caller  string
		allowed bool
	}{
		{"the caller is listed", []string{"billing-service"}, `["billing-service"]`, "billing-service", true},
		{"another caller is listed", []string{"billing-service"}, `["billing-service"]`, "fraud-service", false},
		{"the wildcard", []string{"*"}, `["*"]`, "anything-at-all", true},
		{"an empty list", nil, "", "billing-service", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := store.SetAllowedSignalCallers(ctx, runID, tc.list); err != nil {
				t.Fatalf("SetAllowedSignalCallers(%v): %v", tc.list, err)
			}
			var col sql.NullString
			if err := db.QueryRow(
				`SELECT allowed_signals::text FROM workflow_instances WHERE id = $1`,
				runID).Scan(&col); err != nil {
				t.Fatalf("read allowed_signals: %v", err)
			}
			if col.String != tc.wantCol || col.Valid != (tc.wantCol != "") {
				t.Errorf("allowed_signals holds %#v, want %q", col, tc.wantCol)
			}
			err := check(ctx, runID, tc.caller)
			if tc.allowed && err != nil {
				t.Errorf("caller %q was denied against %v: %v", tc.caller, tc.list, err)
			}
			if !tc.allowed && err == nil {
				t.Errorf("caller %q was permitted against %v", tc.caller, tc.list)
			}
		})
	}
}
