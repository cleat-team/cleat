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
	"testing"

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

func TestTypeScriptHostCallsMetadataScopeAndCancellation(t *testing.T) {
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
	e, closeRT := newEngineWith(t, engine.WithWorkflowState(versionedState{version: 7, minVersion: 3}))
	defer closeRT()
	raw, _, susp, _, _, err := e.Execute(context.Background(), wasm, "metadata", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("running the workflow: %v", err)
	}
	if susp != nil {
		// A scope nobody else holds must not suspend; if it does, setScope's binding is being handed the contention answer.
		t.Fatalf("the workflow suspended (%s); nothing in it waits for anything", susp.Reason)
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
