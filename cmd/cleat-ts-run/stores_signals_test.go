package main

import (
	"context"
	"testing"
)

// A sent signal is recorded; it joins this run's own queue only when addressed to this run. The second half is the one that
// could be wrong silently: a store that queued every send would let a workflow read back a signal it sent to somebody else.
func TestSignalsStoreRecordsSendsAndQueuesOnlyThoseAddressedToItself(t *testing.T) {
	ctx := context.Background()
	m := newMemorySignals(nil)
	m.self = "me"
	_ = m.DeliverSignal(ctx, "other", "go", `"x"`)
	_ = m.DeliverSignal(ctx, "me", "ping", `{"n":1}`)

	if _, found, _ := m.PollSignal(ctx, "me", "go"); found {
		t.Fatal("a signal addressed to another run became visible to this run's poll")
	}
	if d, found, _ := m.PollSignal(ctx, "me", "ping"); !found || d.Payload != `{"n":1}` {
		t.Fatalf("a signal the run sent to itself was not delivered: found=%v %+v", found, d)
	}
	got := m.reportInto(nil)["signalsSent"].([]sentSignal)
	if len(got) != 2 || got[0] != (sentSignal{"other", "go", `"x"`}) || got[1].Target != "me" {
		t.Fatalf("signalsSent = %+v, want both sends in order", got)
	}
}

// With no self id (how a unit test builds the store) every delivery queues, as before it recorded sends; and a run that sends
// nothing reports nothing, so an existing envelope does not grow a key.
func TestSignalsStoreWithoutAnIdQueuesEverythingAndAnIdleRunReportsNothing(t *testing.T) {
	ctx := context.Background()
	m := newMemorySignals(nil)
	if m.reportInto(nil) != nil {
		t.Fatal("a run that sent nothing reported a key")
	}
	_ = m.DeliverSignal(ctx, "anyone", "go", "1")
	if _, found, _ := m.PollSignal(ctx, "x", "go"); !found {
		t.Fatal("with no self id a delivery was dropped")
	}
	if extras := m.reportInto(map[string]any{"updates": 1}); extras["updates"] != 1 || extras["signalsSent"] == nil {
		t.Fatalf("reportInto did not merge into the existing extras: %v", extras)
	}
}

// `Extras: signals.reportInto(extrasFrom(specs))` composes only if reportInto ADDS to the map it is given and returns that same
// map: a fresh map or a copy would silently drop every registered store's key (pluginCalls, fetchRequests, updates, ...), and the
// tests asserting on them would fail as if the plugin were wrong. This drives the real extrasFrom with a registered spec.
func TestSignalsReportIntoKeepsTheRegistryStoresKeys(t *testing.T) {
	ctx := context.Background()
	m := newMemorySignals(nil)
	_ = m.DeliverSignal(ctx, "other", "go", "1")

	specs := []storeSpec{
		{Extra: func() (string, any) { return "pluginCalls", []string{"a"} }},
		{Extra: func() (string, any) { return "updates", 3 }},
	}
	registry := extrasFrom(specs)
	got := m.reportInto(registry)

	for _, k := range []string{"pluginCalls", "updates", "signalsSent"} {
		if _, ok := got[k]; !ok {
			t.Errorf("extras lost %q: have %v", k, got)
		}
	}
	// The SAME map, not a copy: a caller holding the original sees the new key too.
	if _, ok := registry["signalsSent"]; !ok {
		t.Error("reportInto returned a different map from the one it was given")
	}
	// And with no registry store at all, the signals key still arrives (extrasFrom returns nil then).
	if only := m.reportInto(extrasFrom(nil)); only["signalsSent"] == nil {
		t.Errorf("with no registry stores the signals entry was lost: %v", only)
	}
}
