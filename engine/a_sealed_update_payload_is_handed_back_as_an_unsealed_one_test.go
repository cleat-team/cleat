package engine

// cleat#3852. Turning on --encrypt-sensitive-payloads must not change what an
// update handler is handed.
//
// Unsealed, PostgreSQL reads workflow_update_requests.payload with
// `payload #>> '{}'`, which strips a JSON string scalar's quotes. Sealed, the
// column holds base64 ciphertext inside a JSON string, so the same operator
// hands the reader the ciphertext, the decrypt returns the JSON text that was
// sealed, and nothing unwraps it. Every payload that is a string, a JSON null
// or the empty string therefore read back differently depending on whether the
// deployment was sealed -- `"yes"` arrived quoted, `hello` arrived quoted,
// `"{\"a\":1}"` arrived as JSON text rather than a compacted object, `null`
// arrived as the four characters `null` and the empty payload as `""`.
//
// This asserts the INVARIANT rather than a table of expected strings: for each
// payload shape, a sealed store and an unsealed store over the SAME database
// must answer byte for byte the same, for both an update payload and a signal
// payload. Equality rather than a literal is the point -- a table of literals
// would encode this test's own reading of decodeJSONPayload, and the defect was
// that both readings were individually plausible.
//
// Signals are included because they share encodeJSONPayload/decodeJSONPayload
// and, unlike updates, already unwrapped a JSON string scalar on the sealed
// path (PollSignal decrypts and then calls decodeJSONPayload, and it reads the
// jsonb column itself rather than `#>>`). They passed before the fix; they are
// here so a future change cannot make one path transparent and the other not,
// and the string-wrapped-object shape below is the one clew delivers.
//
// The `result` and `error_msg` columns are read on the same row, are decrypted,
// and are NOT unwrapped, so they carry the same asymmetry the payload had. They
// are asserted instead of fixed: a pending row cannot carry either today
// (CompleteUpdateRequest writes a result and moves status off 'pending' in one
// statement), and the test asserts both are empty on both arms AND equal between
// them. That converts "these columns are out of scope because nothing sets them
// on a pending row" from a claim about consumers into a measurement -- and a
// change that starts setting one fails here rather than shipping the asymmetry.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestASealedUpdatePayloadIsHandedBackAsAnUnsealedOne(t *testing.T) {
	// The payload shapes measured on cleat#3852: objects, a number, a boolean,
	// a JSON string, a plain (non-JSON) string, a string containing a quote, a
	// JSON null, the empty string, a string whose content is an object in both
	// spacings (clew's signal shape), a number beyond float64's exact range, and
	// a non-ASCII string.
	payloads := []struct{ name, payload string }{
		{"an object", `{"a":1}`},
		{"an object with spacing and an array", `{"a": 1,"b":[1, 2]}`},
		{"a number", `5`},
		{"a boolean", `true`},
		{"a JSON string", `"yes"`},
		{"a plain string", `hello`},
		{"a string containing a quote", `"a\"b"`},
		{"a JSON null", `null`},
		{"the empty string", ``},
		{"a string whose content is an object", `"{\"a\":1}"`},
		{"a string whose content is a spaced object", `"{\"a\": 1}"`},
		{"an integer beyond float64's exact range", `{"big":123456789012345678901234567890}`},
		{"a non-ASCII string", `"héllo"`},
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, base)
			truncateAll(t, base)
			ctx := context.Background()

			enc, err := NewPayloadEncryption(validKey(t))
			if err != nil {
				t.Fatalf("NewPayloadEncryption: %v", err)
			}
			// The sealed arm is a second STORE over the same database, not a
			// second database: the comparison has to be same-dialect, same rows'
			// worth of fixtures, one reader with the key and one without.
			//
			// MySQL and SQL Server run the UNSEALED arm alone. cleat#3906 made
			// `WithEncryption(enc, true)` panic on those dialects -- it used to
			// accept an option that half-sealed a store, and a configuration that
			// cannot seal is now refused at construction -- so there is no second
			// store over the same database to compare against. The refusal itself
			// is pinned in TestEnablingPayloadEncryptionIsRefusedOnADialectThatCannotSeal.
			var sealed WorkflowStore
			switch s := base.(type) {
			case *PostgresStore:
				sealed = s.WithEncryption(enc, true)
			case *MySQLStore, *MSSQLStore:
				// sealed stays nil; see the comment above.
			default:
				t.Fatalf("%T is not a dialect this test knows; seal it deliberately rather than reporting a pass on a store this test did not exercise", base)
			}
			plain := base

			wfPlain, _, err := plain.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "sealed-payload-plain", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (unsealed): %v", err)
			}
			var wfSealed string
			if sealed != nil {
				var serr error
				wfSealed, _, serr = sealed.StartNewRun(ctx, "", "test-workflow", 1,
					json.RawMessage(`{}`), "sealed-payload-sealed", DefaultTenantUUID, 0)
				if serr != nil {
					t.Fatalf("StartNewRun (sealed): %v", serr)
				}
			}

			for i, tc := range payloads {
				name := fmt.Sprintf("p%d", i)
				if err := plain.CreateUpdateRequest(ctx, wfPlain, name, tc.payload, "promise-"+name); err != nil {
					t.Fatalf("%s: CreateUpdateRequest on the unsealed store: %v", tc.name, err)
				}
				if err := plain.DeliverSignal(ctx, wfPlain, name, tc.payload); err != nil {
					t.Fatalf("%s: DeliverSignal on the unsealed store: %v", tc.name, err)
				}
				if sealed != nil {
					if err := sealed.CreateUpdateRequest(ctx, wfSealed, name, tc.payload, "promise-"+name); err != nil {
						t.Fatalf("%s: CreateUpdateRequest on the sealed store: %v", tc.name, err)
					}
					if err := sealed.DeliverSignal(ctx, wfSealed, name, tc.payload); err != nil {
						t.Fatalf("%s: DeliverSignal on the sealed store: %v", tc.name, err)
					}
				}
			}

			// Precondition, stated beside the verdict: only PostgreSQL seals, so
			// only there can this test measure the sealed read at all. Check the
			// row really is ciphertext rather than trusting that WithEncryption
			// still reaches the write path -- otherwise a store that silently
			// stopped sealing would make every comparison below a tautology.
			if backend.Name() == "postgres" {
				var onDisk string
				if err := rawDBOf(t, base).QueryRowContext(ctx,
					`SELECT payload #>> '{}' FROM workflow_update_requests WHERE workflow_id = $1 AND update_name = 'p5'`,
					wfSealed).Scan(&onDisk); err != nil {
					t.Fatalf("read the sealed row's stored bytes: %v", err)
				}
				if strings.Contains(onDisk, "hello") {
					t.Fatalf("the 'hello' update payload is stored as %q, so this test is not reading a sealed row", onDisk)
				}
			}

			plainUpdates, err := plain.GetPendingUpdateRequests(ctx, wfPlain)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests (unsealed): %v", err)
			}
			if len(plainUpdates) != len(payloads) {
				t.Fatalf("pending updates (unsealed): %d, want %d", len(plainUpdates), len(payloads))
			}
			byName := func(rs []UpdateRequestInfo) map[string]UpdateRequestInfo {
				m := make(map[string]UpdateRequestInfo, len(rs))
				for _, r := range rs {
					m[r.UpdateName] = r
				}
				return m
			}
			plainByName := byName(plainUpdates)

			var sealedByName map[string]UpdateRequestInfo
			if sealed != nil {
				sealedUpdates, serr := sealed.GetPendingUpdateRequests(ctx, wfSealed)
				if serr != nil {
					t.Fatalf("GetPendingUpdateRequests (sealed): %v", serr)
				}
				if len(sealedUpdates) != len(payloads) {
					t.Fatalf("pending updates (sealed): %d, want %d", len(sealedUpdates), len(payloads))
				}
				sealedByName = byName(sealedUpdates)
			}

			for i, tc := range payloads {
				name := fmt.Sprintf("p%d", i)
				pr, onPlain := plainByName[name]
				if !onPlain {
					t.Fatalf("%s: the request is missing from the unsealed pending list", tc.name)
				}
				pl, foundPlain, err := plain.PollSignal(ctx, wfPlain, name)
				if err != nil || !foundPlain {
					t.Fatalf("%s: PollSignal (unsealed): found=%v err=%v", tc.name, foundPlain, err)
				}
				if sealed == nil {
					// The unsealed arm alone (MySQL and SQL Server, cleat#3906):
					// the row and its signal are readable and carry no result.
					if pr.Result != "" || pr.ErrorMsg != "" {
						t.Errorf("%s: a PENDING update row carries result=%q error_msg=%q; cleat#3852 leaves those columns alone on the assumption they are empty here",
							tc.name, pr.Result, pr.ErrorMsg)
					}
					continue
				}

				sr, onSealed := sealedByName[name]
				if !onSealed {
					t.Fatalf("%s: the request is missing from the sealed pending list", tc.name)
				}
				if sr.Payload != pr.Payload {
					t.Errorf("%s: the update payload read back as %q with encryption on, %q with it off", tc.name, sr.Payload, pr.Payload)
				}

				// cleat#3852 does not touch `result` or `error_msg`, and these two
				// assertions are the reason stated as a measurement rather than as a
				// claim about consumers. A pending row cannot carry either today:
				// CompleteUpdateRequest is the only writer and it sets status and
				// result in one statement, so the row stops being pending. If a later
				// change lets a pending row carry one, THIS fails -- instead of the
				// same read asymmetry shipping quietly through the other two columns,
				// which decrypt and never unwrap.
				if pr.Result != "" || pr.ErrorMsg != "" || sr.Result != "" || sr.ErrorMsg != "" {
					t.Errorf("%s: a PENDING update row carries result=%q error_msg=%q (unsealed) / %q %q (sealed); cleat#3852 leaves those columns alone on the assumption they are empty here",
						tc.name, pr.Result, pr.ErrorMsg, sr.Result, sr.ErrorMsg)
				}
				if sr.Result != pr.Result || sr.ErrorMsg != pr.ErrorMsg {
					t.Errorf("%s: result/error_msg read back as %q/%q with encryption on, %q/%q with it off",
						tc.name, sr.Result, sr.ErrorMsg, pr.Result, pr.ErrorMsg)
				}

				sl, foundSealed, err := sealed.PollSignal(ctx, wfSealed, name)
				if err != nil || !foundSealed {
					t.Fatalf("%s: PollSignal (sealed): found=%v err=%v", tc.name, foundSealed, err)
				}
				if sl.Payload != pl.Payload {
					t.Errorf("%s: the signal payload read back as %q with encryption on, %q with it off", tc.name, sl.Payload, pl.Payload)
				}
			}
		})
	}
}
