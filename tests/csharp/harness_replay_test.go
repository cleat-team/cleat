//go:build cgo

package csharp

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// The harness's Replay option has to REPLAY: read the recorded history instead of executing again. A replay whose result matches a fresh
// run proves nothing when the guest makes no host call, because executing and replaying agree (cleat-review found the 5.1 replay test
// passing with Replay ignored: the smoke entry makes no call). This one uses a call whose replay is observable two ways, and tampers
// with what is recorded so a replay that ignores the history cannot agree.
func TestCSharpHarnessReplayReadsTheRecordedHistoryNotTheStore(t *testing.T) {
	first := &fakePromiseStore{}
	opts := func(s *fakePromiseStore) []engine.EngineOption {
		return []engine.EngineOption{engine.WithPromiseStore(s)}
	}
	fresh := mustFinish(t, runConformanceEntry(t, "b_create_promise", entryRun{WorkflowID: "wf-rp", Input: `{"name":"approval","ttl_ms":0}`, Options: opts(first)}))
	if len(first.created) != 1 {
		t.Fatalf("the fresh run created %d promises, want 1", len(first.created))
	}
	run := runConformanceEntry(t, "b_create_promise", entryRun{WorkflowID: "wf-rp", Input: `{"name":"approval","ttl_ms":0}`, Options: opts(first)})
	hist := append([]engine.EventRecord{}, run.History...)

	// 1. An untampered replay returns the recorded id and creates nothing: it did not execute.
	second := &fakePromiseStore{}
	replayed := mustFinish(t, runConformanceEntry(t, "b_create_promise", entryRun{WorkflowID: "wf-rp", Input: `{"name":"approval","ttl_ms":0}`, Options: opts(second), Replay: hist}))
	if len(second.created) != 0 {
		t.Errorf("a replay created %d promise(s) in the store: it executed instead of replaying", len(second.created))
	}
	if replayed["id"] == nil || replayed["id"] == "" {
		t.Errorf("the replay returned no id: %v", replayed)
	}

	// 2. A tampered history is what the replay returns. A harness that ignored Replay would return the store's id instead.
	tampered := append([]engine.EventRecord{}, hist...)
	found := false
	for i := range tampered {
		if tampered[i].EventType == engine.EventTypeCreatePromise {
			tampered[i].PromiseID = "tampered-promise-id"
			found = true
		}
	}
	if !found {
		t.Fatalf("no create_promise record to tamper with in %+v", hist)
	}
	third := &fakePromiseStore{}
	got := mustFinish(t, runConformanceEntry(t, "b_create_promise", entryRun{WorkflowID: "wf-rp", Input: `{"name":"approval","ttl_ms":0}`, Options: opts(third), Replay: tampered}))
	if got["id"] != "tampered-promise-id" {
		t.Errorf("replay over a tampered history returned id %v, want the tampered one: the history was not what the guest replayed (a fresh run returned %v)", got["id"], fresh["id"])
	}
}
