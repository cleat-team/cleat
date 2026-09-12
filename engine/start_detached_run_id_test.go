package engine

import (
	"context"
	"strings"
	"testing"
)

// TestStartDetachedReturnsTheIDThatAddressesTheRun is cleat#1154.
//
// RunDetached computes a run id and discards it, so a workflow that starts a
// detached run has no handle to it -- it cannot poll it, signal it, or record
// it anywhere durable. StartDetached is the same work with the id written back.
//
// ASSERTING THE ID IS NON-EMPTY WOULD PROVE NOTHING, and that is the whole
// point of this test. engine/children.go mints a fallback id --
// fmt.Sprintf("detached-%s-%d", name, s.stepCount) -- whenever the store call
// does not produce one, and that string is non-empty, well-formed, and
// addresses nothing at all. A test that checked only for a non-empty return
// would pass against a StartDetached whose store was never consulted.
//
// So the assertion is the round trip the issue asked for: take the id back to
// the store and check that the run behind it is the run that was started.
func TestStartDetachedReturnsTheIDThatAddressesTheRun(t *testing.T) {
	const detachedDef = "detached-reconcile"

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			if _, ok := store.(childWorkflowStarter); !ok {
				t.Fatalf("%T cannot start a child workflow, so this test would "+
					"measure the fallback id rather than a real run", store)
			}

			parent := newIntentWorkflow(t, ctx, store, "start-detached-parent")
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: detachedDef, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef(%s): %v", detachedDef, err)
			}

			s := &execSession{
				workflowID: parent,
				engine:     NewEngine(nil, &mockCaller{}, WithWorkflowStore(store), WithChildWorkflowStore(store)),
			}

			// The guest's output buffer. writeResult prefers the raw buffer on
			// the context over m.Memory(), which is how the wasmtime host
			// functions hand it over, so m can be nil here.
			buf := make([]byte, 4096)
			memCtx := ctxWithMem(ctx, buf)

			result := s.StartDetached(memCtx, nil, detachedDef, `{"id":7}`, 0, uint32(len(buf)))
			if errCode := uint32(result); errCode != 0 {
				t.Fatalf("StartDetached returned errCode %d (%#x), want 0", errCode, result)
			}
			written := uint32(uint64(result) >> 32)
			if written == 0 {
				t.Fatal("StartDetached wrote 0 bytes, so it reported success and handed " +
					"back nothing -- the defect cleat#1154 describes, unchanged")
			}
			runID := string(buf[:written])

			// The fallback id is a real string that addresses no row. Name it
			// here so a regression that stops consulting the store fails with
			// the reason rather than with a confusing "not found".
			if strings.HasPrefix(runID, "detached-"+detachedDef+"-") {
				t.Fatalf("StartDetached returned %q, which is the fallback id children.go "+
					"mints when the child store produced nothing. The store was not "+
					"consulted, or its call failed silently.", runID)
			}
			if runID == parent {
				t.Fatalf("StartDetached returned the PARENT's id %q", runID)
			}

			// The round trip.
			wf, err := store.GetWorkflowByID(ctx, runID)
			if err != nil {
				t.Fatalf("GetWorkflowByID(%q): %v -- the id StartDetached returned does "+
					"not address a run", runID, err)
			}
			if wf == nil {
				t.Fatalf("GetWorkflowByID(%q) returned no row: the id is well-formed and "+
					"addresses nothing", runID)
			}
			if wf.DefName != detachedDef {
				t.Errorf("the run behind %q is %q, want %q -- the id came back but points "+
					"at a different run", runID, wf.DefName, detachedDef)
			}

			// And the event was recorded, so a replay hands back the SAME id
			// rather than starting a second run. Both calls share one body for
			// exactly this reason; asserting it here is what makes the sharing
			// a property rather than a coincidence of the current code.
			if len(s.history) != 1 {
				t.Fatalf("recorded %d events, want 1", len(s.history))
			}
			if got := s.history[0].EventType; got != EventTypeRunDetached {
				t.Errorf("recorded event type %q, want %q -- a history written by "+
					"StartDetached must replay against RunDetached and back",
					got, EventTypeRunDetached)
			}
			if got := s.history[0].DetachedRunID; got != runID {
				t.Errorf("recorded DetachedRunID %q, returned %q; a replay would hand the "+
					"guest an id the original execution never gave it", got, runID)
			}

			replay := &execSession{
				workflowID: parent,
				engine:     s.engine,
				isReplay:   true,
				history:    s.history,
			}
			replayBuf := make([]byte, 4096)
			rr := replay.StartDetached(ctxWithMem(ctx, replayBuf), nil,
				detachedDef, `{"id":7}`, 0, uint32(len(replayBuf)))
			if errCode := uint32(rr); errCode != 0 {
				t.Fatalf("replayed StartDetached returned errCode %d", errCode)
			}
			if got := string(replayBuf[:uint32(uint64(rr)>>32)]); got != runID {
				t.Errorf("replay returned %q, the original execution returned %q", got, runID)
			}
		})
	}
}

// childWorkflowStarter is the store capability StartDetached needs. Declared
// rather than asserted inline so the failure above names what is missing.
type childWorkflowStarter interface {
	StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string,
		defVersion int, parentClosePolicy string, priority int) (string, error)
}
