package engine

// cleat#3822. POST /api/workflows/{id}/update/{name} takes its body as the update's payload, whatever it is, so
// `null` is a payload. PostgresStore.GetPendingUpdateRequests read the column with `payload #>> '{}'`, which
// returns SQL NULL for JSON null, and scanned it into a string: "converting NULL to string is unsupported". That
// read is how the update route checks for a pending update of the same name, how the guest polls for work, and
// how the sweeper looks for stranded requests, so one `null` update made every later read of that workflow's
// pending list fail: the route answered 500 to every update request for the run, and poll_update logged the error
// and reported no work, so the null update was never delivered and its caller never got an answer.
//
// MySQL and SQL Server returned an empty payload for it. This pins that all three agree and that the list is
// still readable beside an ordinary request.
//
// Deliberately NOT asserted here: what a string payload reads back as, beyond the null case below. Unsealed it
// is unquoted on every dialect (`"yes"` reads back `yes`, `hello` reads back `hello`), by the contract that a
// payload which was not JSON is stored wrapped and handed back as passed (decodeJSONPayload), shared with signals.
// When this test was written the sealed PostgreSQL arm broke that contract instead, coming back as the JSON text
// the value was sealed from; cleat#3852 made that read go through decodeJSONPayload too, so both arms now agree
// and TestASealedUpdatePayloadIsHandedBackAsAnUnsealedOne holds the whole payload set to it.

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAJSONNullUpdatePayloadDoesNotBreakThePendingList(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		for _, sealed := range []bool{false, true} {
			sealed := sealed
			name := backend.Name() + "/plain"
			if sealed {
				name = backend.Name() + "/sealed"
			}
			t.Run(name, func(t *testing.T) {
				base, teardown := backend.Setup(t)
				defer teardown()
				setupTestData(t, base)
				truncateAll(t, base)

				store := base
				if sealed {
					enc, err := NewPayloadEncryption(validKey(t))
					if err != nil {
						t.Fatalf("NewPayloadEncryption: %v", err)
					}
					switch s := base.(type) {
					case *PostgresStore:
						store = s.WithEncryption(enc, true)
					case *MySQLStore:
						store = s.WithEncryption(enc, true)
					case *MSSQLStore:
						store = s.WithEncryption(enc, true)
					default:
						t.Fatalf("%T has no WithEncryption; seal it deliberately rather than reporting a pass on a store this test did not exercise", base)
					}
				}
				ctx := context.Background()

				wf, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "null-update", DefaultTenantUUID, 0)
				if err != nil {
					t.Fatalf("StartNewRun: %v", err)
				}
				if err := store.CreateUpdateRequest(ctx, wf, "first", `{"n":1}`, "p-first"); err != nil {
					t.Fatalf("CreateUpdateRequest(object): %v", err)
				}
				if err := store.CreateUpdateRequest(ctx, wf, "second", `null`, "p-second"); err != nil {
					t.Fatalf("CreateUpdateRequest(null): %v", err)
				}

				pending, err := store.GetPendingUpdateRequests(ctx, wf)
				if err != nil {
					t.Fatalf("GetPendingUpdateRequests with a JSON null payload pending: %v", err)
				}
				got := map[string]string{}
				for _, r := range pending {
					got[r.UpdateName] = r.Payload
				}
				if len(got) != 2 {
					t.Fatalf("pending = %v, want both requests listed", got)
				}
				if got["first"] != `{"n":1}` {
					t.Errorf("the object payload read back as %q, want %q", got["first"], `{"n":1}`)
				}
				// MySQL and SQL Server read a null payload as an empty string, and PostgreSQL does too sealed or
				// unsealed: cleat#3852 routed the sealed read through decodeJSONPayload, whose json.Unmarshal of
				// a bare `null` into a string is a no-op that returns "", the same answer COALESCE gives the
				// unsealed read. This arm pinned the OLD sealed answer (`null`) until then; the update is the fix,
				// and the sealed arm is kept so the table stays complete rather than silently losing a column.
				if got["second"] != "" {
					t.Errorf("the null payload read back as %q, want %q", got["second"], "")
				}
			})
		}
	}
}
