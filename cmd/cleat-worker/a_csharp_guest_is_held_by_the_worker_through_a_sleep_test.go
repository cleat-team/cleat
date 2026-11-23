package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#4162. The engine's C# held rows (engine/a_real_csharp_component_is_held_through_a_sleep_and_fenced_test.go) run a real C# component through Engine.Execute. This is the
// same guest through the WORKER's own dispatch path (claim, executeWorkflow, the worker's wasmtime backend and limits, finalize), which is the path a deployment runs and the
// engine rows do not touch. It asks one thing, observed while the guest sleeps and not inferred afterwards: is a C# run HELD in this worker (in its resident set, status
// `running`), and does it then finish in place?
//
// The control is the same run with residency off, which must suspend after its first call; without it "both calls ran in one execution" is also what a guest that never sleeps
// does.
//
// What this does not show: the language's memory charge (the engine row pins it; the worker's set is not readable from here), and a SIGNAL wake (the checked-in fixture has a
// timer mode only; a signal row needs a rebuilt fixture).

// csharpSleepFixture is the checked-in C# defer-order component, stamped the way `cleat build --target csharp` stamps a real build. Unstamped it is detected as python.
func csharpSleepFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "engine", "testdata", "csharp_defer_order", "defer_order.wasm"))
	if err != nil {
		t.Fatalf("read the C# fixture: %v", err)
	}
	// The worker, unlike Engine.Execute("run", ...), has to be TOLD the entry point, so the stamp carries one, as nonGoMetadata in cmd/cleat/build_metadata.go does for a real build.
	b, err = wasm.WriteMetadata(b, &wasm.Metadata{
		WorkflowName: "csharp-held", WorkflowVersion: 1, ABIVersion: wasm.CurrentABIVersion, MinCompatibleVersion: wasm.CurrentABIVersion,
		Language: "csharp", EntryPoints: []string{"run"},
	})
	if err != nil {
		t.Fatalf("stamp the fixture: %v", err)
	}
	if got := wasm.DetectLanguage(b); got != "csharp" {
		t.Fatalf("PRECONDITION FAILED: the stamped fixture is detected as %q, not csharp, so this row would run a Python-charged guest", got)
	}
	return b
}

type csharpSleepOutcome struct {
	sawHeld     bool   // the run was in the worker's resident set at some sample
	statusHeld  string // its store status at that moment
	finalStatus string
	ops         []string
}

func runCSharpThroughASleep(t *testing.T, residency bool) csharpSleepOutcome {
	t.Helper()
	ctx := context.Background()
	db := testutil.SuiteTestDB(t, "cleat_worker")
	store := engine.NewPostgresStore(db)
	wasmBytes := csharpSleepFixture(t)

	defName := fmt.Sprintf("csharp-held-%d", time.Now().UnixNano())
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{Name: defName, Version: 1, WASMBytes: wasmBytes, ABIVersion: wasm.CurrentABIVersion, MinVersion: wasm.CurrentABIVersion}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	wfID := fmt.Sprintf("csharp-held-%d", time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, 1, json.RawMessage(`{"mode":"sleep"}`), "", engine.DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	var mu sync.Mutex
	var ops []string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ops = append(ops, strings.TrimPrefix(r.URL.Path, "/call/"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer svc.Close()
	oldSvcURL := *benchSvcURL
	*benchSvcURL = svc.URL
	defer func() { *benchSvcURL = oldSvcURL }()

	logs := &syncBuffer{}
	w := newRealDeferPhaseWorker(t, db, store, logs)
	w.id = "csharp-held-worker"
	w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
	if residency {
		cfg := engine.DefaultResidencyConfig()
		cfg.MaxRuns = 2
		set, err := engine.NewResidentSet(cfg)
		if err != nil {
			t.Fatalf("NewResidentSet: %v", err)
		}
		w.resident = set
	}

	claimed := claimOne(t, ctx, store, w.id, wfID)
	w.inflight.Store(wfID, claimed)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runExecuteWorkflow(w, claimed)
	}()

	out := csharpSleepOutcome{}
	// The first C# run in a process pays for a cold component compile before it ever reaches its sleep, so the wait is generous. With residency on it ends at the first sample that
	// shows the run in the resident set; with it off the execution simply returns at the suspension.
	deadline := time.Now().Add(90 * time.Second)
poll:
	for residency && time.Now().Before(deadline) {
		select {
		case <-done:
			break poll
		case <-time.After(2 * time.Millisecond):
		}
		if w.resident != nil && w.resident.Len() > 0 { // nil when the worker has no resident set: that is "never held", not a crash
			out.sawHeld = true
			out.statusHeld = statusOf(t, ctx, store, wfID)
			break
		}
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("the execution never finished (residency=%v; logs:\n%s)", residency, logs.String())
	}
	out.finalStatus = statusOf(t, ctx, store, wfID)
	if out.finalStatus == "failed" {
		if wf, err := store.GetWorkflowByID(ctx, wfID); err == nil {
			t.Logf("residency=%v: the run FAILED: code=%q op=%q error=%q; worker log:\n%s", residency, wf.ErrorCode, wf.ErrorOp, wf.Error, logs.String())
		}
	}
	mu.Lock()
	out.ops = append([]string(nil), ops...)
	mu.Unlock()
	if out.finalStatus == "ready" || out.finalStatus == "running" || out.finalStatus == "terminating" {
		// A run left claimable would be picked up by whichever test next claims from this shared database.
		if err := store.TerminateWorkflow(ctx, wfID, "csharp held-row cleanup"); err != nil {
			t.Errorf("cleaning up the run that was left %q: %v", out.finalStatus, err)
		}
	}
	return out
}

func TestACSharpGuestIsHeldByTheWorkerThroughASleepAndFinishesInPlace(t *testing.T) {
	// The control first: without residency the same guest SUSPENDS after its first call (the execution returns, the run reads `ready`). Without this, "both calls ran in one
	// execution" is also what a guest that never sleeps does.
	off := runCSharpThroughASleep(t, false)
	if off.sawHeld || off.finalStatus != "ready" || len(off.ops) != 1 {
		t.Fatalf("PRECONDITION FAILED (residency off): sawHeld=%v final=%q ops=%v, want the run suspended (ready) after exactly one call", off.sawHeld, off.finalStatus, off.ops)
	}

	on := runCSharpThroughASleep(t, true)
	if !on.sawHeld {
		t.Fatalf("the C# run was never in the worker's resident set (final=%q ops=%v): it was not held", on.finalStatus, on.ops)
	}
	// Held means RUNNING in the store: it keeps its claim and its status (design 4.2, option (a)), unlike the suspended control above.
	if on.statusHeld != "running" {
		t.Errorf("while held the run's store status was %q, want running", on.statusHeld)
	}
	if on.finalStatus != "done" {
		t.Errorf("the held run ended %q, want done", on.finalStatus)
	}
	if want := []string{"payments/Charge", "payments/Charge"}; strings.Join(on.ops, ",") != strings.Join(want, ",") {
		t.Errorf("the service saw %v, want %v: both calls in one execution", on.ops, want)
	}
}
