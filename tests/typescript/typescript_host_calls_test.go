package typescript

// The first host calls bound beyond the prototype's subset (cleat#3827): workflow metadata, virtual-object scope and
// cancellation. The bindings are thin, but they are not testable by reading them: the ABI packs each answer into a
// different set of bits, and a binding that decoded the wrong field would return a plausible value rather than an error.
//
// This runs a BUILT module in the in-process engine and compares what the workflow returned against what that engine was
// configured with. Two of the values are the engine's own identifiers, so the comparison is against the harness rather
// than against a literal this file chose -- the same reason the numbers test compares two languages rather than printing
// a result it believes.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

type scopeValue struct {
	ObjectType  string `json:"objectType"`
	InstanceKey string `json:"instanceKey"`
}

type cancellationValue struct {
	Cancelled bool   `json:"cancelled"`
	Reason    string `json:"reason"`
}

// versionedState is a WorkflowState with two DIFFERENT numbers, so the test can tell version() from minVersion(). The
// benchmark harness leaves the engine's state nil, where the host answers 1 for both and a binding that wired the two
// together would pass.
type versionedState struct{ version, minVersion int }

func (s versionedState) Version() int                    { return s.version }
func (s versionedState) MinVersion() int                 { return s.minVersion }
func (s versionedState) ChildVersion(string) (int, bool) { return 0, false }
func (s versionedState) Priority() int                   { return 0 }

// acquiringKeys is a concurrency-key store that either grants every key or refuses every one. Refusing is the
// contention branch in engine/scope.go, which is the only path on which set_scope suspends -- so the two arms below
// are what distinguishes a binding that suspends from one that hands the workflow back a scope it does not hold.
type acquiringKeys struct{ grant bool }

func (k acquiringKeys) AcquireConcurrencyKey(context.Context, string, string, time.Duration) (bool, error) {
	return k.grant, nil
}

func (acquiringKeys) ReleaseConcurrencyKey(context.Context, string, string) (bool, error) {
	return true, nil
}

func runMetadataWorkflow(t *testing.T, extra ...engine.EngineOption) (raw string, suspended bool, reason string) {
	t.Helper()
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/metadata.ts"
	res := build(t, root, src, "metadata")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	e, closeRT := newEngineWith(t, append([]engine.EngineOption{
		engine.WithWorkflowState(versionedState{version: 7, minVersion: 3}),
	}, extra...)...)
	defer closeRT()
	out, _, susp, _, _, err := e.Execute(context.Background(), wasm, "metadata", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the metadata workflow: %v", err)
	}
	if susp != nil {
		return out, true, susp.Reason
	}
	return out, false, ""
}

// TestTypeScriptSetScopeSuspendsRatherThanProceedingWithoutTheKey is the arm the metadata test cannot reach with no
// store at all. set_scope is guarded because the host suspends here; if the binding were not, the workflow would come
// back with a scope it does not hold and run the critical section anyway, which is the mutual-exclusion guarantee the
// scope exists to provide. The granting arm is the control: it proves the suspension below comes from the refusal and
// not from any store being present.
func TestTypeScriptSetScopeSuspendsRatherThanProceedingWithoutTheKey(t *testing.T) {
	t.Run("granted", func(t *testing.T) {
		_, suspended, reason := runMetadataWorkflow(t, engine.WithConcurrencyKeyStore(acquiringKeys{grant: true}))
		if suspended {
			t.Fatalf("the workflow suspended (%s) with every key granted", reason)
		}
	})
	t.Run("refused", func(t *testing.T) {
		raw, suspended, reason := runMetadataWorkflow(t, engine.WithConcurrencyKeyStore(acquiringKeys{grant: false}))
		if !suspended {
			t.Fatalf("the workflow finished (%s) with the scope key held by another run; set_scope must suspend instead of proceeding", raw)
		}
		// The workflow sets two scopes and both are refused, and the engine keeps only the LAST suspend reason, so the key
		// named here is vo:order:o1 rather than vo:cart:c1. Measured, not assumed: the first version of this assertion pinned
		// vo:cart:c1 and failed. What is asserted is the contention branch, not a particular key.
		for _, want := range []string{"virtual object scope vo:", "held by another workflow"} {
			if !strings.Contains(reason, want) {
				t.Fatalf("suspended for %q, want it to name %q (the contention branch in engine/scope.go)", reason, want)
			}
		}
	})
}

// TestTypeScriptACallAfterASuspensionRecordsNothing is what `guarded` on set_scope actually buys, and it is NOT the
// contention suspension the test above covers: measured by unguarding the binding, that test still passes, because the
// engine suspends that run at the end of the segment whatever the guest returns. What changes is the HISTORY — a call
// made after the suspension flag is set must not reach the host, or the segment records a scope the run does not hold.
func TestTypeScriptACallAfterASuspensionRecordsNothing(t *testing.T) {
	root := needToolchain(t)
	src := "tests/typescript/testdata/workflows/late_scope.ts"
	res := build(t, root, src, "late_scope")
	if res.exit != 0 {
		t.Fatalf("building %s failed (exit %d):\n%s", src, res.exit, res.output)
	}
	wasm, err := os.ReadFile(res.module)
	if err != nil {
		t.Fatal(err)
	}
	e, closeRT := newEngineWith(t)
	defer closeRT()
	_, history, susp, _, _, err := e.Execute(context.Background(), wasm, "late_scope", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the workflow: %v", err)
	}
	// The precondition: the segment really did suspend at the sleep. Sleep is local and records no event (see
	// DurableSleep), so the suspend reason is the only evidence that the workflow ran far enough to make the call below.
	if susp == nil {
		t.Fatalf("the workflow did not suspend; ctx.sleep(60000) is supposed to suspend the segment")
	}
	if !strings.Contains(susp.Reason, "cleat_sleep(60000ms)") {
		t.Fatalf("suspended for %q, not at the sleep; the ordering below is then unmeasured", susp.Reason)
	}
	for _, ev := range history {
		if ev.EventType == engine.EventTypeScopeAcquired {
			t.Fatalf("the segment recorded a scope acquisition (%+v) AFTER the call that suspended it; set_scope must be guarded", ev)
		}
	}
}

func TestTypeScriptHostCallsMetadataScopeAndCancellation(t *testing.T) {
	raw, suspended, reason := runMetadataWorkflow(t)
	if suspended {
		// A scope nobody else holds must not suspend; if it does, setScope's binding is being handed the contention answer.
		t.Fatalf("the workflow suspended (%s); nothing in it waits for anything", reason)
	}
	var got struct {
		Version      int64             `json:"version"`
		MinVersion   int64             `json:"minVersion"`
		RunID        string            `json:"runId"`
		WorkflowID   string            `json:"workflowId"`
		Cancellation cancellationValue `json:"cancellation"`
		Before       scopeValue        `json:"before"`
		PrevFirst    string            `json:"prevFirst"`
		During       scopeValue        `json:"during"`
		PrevSecond   string            `json:"prevSecond"`
		After        scopeValue        `json:"after"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("the workflow's result is not the object this test expects: %v\n%.400s", err, raw)
	}

	// Version and MinVersion are the two different numbers the state above was given, so a binding that answered one for
	// the other is caught here rather than by reading the diff.
	if got.Version != 7 {
		t.Errorf("version() = %d, want 7 (the engine state's Version())", got.Version)
	}
	if got.MinVersion != 3 {
		t.Errorf("minVersion() = %d, want 3 (the engine state's MinVersion())", got.MinVersion)
	}
	// Both identifiers come from the engine's workflowID in this harness, so both must equal the constant it was given.
	if got.WorkflowID != benchWF {
		t.Errorf("workflowId() = %q, want %q", got.WorkflowID, benchWF)
	}
	if got.RunID != benchWF {
		t.Errorf("runId() = %q, want %q (the in-process engine runs a workflow under its own id; only a worker sets a run id apart)", got.RunID, benchWF)
	}
	if got.Cancellation.Cancelled || got.Cancellation.Reason != "" {
		t.Errorf("cancellation() = %+v, want not cancelled with no reason; nothing asked this run to cancel", got.Cancellation)
	}

	// The scope sequence, asserted as the host's own forms: empty before, "" as the previous scope the first time, then the
	// `vo:<type>:<key>:` prefix the engine stores (engine/scope.go) as the previous scope on the second call.
	if got.Before != (scopeValue{}) {
		t.Errorf("getScope() before any set = %+v, want empty", got.Before)
	}
	if got.PrevFirst != "" {
		t.Errorf("the first setScope() reported the previous scope as %q, want empty", got.PrevFirst)
	}
	if got.During != (scopeValue{ObjectType: "cart", InstanceKey: "c1"}) {
		t.Errorf("getScope() after setScope(cart, c1) = %+v, want cart/c1", got.During)
	}
	if got.PrevSecond != "vo:cart:c1:" {
		t.Errorf("the second setScope() reported the previous scope as %q, want the host's vo:cart:c1: form", got.PrevSecond)
	}
	if got.After != (scopeValue{ObjectType: "order", InstanceKey: "o1"}) {
		t.Errorf("getScope() after setScope(order, o1) = %+v, want order/o1", got.After)
	}
}
