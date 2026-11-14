package engine

// cleat#3807. A promise result and a workflow result are JSON, and they have to read back as the JSON that was
// stored. On PostgreSQL they did not when the value was a scalar and was not sealed: the read used
// `result #>> '{}'`, which strips the quotes from a string and turns null into SQL NULL, so `"yes"` read back as
// `yes` and `null` as nothing. Promise[string].Await then failed in json.Unmarshal, and GET /api/workflows/:id
// returned `result: yes`, which is not JSON. Objects, arrays, numbers and booleans survived, which is why nobody
// saw it.
//
// Every shape is written and read back on every registered backend, sealed and unsealed, through all four reads:
// GetPromise, ListPromises, GetWorkflowByID, and (for the promise) the unmarshal a typed Await does. Sealed rows
// are in the table on purpose -- the fix keeps `#>>` for the decrypt step, and a version that read the exact text
// everywhere would have broken them.
//
// SQL Server is in the table too. It used to refuse every scalar through CHECK ck_workflow_promises_result /
// ck_workflow_instances_result (isjson(x) is 0 for a bare scalar); the SQL Server migration 019_a_json_scalar_result_is_stored.sql (cleat#3816) uses isjson(x, VALUE).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

type resultShape struct {
	json   string // what is written, already compact
	scalar bool
	into   func(string) error // what a typed reader does with it
}

func resultShapes() []resultShape {
	str := func(want string) func(string) error {
		return func(s string) error {
			var got string
			if err := json.Unmarshal([]byte(s), &got); err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("unmarshalled %q, want %q", got, want)
			}
			return nil
		}
	}
	return []resultShape{
		{`"yes"`, true, str("yes")},
		{`"a\"b"`, true, str(`a"b`)},
		{`""`, true, str("")},
		{`"héllo"`, true, str("héllo")},
		{`"abcd"`, true, str("abcd")}, // looks like base64: must not be mistaken for a sealed value
		{`5`, true, nil},
		{`1.5`, true, nil},
		{`true`, true, nil},
		{`false`, true, nil},
		{`null`, true, nil},
		{`{"by":"alice"}`, false, nil},
		{`[1,2]`, false, nil},
	}
}

func TestAScalarResultReadsBackExactlyAsStored(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		for _, sealed := range []bool{false, true} {
			sealed := sealed
			t.Run(fmt.Sprintf("%s/sealed=%v", backend.Name(), sealed), func(t *testing.T) {
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
				ps := store.(PromiseStore)
				lister := store.(interface {
					ListPromises(context.Context, string) ([]PromiseInfo, error)
				})
				ctx := context.Background()

				shapes := resultShapes()
				if len(shapes) == 0 {
					t.Fatal("no shapes to run")
				}

				// The workflow's own result, through the read the API serves. First, while no other run is ready:
				// ClaimWorkflow takes whichever one is, and resolving a promise (below) wakes its run.
				for i, sh := range shapes {
					sh := sh
					t.Run("workflow/"+sh.json, func(t *testing.T) {
						wf, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`),
							fmt.Sprintf("scalar-wf-%d", i), DefaultTenantUUID, 0)
						if err != nil {
							t.Fatalf("StartNewRun: %v", err)
						}
						claimed, err := store.ClaimWorkflow(ctx, "worker-1")
						if err != nil || claimed == nil || claimed.ID != wf {
							t.Fatalf("ClaimWorkflow = %v, %v; want run %s", claimed, err, wf)
						}
						if err := store.CompleteWorkflow(ctx, claimed.ID, "worker-1", claimed.Generation, sh.json, nil); err != nil {
							t.Fatalf("CompleteWorkflow(%s): %v", sh.json, err)
						}
						got, err := store.GetWorkflowByID(ctx, claimed.ID)
						if err != nil || got == nil {
							t.Fatalf("GetWorkflowByID: %v, %v", got, err)
						}
						if got.Result != sh.json {
							t.Errorf("GetWorkflowByID result = %q, want %q: the API serves this as the run's result", got.Result, sh.json)
						}
					})
				}

				// A run per shape, all started before any promise is resolved: resolving wakes its run, and
				// publishRun claims "the next ready run".
				ids := make([]string, len(shapes))
				for i := range shapes {
					ids[i] = publishRun(t, store, fmt.Sprintf("scalar-%d", i), nil)
				}

				for i, sh := range shapes {
					sh := sh
					t.Run(sh.json, func(t *testing.T) {
						id, pid := ids[i], fmt.Sprintf("scalar-p-%d", i)

						if err := ps.CreatePromise(ctx, id, "n", pid); err != nil {
							t.Fatalf("CreatePromise: %v", err)
						}
						// Pending first: a promise nobody resolved must read back as nothing, not as `null`.
						if _, got, _, err := ps.GetPromise(ctx, id, pid); err != nil || got != "" {
							t.Fatalf("a pending promise's GetPromise result = %q, %v; want empty", got, err)
						}
						if err := ps.ResolvePromise(ctx, pid, sh.json); err != nil {
							t.Fatalf("ResolvePromise(%s): %v", sh.json, err)
						}

						status, got, _, err := ps.GetPromise(ctx, id, pid)
						if err != nil || status != "resolved" {
							t.Fatalf("GetPromise = %q, %q, %v", status, got, err)
						}
						if got != sh.json {
							t.Errorf("GetPromise result = %q, want %q: the guest is handed this text and unmarshals it", got, sh.json)
						}
						if sh.into != nil {
							if err := sh.into(got); err != nil {
								t.Errorf("a typed Await cannot read GetPromise's result %q: %v", got, err)
							}
						}

						list, err := lister.ListPromises(ctx, id)
						if err != nil || len(list) != 1 {
							t.Fatalf("ListPromises = %+v, %v", list, err)
						}
						if list[0].Result != sh.json {
							t.Errorf("ListPromises result = %q, want %q", list[0].Result, sh.json)
						}
					})
				}

			})
		}
	}
}

// A promise that was never resolved, and a run that never finished, read back as nothing. exactJSONText is the
// one place that decides between the extracted and the exact text, and an empty raw column has to leave the
// empty value alone.
func TestExactJSONTextLeavesAnEmptyColumnEmpty(t *testing.T) {
	for _, c := range []struct{ name, extracted, decrypted, raw, want string }{
		{"never written", "", "", "", ""},
		{"an unsealed string", "yes", "yes", `"yes"`, `"yes"`},
		{"json null", "", "", "null", "null"},
		{"an empty string", "", "", `""`, `""`},
		{"a sealed value keeps what the decrypt returned", "Zm9v", `"yes"`, `"Zm9v"`, `"yes"`},
		{"an object", `{"a": 1}`, `{"a": 1}`, `{"a": 1}`, `{"a": 1}`},
	} {
		if got := exactJSONText(c.extracted, c.decrypted, c.raw); got != c.want {
			t.Errorf("%s: exactJSONText(%q, %q, %q) = %q, want %q", c.name, c.extracted, c.decrypted, c.raw, got, c.want)
		}
	}
}

// extractedJSONText must agree with what PostgreSQL's `#>> '{}'` returns, because the decrypt step reads the
// extraction. The live test above proves it end to end for every shape; this pins the rule itself.
func TestExtractedJSONTextIsWhatTextExtractionReturned(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"", ""},
		{"null", ""},
		{`"yes"`, "yes"},
		{`"a\"b"`, `a"b`},
		{`""`, ""},
		{`"Zm9v"`, "Zm9v"}, // a sealed value: the bare base64 the decrypt step reads
		{`5`, "5"},
		{`true`, "true"},
		{`{"by": "alice"}`, `{"by": "alice"}`},
		{`[1, 2]`, `[1, 2]`},
	} {
		if got := extractedJSONText(c.raw); got != c.want {
			t.Errorf("extractedJSONText(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
