package engine

import (
	"context"
	"strings"
	"testing"
)

// A sharded deployment and write-ahead call intent were built independently,
// and nothing connected them: ShardedStore implemented WorkflowStore and not
// callIntentStore, so Engine.intentStore() refused it and
// freshCallWithIntent returned WITHOUT DISPATCHING. Every durable call on a
// sharded worker would have failed the moment the write-ahead default was
// turned on -- which is cleat#1778's whole subject, and why this is its
// prerequisite rather than a detail of it.
//
// The refusal was correct behaviour from intentStore(), whose doc comment
// chooses a loud failure over a silent downgrade to at-least-once. What was
// missing is the capability, not the check.

// intentCapableShardStore is a shard store that CAN honour the guarantee.
//
// The shard stores are deliberately intent-capable so that a refusal cannot
// be blamed on them. If this engine still refuses, only the ShardedStore in
// front of them can be responsible -- which is what makes the known-positive
// below diagnostic rather than merely red.
type intentCapableShardStore struct {
	stubWorkflowStore
	name      string
	history   []EventRecord
	intents   []EventRecord
	completed []EventRecord
	resolved  []EventRecord
}

// LoadEventHistory serves whatever this shard was seeded with, so a test can
// put a pending row on one shard and assert the operator path finds it there.
func (s *intentCapableShardStore) LoadEventHistory(_ context.Context, _ string) ([]EventRecord, error) {
	return s.history, nil
}

func (s *intentCapableShardStore) ResolveCallIntent(_ context.Context, _ string, rec EventRecord, _ []byte, _ string, _ int64, _ []EventRecord) error {
	s.resolved = append(s.resolved, rec)
	return nil
}

func (s *intentCapableShardStore) WriteCallIntent(_ context.Context, _ string, rec EventRecord, _ string, _ int64) error {
	s.intents = append(s.intents, rec)
	return nil
}

func (s *intentCapableShardStore) CompleteCallIntent(_ context.Context, _ string, rec EventRecord, _ []byte, _ string, _ string, _ int64) error {
	s.completed = append(s.completed, rec)
	return nil
}

// shardedOverIntentCapableStores builds a two-shard store whose shards both
// honour write-ahead intent, and returns it with the two shards.
func shardedOverIntentCapableStores(t *testing.T) (*ShardedStore, *intentCapableShardStore, *intentCapableShardStore) {
	t.Helper()
	a := &intentCapableShardStore{name: "shard-a"}
	b := &intentCapableShardStore{name: "shard-b"}
	ss, err := NewShardedStore(
		[]ShardConfig{{Name: "shard-a"}, {Name: "shard-b"}},
		[]WorkflowStore{a, b},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}
	return ss, a, b
}

// TestShardedStore_HonoursWriteAheadCallIntent is the known-positive: before
// the delegation existed this failed at intentStore(), with the error that
// names the missing capability.
func TestShardedStore_HonoursWriteAheadCallIntent(t *testing.T) {
	ss, _, _ := shardedOverIntentCapableStores(t)
	e := NewEngine(nil, &mockCaller{},
		WithWorkflowStore(ss),
		WithWriteAheadIntentOps(intentService+"."+intentOperation))

	if got := e.callSemantics(intentService, intentOperation); got != WriteAheadIntent {
		t.Fatalf("callSemantics = %v, want WriteAheadIntent", got)
	}

	st, err := e.intentStore()
	if err != nil {
		t.Fatalf("a sharded store over intent-capable shards was refused: %v\n"+
			"Both shards implement callIntentStore, so the refusal is the ShardedStore's own: "+
			"freshCallWithIntent would return without dispatching, and every durable call on a "+
			"sharded worker fails.", err)
	}
	if st == nil {
		t.Fatal("intentStore returned a nil store and no error")
	}
}

// TestShardedStore_CallIntentLandsOnTheOwningShard is the assertion that a
// weaker test would miss.
//
// "It reached a shard" is satisfied by a delegation that always picks shard 0.
// That would put the intent on one shard and the workflow's history on
// another, so the completion, the replay and the operator's reconciliation
// query would all look in a different database from the one holding the
// pending row -- an ambiguous call that cannot be found.
//
// The expected shard is not hardcoded. It is read from where an EXISTING
// routed method sends the same workflow ID, so this test cannot disagree with
// the routing it is checking, and it still fails if the intent goes elsewhere.
func TestShardedStore_CallIntentLandsOnTheOwningShard(t *testing.T) {
	ctx := context.Background()

	// Two IDs that route to different shards, found rather than assumed --
	// a pair that happened to share a shard would make this test vacuous.
	ss, a, b := shardedOverIntentCapableStores(t)
	var idA, idB string
	for _, id := range []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"44444444-4444-4444-4444-444444444444",
		"55555555-5555-5555-5555-555555555555",
	} {
		switch ss.getShard(id).Store {
		case WorkflowStore(a):
			if idA == "" {
				idA = id
			}
		case WorkflowStore(b):
			if idB == "" {
				idB = id
			}
		}
	}
	if idA == "" || idB == "" {
		// Fatal, not Skip. getShard is sha256 over these literals, so whether
		// they cover both shards is a fixed property of the code and not an
		// environmental precondition -- this branch is either always taken or
		// never taken. Never, today. If a change to the hash makes it true,
		// the test has stopped distinguishing correct routing from a constant
		// and must say so rather than quietly stop measuring.
		t.Fatalf("no pair of the sampled ids routes to two different shards (a=%q b=%q): "+
			"add ids until both shards are covered, or this test cannot fail", idA, idB)
	}

	st, err := intentStoreFor(t, ss)
	if err != nil {
		t.Fatalf("intentStore: %v", err)
	}

	if err := st.WriteCallIntent(ctx, idA, EventRecord{Step: 1, Service: intentService, Op: intentOperation}, "w", 1); err != nil {
		t.Fatalf("WriteCallIntent(%s): %v", idA, err)
	}
	if err := st.WriteCallIntent(ctx, idB, EventRecord{Step: 2, Service: intentService, Op: intentOperation}, "w", 1); err != nil {
		t.Fatalf("WriteCallIntent(%s): %v", idB, err)
	}

	if len(a.intents) != 1 || len(b.intents) != 1 {
		t.Fatalf("intents landed %d on shard-a and %d on shard-b, want one each: "+
			"a delegation that ignores the workflow id puts the pending row in a different "+
			"database from the history it belongs to", len(a.intents), len(b.intents))
	}
	if a.intents[0].Step != 1 {
		t.Errorf("shard-a holds step %d, want 1 -- the two intents were swapped, so routing "+
			"disagrees with getShard", a.intents[0].Step)
	}
	if b.intents[0].Step != 2 {
		t.Errorf("shard-b holds step %d, want 2", b.intents[0].Step)
	}
}

// TestShardedStore_CallIntentFailsLoudlyOnAnIncapableShard keeps the property
// intentStore() was protecting. A sharded store is only as capable as the
// shard the workflow lands on, and one incapable shard must refuse for the
// workflows it owns rather than silently downgrading them to at-least-once.
func TestShardedStore_CallIntentFailsLoudlyOnAnIncapableShard(t *testing.T) {
	ctx := context.Background()
	capable := &intentCapableShardStore{name: "capable"}
	ss, err := NewShardedStore(
		[]ShardConfig{{Name: "capable"}, {Name: "incapable"}},
		[]WorkflowStore{capable, &stubWorkflowStore{}},
		[]func() error{func() error { return nil }, func() error { return nil }},
	)
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	// Find an id owned by the incapable shard.
	var id string
	for _, cand := range []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"44444444-4444-4444-4444-444444444444",
	} {
		if _, ok := ss.getShard(cand).Store.(callIntentStore); !ok {
			id = cand
			break
		}
	}
	if id == "" {
		// Fatal for the same reason as above: deterministic, so a miss means
		// the test no longer reaches the incapable shard it exists to check.
		t.Fatal("none of the sampled ids routes to the incapable shard: this test would pass " +
			"without ever exercising the capability check")
	}

	st, err := intentStoreFor(t, ss)
	if err != nil {
		t.Fatalf("intentStore: %v", err)
	}
	err = st.WriteCallIntent(ctx, id, EventRecord{Step: 1}, "w", 1)
	if err == nil {
		t.Fatal("a workflow on a shard with no intent support was accepted: the call would be " +
			"dispatched believing a pending row exists, which is the silent downgrade " +
			"intentStore() refuses to allow")
	}
	if !strings.Contains(err.Error(), "write-ahead call intent") {
		t.Errorf("error = %v, want it to name the missing capability the way intentStore does", err)
	}
	if len(capable.intents) != 0 {
		t.Errorf("the capable shard received %d intents for a workflow it does not own",
			len(capable.intents))
	}
}

// intentStoreFor builds an engine over store and returns its intent store.
func intentStoreFor(t *testing.T, store WorkflowStore) (callIntentStore, error) {
	t.Helper()
	e := NewEngine(nil, &mockCaller{},
		WithWorkflowStore(store),
		WithWriteAheadIntentOps(intentService+"."+intentOperation))
	return e.intentStore()
}

// TestShardedStore_OperatorCanResolveAnAmbiguousCall covers a live defect
// rather than a prerequisite for cleat#1778's default change.
//
// ResolveStep is the operator's way out of an ambiguous call -- they ask the
// external service what happened and assert the answer. It takes a
// WorkflowStore and type-asserts callIntentResolver (engine/admin_intent.go:62).
// A ShardedStore failed that assertion, so on a sharded deployment the answer
// was "store *engine.ShardedStore cannot resolve call intents" and there was
// no way to resolve the call at all -- with any write-ahead op declared, today,
// without waiting for the default to change.
//
// The resolution must land on the shard holding the history. resolveAmbiguity
// and ResolveStep both read the history through the same routing, so a
// resolution written to a different shard would leave the pending row exactly
// where it was and report success.
//
// IT RESOLVES ONE WORKFLOW PER SHARD, and that is not thoroughness -- it is
// what makes the test able to fail. A first version used a single workflow id
// and a routing mutation (getShard("") instead of getShard(workflowID)) left
// it GREEN: with two shards, "" and that particular id both hash to shard-a,
// so the constant router happened to be right for the one case being checked.
// A routing test that exercises one destination cannot distinguish correct
// routing from a constant, whatever the destination is.
func TestShardedStore_OperatorCanResolveAnAmbiguousCall(t *testing.T) {
	ctx := context.Background()
	ss, a, b := shardedOverIntentCapableStores(t)

	// One workflow per shard, found rather than assumed.
	perShard := map[*intentCapableShardStore]string{}
	for _, id := range []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"44444444-4444-4444-4444-444444444444",
		"55555555-5555-5555-5555-555555555555",
	} {
		owner := ss.getShard(id).Store.(*intentCapableShardStore)
		if _, seen := perShard[owner]; !seen {
			perShard[owner] = id
		}
	}
	if len(perShard) != 2 {
		// Fatal, not Skip -- deterministic, per the note in the sibling test.
		t.Fatalf("the sampled ids cover %d of 2 shards: a one-shard sample cannot tell correct "+
			"routing from a constant, so this would pass without measuring", len(perShard))
	}

	for _, shard := range []*intentCapableShardStore{a, b} {
		wfID := perShard[shard]
		shard.history = []EventRecord{{
			Step:      0,
			EventType: EventTypeCall,
			Service:   intentService,
			Op:        intentOperation,
			Request:   `{"amount":100}`,
			Pending:   true,
		}}

		if err := ResolveStep(ctx, ss, wfID, 0, `{"charged":true}`, "operator@example.com"); err != nil {
			t.Fatalf("ResolveStep(%s) over a sharded store: %v\n"+
				"An operator on a sharded deployment has no other way to settle an ambiguous call.",
				wfID, err)
		}
	}

	for _, shard := range []*intentCapableShardStore{a, b} {
		if len(shard.resolved) != 1 {
			t.Fatalf("shard %q recorded %d resolutions, want exactly 1: a resolution written to a "+
				"shard that does not hold the history leaves the pending row pending and reports "+
				"success", shard.name, len(shard.resolved))
		}
		if got := shard.resolved[0].ResolvedBy; got != "operator@example.com" {
			t.Errorf("shard %q: ResolvedBy = %q, want the operator: the provenance is what keeps an "+
				"asserted outcome distinguishable from an observed one", shard.name, got)
		}
		if shard.resolved[0].Pending {
			t.Errorf("shard %q: the resolved record is still marked pending", shard.name)
		}
	}
}
