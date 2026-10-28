package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// cleat#3511: WorkflowLoader.Deploy carried its own upsert, and the statement had
// `disabled_at IS NULL` inside the SET list, which PostgreSQL rejects as a syntax
// error before it runs. Its three tests drove a mock database that accepts any
// statement, so they passed against it. These run against a real PostgreSQL, and the
// first one is red against that statement: Deploy returned the syntax error and wrote
// nothing.
//
// The loader is PostgreSQL-only like every other method on it (every query uses $N),
// so unlike the store's deploy tests these have no other dialect to run on.
func TestAWorkflowLoaderDeployWritesARowTheStoreReads(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	l := NewWorkflowLoader(db, nil, nil)

	wasm := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0a}
	const name = "loader-deploy-writes"
	if err := l.Deploy(ctx, name, 1, wasm, map[string]string{"p": "v1"}, 1); err != nil {
		t.Fatalf("Deploy returned an error: %v", err)
	}
	got, err := store.LoadWASM(ctx, name, 1)
	if err != nil {
		t.Fatalf("Deploy reported success but the store cannot read the definition: %v", err)
	}
	if !bytes.Equal(got, wasm) {
		t.Fatalf("stored bytes = %x, want %x", got, wasm)
	}
	if err := l.Deploy(ctx, name, 1, wasm, map[string]string{"p": "v1"}, 1); err != nil {
		t.Fatalf("redeploying identical bytes returned an error: %v", err)
	}
}

// The deploy rule is the store's, not a second copy: replacing a version's bytes while
// a run of it is open is refused through the loader as well (cleat#3497).
func TestAWorkflowLoaderDeployRefusesToReplaceBytesUnderAnOpenRun(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	l := NewWorkflowLoader(db, nil, nil)

	bytesA := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0a}
	bytesB := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x0b}
	const name = "loader-deploy-refuses"
	if err := l.Deploy(ctx, name, 1, bytesA, nil, 1); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}
	id, existed, err := store.StartNewRun(ctx, "", name, 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
	if err != nil || existed {
		t.Fatalf("UNMEASURED: could not start a run (existed=%v): %v", existed, err)
	}
	inst, err := store.GetWorkflowByID(ctx, id)
	if err != nil || inst == nil || map[string]bool{"done": true, "failed": true, "dead_lettered": true, "terminated": true, "cancelled": true}[inst.Status] {
		t.Fatalf("UNMEASURED: the run is not open (%v, %v), so the refusal below would not be tested", inst, err)
	}

	if err := l.Deploy(ctx, name, 1, bytesB, nil, 1); !errors.Is(err, ErrWorkflowDefInUse) {
		t.Fatalf("Deploy of different bytes under an open run returned %v, want ErrWorkflowDefInUse", err)
	}
	if got, err := store.LoadWASM(ctx, name, 1); err != nil || !bytes.Equal(got, bytesA) {
		t.Fatalf("a refused Deploy changed the stored bytes: have %x (%v), want %x", got, err, bytesA)
	}
}
