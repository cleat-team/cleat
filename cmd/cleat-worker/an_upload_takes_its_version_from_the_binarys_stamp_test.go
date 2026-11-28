package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3513. A worker only runs a binary under the version it is stamped with: its
// pre-flight returns a run to the queue when the stamp and the row's version differ.
// POST /api/definitions used to take the next free number when "version" was left out
// and store the binary unchanged, so a default build (stamp 1) uploaded twice made row
// 2 with a binary that says 1, and a stamp of 0 became row 1; neither version ever ran.
// The upload now takes the version from the stamp, refuses a requested version that
// disagrees with it, and refuses a stamp that is not positive. A binary with no
// metadata keeps the old behaviour. On real stores of all three dialects, since the
// version a row is written under is the store's.
func TestAnUploadTakesItsVersionFromTheBinarysStamp(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			var store engine.WorkflowStore
			switch be.Dialect {
			case testutil.DialectPostgres:
				store = engine.NewPostgresStore(be.DB)
			case testutil.DialectMySQL:
				store = engine.NewMySQLStore(be.DB)
			case testutil.DialectMSSQL:
				// A plain pool sets no SESSION_CONTEXT, so a store built on it reads
				// nothing under row-level security; production goes through the factory.
				factory := engine.NewMSSQLStoreFactory(os.Getenv("CLEAT_TEST_MSSQL"))
				s, closer, err := factory.OpenStore(ctx, engine.DefaultTenantUUID, "default")
				if err != nil {
					t.Fatalf("OpenStore on %s: %v", be.Name, err)
				}
				t.Cleanup(func() { _ = closer.Close() })
				store = s
			default:
				t.Fatalf("unhandled dialect %s", be.Dialect)
			}

			api := &apiServer{store: store, worker: newTestWorker(&mockStore{}), maxBodySize: 1 << 20}
			mux := http.NewServeMux()
			registerRoutes(mux, api)

			// A module with a header, a distinguishing name and the given stamp. The stamp is
			// written through WriteMetadata, which does not validate, so 0 can be made.
			binary := func(t *testing.T, name string, stamp int) []byte {
				t.Helper()
				out, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"),
					&wasm.Metadata{WorkflowName: name, WorkflowVersion: stamp, ABIVersion: 1, MinCompatibleVersion: 1})
				if err != nil {
					t.Fatalf("WriteMetadata: %v", err)
				}
				return out
			}
			upload := func(t *testing.T, name string, version int, wasmBytes []byte) (int, string) {
				t.Helper()
				body := map[string]any{"name": name, "wasm_bytes_base64": base64.StdEncoding.EncodeToString(wasmBytes)}
				if version > 0 {
					body["version"] = version
				}
				raw, _ := json.Marshal(body)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(raw))))
				return rec.Code, rec.Body.String()
			}
			rows := func(t *testing.T, name string) []int {
				t.Helper()
				defs, err := store.ListWorkflowDefs(ctx, name)
				if err != nil {
					t.Fatalf("ListWorkflowDefs(%s): %v", name, err)
				}
				var vs []int
				for _, d := range defs {
					vs = append(vs, d.Version)
				}
				return vs
			}
			unique := func(base string) string { return base + "-" + uuid.New().String()[:8] }
			// uploaded performs the upload and requires a 201, returning the version it was given.
			uploaded := func(t *testing.T, name string, version int, wasmBytes []byte) int {
				t.Helper()
				code, body := upload(t, name, version, wasmBytes)
				if code != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body = %s", code, body)
				}
				var resp struct {
					Version int `json:"version"`
				}
				if err := json.Unmarshal([]byte(body), &resp); err != nil {
					t.Fatalf("response %q: %v", body, err)
				}
				return resp.Version
			}

			t.Run("a default build uploaded twice stays one row at its own stamp", func(t *testing.T) {
				name := unique("stamp-twice")
				b := binary(t, name, 1)
				if v := uploaded(t, name, 0, b); v != 1 {
					t.Fatalf("first upload is version %d, want 1", v)
				}
				if v := uploaded(t, name, 0, b); v != 1 {
					t.Fatalf("the same stamp-1 binary uploaded again is version %d, want 1: a row numbered 2 would carry a binary that says 1, and no worker runs it", v)
				}
				if got := rows(t, name); len(got) != 1 || got[0] != 1 {
					t.Fatalf("rows = %v, want exactly [1]", got)
				}
			})

			t.Run("the stamp is the version, not the next free number", func(t *testing.T) {
				name := unique("stamp-three")
				if v := uploaded(t, name, 0, binary(t, name, 3)); v != 3 {
					t.Fatalf("a binary stamped 3, uploaded with no version, is version %d, want 3", v)
				}
			})

			t.Run("a requested version that agrees with the stamp is accepted", func(t *testing.T) {
				name := unique("stamp-agree")
				if v := uploaded(t, name, 2, binary(t, name, 2)); v != 2 {
					t.Fatalf("version = %d, want 2", v)
				}
			})

			t.Run("a requested version that disagrees with the stamp is a 400 naming both and writes nothing", func(t *testing.T) {
				name := unique("stamp-disagree")
				code, body := upload(t, name, 2, binary(t, name, 1))
				if code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body = %s", code, body)
				}
				for _, want := range []string{"version 2 was requested", "stamped version 1"} {
					if !strings.Contains(body, want) {
						t.Errorf("the 400 body %q does not say %q", body, want)
					}
				}
				if got := rows(t, name); len(got) != 0 {
					t.Fatalf("a refused upload still wrote rows %v", got)
				}
			})

			t.Run("a binary stamped 0 is refused at upload and no row is left behind", func(t *testing.T) {
				name := unique("stamp-zero")
				for _, requested := range []int{0, 1} {
					code, body := upload(t, name, requested, binary(t, name, 0))
					if code != http.StatusBadRequest {
						t.Fatalf("requested version %d: status = %d, want 400; body = %s", requested, code, body)
					}
					if !strings.Contains(body, "stamped version 0") {
						t.Errorf("requested version %d: the 400 body %q does not name the stamp", requested, body)
					}
				}
				if got := rows(t, name); len(got) != 0 {
					t.Fatalf("a 0-stamped binary left rows %v: a worker would return a run of it to the queue forever", got)
				}
			})

			t.Run("control: a binary with no metadata still takes the next free number", func(t *testing.T) {
				name := unique("no-metadata")
				plain := func(tag byte) []byte { return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, tag} }
				if v := uploaded(t, name, 0, plain(1)); v != 1 {
					t.Fatalf("first upload is version %d, want 1", v)
				}
				if v := uploaded(t, name, 0, plain(2)); v != 2 {
					t.Fatalf("second upload is version %d, want 2", v)
				}
			})
		})
	}
}
