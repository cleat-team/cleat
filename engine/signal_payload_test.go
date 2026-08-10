package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestSignalPayloadRoundTripsOnEveryDialect pins a behavioural difference
// between the dialects that was live in shipped code.
//
// All three schemas require the workflow_signals.payload column to hold valid
// JSON, and each says so differently: PostgreSQL's column is JSONB, MySQL's is
// JSON, SQL Server's is NVARCHAR(MAX) with CHECK (ISJSON(payload) = 1). Only
// PostgresStore.DeliverSignal knew that -- it wrapped a non-JSON payload in
// quotes before writing, and decodeJSONPayload unwrapped it on the way out.
// MySQLStore and MSSQLStore did neither, so an opaque payload was accepted on
// PostgreSQL and rejected outright on the other two:
//
//	Error 3140 (22032): Invalid JSON text: "Invalid value." at position 0
//
// DeliverSignal is reachable from the worker's signal endpoint, so this was a
// live difference in what the product accepts, not a test artefact.
//
// The cases are chosen to separate the two halves of the problem: whether the
// write is accepted at all, and whether what comes back is what went in. A JSON
// object exercises the second on its own, because every one of these column
// types normalises whitespace on round-trip.
func TestSignalPayloadRoundTripsOnEveryDialect(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"json object", `{"data":"hello"}`},
		{"bare string", "payload-1"},
		{"empty string", ""},
		// A quote and a backslash are the reason encodeJSONPayload marshals
		// rather than concatenating quote characters, which is what the
		// PostgreSQL path used to do: `"` + `he"llo` + `"` is `"he"llo"`, which
		// is not valid JSON and is rejected by the very column the wrapping
		// exists to satisfy.
		{"embedded quote", `he"llo`},
		{"embedded backslash", `C:\path\to`},
		{"json array", `[1,2,3]`},
	}

	dialects := []struct {
		name    string
		dialect testutil.Dialect
	}{
		{"postgres", testutil.DialectPostgres},
		{"mysql", testutil.DialectMySQL},
		{"mssql", testutil.DialectMSSQL},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			switch d.dialect {
			case testutil.DialectMySQL:
				testutil.SetupMySQLFullSchema(t, db)
			case testutil.DialectMSSQL:
				testutil.SetupMSSQLFullSchema(t, db)
			default:
				testutil.SetupFullSchema(t, db, d.dialect)
			}
			defer db.Close()

			store := storeFor(t, d.dialect, db)
			ctx := context.Background()

			for i, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					wfID := "signal-roundtrip-" + d.name
					seedWorkflowInstance(t, db, d.dialect, wfID)

					sigName := fmt.Sprintf("sig-%d", i)
					if err := store.DeliverSignal(ctx, wfID, sigName, tc.payload); err != nil {
						t.Fatalf("DeliverSignal(%q) on %s: %v\n\n"+
							"the payload column requires valid JSON on every "+
							"dialect; DeliverSignal is what makes an opaque "+
							"payload satisfy it", tc.payload, d.name, err)
					}

					got, found, err := store.PollSignal(ctx, wfID, sigName)
					if err != nil {
						t.Fatalf("PollSignal on %s: %v", d.name, err)
					}
					if !found {
						t.Fatalf("PollSignal on %s: signal not found", d.name)
					}
					if !sameJSONOrString(got, tc.payload) {
						t.Errorf("round-trip on %s: sent %q, got back %q",
							d.name, tc.payload, got)
					}
				})
			}
		})
	}
}

// sameJSONOrString compares what came back with what went in, allowing the
// whitespace normalisation every one of these column types performs on JSON
// but nothing else. Comparing raw bytes would fail on `{"data":"hello"}`
// returning as `{"data": "hello"}`, which is not a defect; comparing as JSON
// unconditionally would let a non-JSON payload come back altered unnoticed.
func sameJSONOrString(got, want string) bool {
	if got == want {
		return true
	}
	if !json.Valid([]byte(want)) || !json.Valid([]byte(got)) {
		return false
	}
	var a, b any
	if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
