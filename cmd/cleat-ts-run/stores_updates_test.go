package main

import (
	"context"
	"strings"
	"testing"
)

// The loader refuses what would blame the workflow, at load: a misspelt key would otherwise script a request with nothing in
// it, and the update test would then pass having delivered an empty payload to the handler.
func TestUpdatesFixtureRefusesWhatWouldBlameTheWorkflow(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"not an array", `{"name":"add"}`, "not a JSON array"},
		{"a misspelt key", `[{"name":"add","payloads":{"n":1}}]`, "unknown field"},
		{"a request with no name", `[{"payload":{"n":1}}]`, "has no name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newMemoryUpdatesFromFile(writeFixture(t, t.TempDir(), "u.json", c.body))
			if err == nil {
				t.Fatalf("accepted %s", c.body)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), c.want)
			}
		})
	}
}

// Delivery does not consume a row; completing it does. A store that dropped a request on delivery would lose it whenever a
// handler suspended, which is the case the engine's at-least-once redelivery exists for.
func TestUpdatesStoreDeliversUntilCompleted(t *testing.T) {
	m, err := newMemoryUpdatesFromFile(writeFixture(t, t.TempDir(), "u.json", `[{"name":"add","payload":{"n":5}},{"name":"ping"}]`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for i := 0; i < 2; i++ { // asked twice: delivery is not consumption
		got, _ := m.GetPendingUpdateRequests(ctx, "wf")
		if len(got) != 2 || got[0].UpdateName != "add" || got[0].Payload != `{"n":5}` || got[1].Payload != "" {
			t.Fatalf("pending = %+v, want both requests in order, payload as the JSON text (empty when absent)", got)
		}
	}
	if err := m.CompleteUpdateRequest(ctx, "wf", "req-1", `{"total":5}`, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetPendingUpdateRequests(ctx, "wf")
	if len(got) != 1 || got[0].UpdateName != "ping" {
		t.Fatalf("after completing req-1 pending = %+v, want only ping", got)
	}
	if err := m.CompleteUpdateRequest(ctx, "wf", "req-99", "", ""); err == nil {
		t.Fatal("completing an id the store never issued was accepted: a guest that answers the wrong request would be invisible")
	}
}

// What the envelope reports is what the store saw, in completion order, with the promise settlements.
func TestUpdatesStoreReportsWhatItSaw(t *testing.T) {
	m, err := newMemoryUpdatesFromFile(writeFixture(t, t.TempDir(), "u.json", `[{"name":"a"},{"name":"b"},{"name":"c"}]`))
	if err != nil {
		t.Fatal(err)
	}
	spec := updatesSpec(m)
	ctx := context.Background()
	_ = m.CompleteUpdateRequest(ctx, "wf", "req-2", `"B"`, "")
	_ = m.ResolvePromise(ctx, "prom-2", `"B"`)
	_ = m.CompleteUpdateRequest(ctx, "wf", "req-1", "", "refused")
	_ = m.RejectPromise(ctx, "prom-1", "refused")

	key, v := spec.Extra()
	if key != "updates" {
		t.Fatalf("reports under %q, want updates", key)
	}
	r := v.(map[string]any)
	done := r["completed"].([]completedUpdate)
	if len(done) != 2 || done[0].Name != "b" || done[1].Name != "a" || done[1].Error != "refused" {
		t.Fatalf("completed = %+v, want b then a, in completion order and not request order", done)
	}
	if r["pending"].(int) != 1 {
		t.Fatalf("pending = %v, want 1 (c)", r["pending"])
	}
	if s := r["settled"].([]settledPromise); len(s) != 2 || !s[0].Resolved || s[1].Resolved || s[1].Error != "refused" {
		t.Fatalf("settled = %+v", s)
	}
}

// The wiring: the registered name reaches this loader, and an unscripted run has no such store.
func TestUpdatesStoreIsRegisteredUnderItsName(t *testing.T) {
	f, ok := storeRegistry["updates"]
	if !ok {
		t.Fatalf("no store registered as %q; known: %s", "updates", strings.Join(registeredNames(), ", "))
	}
	if _, err := f(writeFixture(t, t.TempDir(), "u.json", `[]`)); err != nil {
		t.Fatalf("an empty script is a valid fixture: %v", err)
	}
}
