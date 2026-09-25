package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
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

// TestEveryStoreThatFlushesPerStepIsRefusedBatchMode ties the deny list in
// batchFlushSupported to the interface that says a store's database is not
// PostgreSQL. A store on a fourth dialect has to implement flushEventForStep to
// flush at all; when it does, this fails until the gate names it.
//
// Read from source, not from the running binary, because a store that is
// missing from the list is exactly a value nothing else enumerates.
func TestEveryStoreThatFlushesPerStepIsRefusedBatchMode(t *testing.T) {
	stores := map[string]WorkflowStore{
		"MySQLStore": &MySQLStore{},
		"MSSQLStore": &MSSQLStore{},
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
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
		for _, d := range parsed.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "flushEventForStep" || len(fn.Recv.List) != 1 {
				continue
			}
			recv := fn.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if id, ok := recv.(*ast.Ident); ok {
				found = append(found, id.Name)
			}
		}
	}
	sort.Strings(found)

	// The scan has to see something, or an empty set passes every check below.
	if len(found) < 2 {
		t.Fatalf("UNMEASURED: found %d non-test flushEventForStep implementations (%v), expected at least MySQLStore and MSSQLStore", len(found), found)
	}
	for _, name := range found {
		store, ok := stores[name]
		if !ok {
			t.Errorf("%s implements flushEventForStep, so its database is not PostgreSQL, but batchFlushSupported and this test do not name it. "+
				"Add it to the deny list in engine/engine.go and to the map above.", name)
			continue
		}
		if batchFlushSupported(store) {
			t.Errorf("batchFlushSupported(%s) is true; the batch writer is PostgreSQL SQL", name)
		}
	}
	if !batchFlushSupported(&PostgresStore{}) {
		t.Errorf("batchFlushSupported(*PostgresStore) is false; batch mode is what PostgreSQL uses")
	}
}
