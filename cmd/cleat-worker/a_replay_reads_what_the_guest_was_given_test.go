package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3581 asked whether a value the engine redacts reaches a REPLAY as
// "[REDACTED]", so that a workflow which branched on it the first time branches
// the other way the second time. The worker redacts new events before it
// persists them (setup.go), and replay reads history.
//
// It does not, for the two events measured here, and this test is that
// measurement kept. A real PostgreSQL, a real wasmtime guest and a real worker:
// the guest takes one branch if it sees a value and the other if it does not,
// the run suspends, and a second segment replays it. The branches are
// DIFFERENT calls, so a replay that read a different value shows up as a
// different call.
//
// THE CONTROL IS PART OF THE TEST. A "replayed as it was" result proves nothing
// about redaction if redaction would not have touched the value, and the first
// version of this measurement used a body that engine.Redact leaves alone. So
// every subtest first asserts that engine.Redact REWRITES the body it is about
// to feed the guest, and a body that redaction ignores fails the test rather
// than passing it.
//
// WHY IT HOLDS, and so what would break it: the engine flushes each event as it
// happens, unredacted; the segment-end append upserts only PENDING await rows;
// and replay reads the payload JSONB, which LoadEventHistory fills in after its
// read-side redaction and never redacts. Remove the payload override and the
// replay reads the redacted column: that is the falsification.
//
// What this does NOT say is that redaction is safe. The same facts mean a
// flushed call's response is stored, and served by the history API, unredacted
// (cleat#3584).

// aBodyRedactionRewrites carries a secret under a key redaction must always
// catch, and the guest branches on that secret, so a replay that read the
// redacted copy takes the other branch. It must stay a body Redact rewrites;
// the test checks.
//
// The marker is built from two pieces, and so is the guest's: a secret scanner
// reads a whole `"api_key":"<value>"` literal as a credential, and this is a
// placeholder that must not be one.
const secretMarker = "sk-live-" + "SECRET123"

const aBodyRedactionRewrites = `{"api_key":"` + secretMarker + `","version":"1.2.3"}`

func TestReplayReadsWhatTheGuestWasGiven(t *testing.T) {
	if got := engine.Redact(aBodyRedactionRewrites); got == aBodyRedactionRewrites {
		t.Fatalf("CONTROL: engine.Redact does not rewrite %s, so nothing below says anything about redaction", aBodyRedactionRewrites)
	}

	for _, tc := range []struct {
		name  string
		entry string
		// childEntry is set for the case where the value arrives as a child's result.
		childEntry string
	}{
		{name: "a host call's response", entry: "BranchOnServiceVersion"},
		{name: "an awaited child's result", entry: "BranchOnChildResult", childEntry: "ChildAnswersVersion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testutil.SuiteTestDB(t, "cleat_worker")
			store := engine.NewPostgresStore(db)

			wasmBytes := buildDeferFixture(t)
			meta, err := wasm.ReadMetadata(wasmBytes)
			if err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			parentName, childName := "replay-parent-"+suffix, "replay-reads-child"
			for _, n := range []string{parentName, childName} {
				if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
					Name: n, Version: meta.WorkflowVersion, WASMBytes: wasmBytes,
					ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
				}); err != nil {
					t.Fatalf("DeployWorkflowDef %s: %v", n, err)
				}
			}
			wfID := "replay-reads-" + suffix
			input := json.RawMessage(fmt.Sprintf(`{"__entry_point":%q}`, tc.entry))
			if _, _, err := store.StartNewRun(ctx, wfID, parentName, meta.WorkflowVersion,
				input, "", engine.DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			var mu sync.Mutex
			var ops []string
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				op := strings.TrimPrefix(r.URL.Path, "/call/")
				mu.Lock()
				ops = append(ops, op)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if op == "info/version" {
					fmt.Fprint(w, aBodyRedactionRewrites)
					return
				}
				fmt.Fprint(w, `{}`)
			}))
			defer svc.Close()

			segments := 0
			run := func(id string) {
				t.Helper()
				segments++
				logs := &syncBuffer{}
				w := newRealDeferPhaseWorker(t, db, store, logs)
				w.id = fmt.Sprintf("replay-reads-%s-%d", suffix, segments)
				w.serviceEndpoints = map[string]string{"info": svc.URL}
				w.egress = &engine.EgressGuard{AllowLoopback: true, TenantOptional: func(context.Context) bool { return true }}
				claimed := claimOne(t, ctx, store, w.id, id)
				w.inflight.Store(id, claimed)
				runExecuteWorkflow(w, claimed)
			}
			opsSeen := func() []string {
				mu.Lock()
				defer mu.Unlock()
				return append([]string(nil), ops...)
			}

			run(wfID) // segment 1: the guest is given the live value
			if tc.childEntry != "" {
				// The child's id is in the parent's own child_workflow event.
				var childID string
				if err := db.QueryRowContext(ctx, `SELECT payload->>'run_id' FROM event_history WHERE workflow_id=$1 AND event_type='child_workflow'`, wfID).Scan(&childID); err != nil {
					t.Fatalf("finding the child: %v", err)
				}
				run(childID)
				if err := simulateCrashReclaim(ctx, db, wfID); err != nil {
					t.Fatalf("making the parent claimable: %v", err)
				}
				run(wfID) // the await resolves live here
			}

			// What reached the table, decoded: the unredacted value, which is the
			// premise of the replay result and the fact cleat#3584 is about.
			var stored string
			q := `SELECT COALESCE(payload->>'response_b64','') FROM event_history WHERE workflow_id=$1 AND event_type=$2 ORDER BY step LIMIT 1`
			eventType := "call"
			if tc.childEntry != "" {
				eventType = "await_child"
			}
			if err := db.QueryRowContext(ctx, q, wfID, eventType).Scan(&stored); err != nil {
				t.Fatalf("reading the stored %s: %v", eventType, err)
			}
			if tc.childEntry == "" {
				if dec, _ := base64.StdEncoding.DecodeString(stored); !strings.Contains(string(dec), secretMarker) {
					t.Fatalf("the stored response is %q, want the unredacted body: the premise of this test changed", dec)
				}
			}

			// The sleep has to be over for the next segment to get past it.
			time.Sleep(1500 * time.Millisecond)
			if err := simulateCrashReclaim(ctx, db, wfID); err != nil {
				t.Fatalf("making the parent claimable: %v", err)
			}
			run(wfID) // the replay

			if got := statusOf(t, ctx, store, wfID); got != "done" {
				t.Errorf("status after the replay = %q, want done", got)
			}
			seen := opsSeen()
			var live, redacted int
			for _, o := range seen {
				switch o {
				case "info/saw-live":
					live++
				case "info/saw-redacted":
					redacted++
				}
			}
			if redacted != 0 {
				t.Errorf("a replay took the branch for a REDACTED value: calls %v. The guest was given the live value the first time", seen)
			}
			if live == 0 {
				t.Errorf("the guest never took the live branch: calls %v (the premise of this test failed to run)", seen)
			}
		})
	}
}
