package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#2348. The adaptive batch writer is PostgreSQL SQL, batch mode is on by
// default, and it is entered on the step rate alone -- so a busy MySQL or SQL
// Server worker switched into a writer that could not succeed. Measured against
// the real worker binary: every event through it was retried for the whole
// retry window (750ms) and then dropped, and the run carried on.
//
// TestAWorkerAboveTheEnterRateKeepsPersistingEveryEvent is the behaviour;
// TestEveryStoreThatFlushesPerStepIsRefusedBatchMode is what keeps a fourth
// dialect from reopening it.

// TestAWorkerAboveTheEnterRateKeepsPersistingEveryEvent puts a flusher that is
// ALREADY in batch mode in front of each dialect and records events through
// recordEvent, the path the guest takes. The PostgreSQL subtest is the control:
// it must really use the batch writer, or "MySQL did not" says nothing.
func TestAWorkerAboveTheEnterRateKeepsPersistingEveryEvent(t *testing.T) {
	const events = 3

	for _, tc := range []struct {
		dialect   testutil.Dialect
		wantBatch bool
	}{
		{testutil.DialectPostgres, true},
		{testutil.DialectMySQL, false},
		{testutil.DialectMSSQL, false},
	} {
		t.Run(string(tc.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, tc.dialect)
			switch tc.dialect {
			case testutil.DialectPostgres:
				testutil.SetupFullSchema(t, db, tc.dialect)
			case testutil.DialectMySQL:
				testutil.SetupMySQLFullSchema(t, db)
			case testutil.DialectMSSQL:
				testutil.SetupMSSQLFullSchema(t, db)
			}
			defer db.Close()

			wfID := "batch-pg-only-" + string(tc.dialect)
			seedWorkflowInstance(t, db, tc.dialect, wfID)

			// Configured the way the worker does, then forced over the
			// threshold: the rate EWMA is not what is under test, the reaction to
			// being above it is. lastSample is refreshed before every event so
			// updateRate does not sample a rate of zero and leave batch mode
			// again -- which is what made a first version of this test measure
			// the direct path on every dialect.
			af := NewAdaptiveFlusher(db, DefaultTenantUUID, 8*time.Millisecond, 200, 1, 0.5, 0)
			reg := &TenantFlusherRegistry{flushers: map[string]*AdaptiveFlusher{DefaultTenantUUID: af}}
			e := NewEngine(nil, nil,
				WithWorkflowID(wfID),
				WithTenantID(DefaultTenantUUID),
				WithWorkflowStore(storeFor(t, tc.dialect, db)),
				WithDB(db),
				WithFlusherRegistry(reg))
			s := &execSession{engine: e, workflowID: wfID}

			for step := 0; step < events; step++ {
				af.mu.Lock()
				af.batchMode = true
				af.lastSample = time.Now()
				af.mu.Unlock()

				if got := s.recordEvent(stampedRecord(step)); got != eventPersisted {
					t.Fatalf("recordEvent(step %d) on %s = %v, want eventPersisted", step, tc.dialect, got)
				}
			}

			var n int
			if err := testutil.AdminDB(t, db, tc.dialect).QueryRow(countEventsSQL(tc.dialect), wfID).Scan(&n); err != nil {
				t.Fatalf("counting events on %s: %v", tc.dialect, err)
			}
			if n != events {
				t.Errorf("event_history holds %d of %d events on %s after recordEvent returned for each.\n\n"+
					"Batch mode is PostgreSQL-only. A flusher above the enter rate on another dialect "+
					"used to send every event through PostgreSQL-dialect SQL that cannot run there, "+
					"retry it for the whole retry window, and drop it (cleat#2348).", n, events, tc.dialect)
			}

			if batched := af.batchedEvents.Load(); tc.wantBatch && batched == 0 {
				t.Errorf("CONTROL: the batch writer persisted nothing on %s, so this test did not "+
					"exercise batch mode and the other dialects' result says nothing", tc.dialect)
			} else if !tc.wantBatch && batched != 0 {
				t.Errorf("the batch writer handled %d events on %s, which it cannot write", batched, tc.dialect)
			}
		})
	}
}

// wrappedMySQLStore is what a decorator around a MySQL store looks like:
// not a *MySQLStore, but with its flushEventForStep by embedding. A gate that
// names concrete types lets it through to the PostgreSQL batch writer.
type wrappedMySQLStore struct{ *MySQLStore }

type wrappedMSSQLStore struct{ *MSSQLStore }

// aStoreThatIsNotPostgres implements perStepEventFlusher and nothing else.
type aStoreThatIsNotPostgres struct{ WorkflowStore }

func (aStoreThatIsNotPostgres) flushEventForStep(context.Context, string, EventRecord) error {
	return nil
}

type aStoreThatStandsInForPostgres struct{ aStoreThatIsNotPostgres }

func (aStoreThatStandsInForPostgres) standsInForPostgres() bool { return true }

// TestABatchFlushIsRefusedForAnyStoreThatFlushesPerStep pins the gate to the
// interface rather than to a list of types (cleat#2350). The wrappers are the
// known-positive: with the gate keyed on *MySQLStore and *MSSQLStore they pass.
func TestABatchFlushIsRefusedForAnyStoreThatFlushesPerStep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store WorkflowStore
		want  bool
	}{
		{"PostgresStore", &PostgresStore{}, true},
		{"MySQLStore", &MySQLStore{}, false},
		{"MSSQLStore", &MSSQLStore{}, false},
		{"a wrapper embedding *MySQLStore", &wrappedMySQLStore{&MySQLStore{}}, false},
		{"a wrapper embedding *MSSQLStore", &wrappedMSSQLStore{&MSSQLStore{}}, false},
		{"any store implementing perStepEventFlusher", aStoreThatIsNotPostgres{}, false},
		{"a test fake that opts back in", aStoreThatStandsInForPostgres{}, true},
		{"a store with no per-step flush", nil, true},
	} {
		if got := batchFlushSupported(tc.store); got != tc.want {
			t.Errorf("batchFlushSupported(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNoProductionStoreOptsBackIntoBatchMode reads the non-test sources for a
// standsInForPostgres method. The opt-in exists for fakes, and a production
// store that implemented it would send its database the PostgreSQL batch writer
// again. Read from source because a store nothing enumerates is exactly what
// this is about.
func TestNoProductionStoreOptsBackIntoBatchMode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, src, 0)
		if err != nil {
			t.Fatalf("UNMEASURED: %s does not parse: %v", f, err)
		}
		scanned++
		for _, d := range parsed.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == "standsInForPostgres" {
				t.Errorf("%s declares standsInForPostgres on a non-test type; only test fakes may opt back into the batch writer", f)
			}
		}
	}
	// The scan has to have read something, or an empty result passes.
	if scanned < 50 {
		t.Fatalf("UNMEASURED: scanned %d non-test files in engine/, expected far more", scanned)
	}
}
