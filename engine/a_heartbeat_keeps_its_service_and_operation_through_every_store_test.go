package engine

// A heartbeat record keeps its service and operation through every store (cleat#4152, G3).
//
// The heartbeat call's dispatcher spec compares the service and operation of EVERY record of a call's run, heartbeats included. The replay loop it
// replaced compared them on the closing call record only and consumed the heartbeats without looking, so a store that dropped those two columns for a
// heartbeat row would not have been noticed before and would now turn every replay of an in-flight heartbeat call into a divergence. This reads the
// rows back through each dialect's real store and pins that they come back as written.
//
// The control is the call record written beside them: a store that dropped the columns for every type fails on it too, and a store that dropped them
// for heartbeats only fails on the heartbeat rows, so the failure names which.

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAHeartbeatKeepsItsServiceAndOperationThroughEveryStore(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			runID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			recs := []EventRecord{
				{Step: 0, EventType: EventTypeHeartbeat, Service: "svc", Op: "op"},
				{Step: 1, EventType: EventTypeHeartbeat, Service: "svc", Op: "op"},
				{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "op", Request: "{}", Response: `{"ok":true}`},
			}
			if err := store.AppendEventHistoryBatch(ctx, runID, recs); err != nil {
				t.Fatalf("AppendEventHistoryBatch: %v", err)
			}
			got, err := store.LoadEventHistory(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			if len(got) != len(recs) {
				t.Fatalf("wrote %d events and read back %d", len(recs), len(got))
			}
			for i, r := range got {
				if r.EventType != recs[i].EventType || r.Service != "svc" || r.Op != "op" {
					t.Errorf("record %d came back as %s service=%q op=%q, want %s service=\"svc\" op=\"op\"", i, r.EventType, r.Service, r.Op, recs[i].EventType)
				}
			}
		})
	}
}
