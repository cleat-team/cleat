package engine

// cleat#3903. The update and signal payloads a caller hands the store are the
// caller's bytes, and no dialect may rewrite them.
//
// The defect this pins: on PostgreSQL GetPendingUpdateRequests read the payload
// with `payload #>> '{}'` and then ran json.Compact over the result, so a
// payload that was a JSON STRING whose content is JSON had its content
// compacted (`"{\"a\": 1}"` arrived as `{"a":1}`). MySQL, SQL Server and the
// signal read on every dialect went through decodeJSONPayload, which unwraps
// the string and stops -- so the same call gave different bytes per dialect,
// and on PostgreSQL an update disagreed with a signal carrying the same value.
//
// The assertion is AGREEMENT, not a table of expected strings: every backend
// that ran must read the same input back byte for byte, and on each backend the
// update and the signal must agree with each other. A literal table would
// encode this test's own reading of the two helpers; agreement is the property
// that was broken, and it is the property a future dialect change will break.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

func TestAnUpdatePayloadReadsBackTheSameOnEveryDialect(t *testing.T) {
	// -short is the suite's own switch for "no database": every backend's Setup
	// would skip, the parent would then fail its "measured nothing" guard, and
	// the failure would be right about the fact and wrong about the mode.
	if testing.Short() {
		t.Skip("no database in -short mode; every per-dialect subtest would skip")
	}

	payloads := []struct{ name, payload string }{
		{"an object with spacing", `{"a": 1}`},
		{"the same object compact", `{"a":1}`},
		{"a string holding that object", `"{\"a\": 1}"`},
		{"a string holding it compact", `"{\"a\":1}"`},
		{"a plain string", `hello`},
		{"a JSON string", `"hello"`},
		{"a number", `5`},
		{"a boolean", `true`},
		{"JSON null", `null`},
		{"the empty string", ``},
	}

	type reading struct{ update, signal string }
	// backend -> payload name -> what the two reads handed back. Filled inside
	// the subtests; compared after all of them, which is the only way to compare
	// dialects to each other rather than each to its own expectation.
	got := map[string]map[string]reading{}
	var ran []string

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, base)
			truncateAll(t, base)
			ctx := context.Background()

			wf, _, err := base.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "payload-parity", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			for i, tc := range payloads {
				name := fmt.Sprintf("p%d", i)
				if err := base.CreateUpdateRequest(ctx, wf, name, tc.payload, "promise-"+name); err != nil {
					t.Fatalf("%s: CreateUpdateRequest: %v", tc.name, err)
				}
				if err := base.DeliverSignal(ctx, wf, name, tc.payload); err != nil {
					t.Fatalf("%s: DeliverSignal: %v", tc.name, err)
				}
			}

			updates, err := base.GetPendingUpdateRequests(ctx, wf)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests: %v", err)
			}
			byName := make(map[string]UpdateRequestInfo, len(updates))
			for _, u := range updates {
				byName[u.UpdateName] = u
			}

			got[backend.Name()] = make(map[string]reading, len(payloads))
			for i, tc := range payloads {
				name := fmt.Sprintf("p%d", i)
				u, ok := byName[name]
				if !ok {
					t.Fatalf("%s: the request is missing from the pending list", tc.name)
				}
				sig, found, err := base.PollSignal(ctx, wf, name)
				if err != nil || !found {
					t.Fatalf("%s: PollSignal: found=%v err=%v", tc.name, found, err)
				}
				got[backend.Name()][tc.name] = reading{update: u.Payload, signal: sig.Payload}
			}
			ran = append(ran, backend.Name())
		})
	}

	sort.Strings(ran)
	if len(ran) == 0 {
		t.Fatal("no backend ran, so this test measured nothing")
	}

	for _, tc := range payloads {
		// The third witness, per dialect: an update and a signal carrying the
		// same value must read back the same. This is what PostgreSQL broke on
		// its own, before any cross-dialect comparison.
		for _, b := range ran {
			r := got[b][tc.name]
			if r.update != r.signal {
				t.Errorf("%s on %s: an update reads back %q and a signal carrying the same value reads back %q",
					tc.name, b, r.update, r.signal)
			}
		}
		// And the dialects against each other, which is the property the issue
		// measured as agreeing before this bug.
		for i := 1; i < len(ran); i++ {
			a, b := ran[0], ran[i]
			if got[a][tc.name].update != got[b][tc.name].update {
				t.Errorf("%s: GetPendingUpdateRequests returns %q on %s and %q on %s for the same payload",
					tc.name, got[a][tc.name].update, a, got[b][tc.name].update, b)
			}
		}
	}
}
