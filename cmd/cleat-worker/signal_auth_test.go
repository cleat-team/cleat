package main

// IMPROVEMENT-PLAN 3.15: signal authorization consults a list nothing can
// write.
//
// workflow_instances.allowed_signals has no writer anywhere in the product --
// no store method, no API endpoint, no CLI verb, no SDK call, in any language.
// The check installed by --require-signal-auth denies when the list is empty,
// and that flag defaulted to true, so on a default deployment every
// cross-workflow signal, every plugin-originated signal and every external HTTP
// signal was denied. The documented way to permit one -- "add \"*\" (wildcard)
// to allowed_signals" -- could not be followed.
//
// These tests run the check the worker actually installs, against a real store,
// which is the part that was missing: the only previous coverage was
// engine.TestWithSignalAuthCheck, which passes a stub closure and asserts the
// option plumbing. A test that replaces the thing under test cannot see a
// defect in it.

import (
	"context"
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

// TestSignalAuthDeniesEverySignalOnAWorkflowNobodyCanConfigure is the defect,
// pinned. A workflow created through the ordinary path has no allowed callers
// and there is no supported way to give it any, so the check refuses everyone.
//
// It is written as an assertion about *why* the default changed rather than as
// a wish: as long as nothing can write allowed_signals, this is what enabling
// the flag does, and the test says so out loud.
func TestSignalAuthDeniesEverySignalOnAWorkflowNobodyCanConfigure(t *testing.T) {
	store, runID := newSignalAuthStore(t)
	check := signalAuthCheckFor(store)

	err := check(context.Background(), runID, "some-caller")
	if err == nil {
		t.Fatalf("signal auth permitted a caller on a workflow with no allowed_signals; " +
			"if something can now write that column, 3.15 is fixed and this test should " +
			"assert the new behaviour instead")
	}
	if !strings.Contains(err.Error(), "no allowed callers configured") {
		t.Errorf("denial reason is %q, want the empty-list one", err)
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
// The list is set with raw SQL because that remains the only way to set it --
// which is the finding, not the test's convenience.
func TestSignalAuthStillEnforcesAConfiguredList(t *testing.T) {
	store, runID := newSignalAuthStore(t)
	db := testutil.TestDB(t, testutil.DialectPostgres)
	check := signalAuthCheckFor(store)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		list    string
		caller  string
		allowed bool
	}{
		{"the caller is listed", `["billing-service"]`, "billing-service", true},
		{"another caller is listed", `["billing-service"]`, "fraud-service", false},
		{"the wildcard", `["*"]`, "anything-at-all", true},
		{"an empty list", `[]`, "billing-service", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(
				`UPDATE workflow_instances SET allowed_signals = $2::jsonb WHERE id = $1`,
				runID, tc.list); err != nil {
				t.Fatalf("set allowed_signals: %v", err)
			}
			err := check(ctx, runID, tc.caller)
			if tc.allowed && err != nil {
				t.Errorf("caller %q was denied against %s: %v", tc.caller, tc.list, err)
			}
			if !tc.allowed && err == nil {
				t.Errorf("caller %q was permitted against %s", tc.caller, tc.list)
			}
		})
	}
}
