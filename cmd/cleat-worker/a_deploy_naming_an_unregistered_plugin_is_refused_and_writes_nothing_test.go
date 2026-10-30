package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// recordingPluginSource answers from a fixed set, counts how often it was asked, and keeps the
// overrides written to it (or refuses them when recordErr is set).
type recordingPluginSource struct {
	mu        sync.Mutex
	known     map[string]bool
	calls     int
	overrides []engine.PluginDepOverride
	recordErr error
}

func (r *recordingPluginSource) RecordPluginDepOverride(_ context.Context, o engine.PluginDepOverride) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recordErr != nil {
		return r.recordErr
	}
	r.overrides = append(r.overrides, o)
	return nil
}

func (r *recordingPluginSource) KnownPlugins(_ context.Context, names []string) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	out := map[string]bool{}
	for _, n := range names {
		if r.known[n] {
			out[n] = true
		}
	}
	return out, nil
}

// cleat#3600 (owner D52): POST /api/definitions refuses a definition naming a plugin no
// worker has ever registered, before anything is written. On real stores of all three
// dialects, because "writes nothing" is a statement about the store.
func TestADeployNamingAnUnregisteredPluginIsRefusedAndWritesNothing(t *testing.T) {
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

			src := &recordingPluginSource{known: map[string]bool{"registered-plugin": true}}
			api := &apiServer{store: store, worker: newTestWorker(&mockStore{}), maxBodySize: 1 << 20, pluginReg: src}
			mux := http.NewServeMux()
			registerRoutes(mux, api)

			binary := func(t *testing.T, name string, deps map[string]string) []byte {
				t.Helper()
				out, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"),
					&wasm.Metadata{WorkflowName: name, WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 1, PluginDeps: deps})
				if err != nil {
					t.Fatalf("WriteMetadata: %v", err)
				}
				return out
			}
			upload := func(t *testing.T, body map[string]any) (int, map[string]any) {
				t.Helper()
				raw, _ := json.Marshal(body)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(raw))))
				var resp map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				return rec.Code, resp
			}
			request := func(name string, wasmBytes []byte, rowDeps map[string]string, allow bool) map[string]any {
				body := map[string]any{"name": name, "wasm_bytes_base64": base64.StdEncoding.EncodeToString(wasmBytes)}
				if rowDeps != nil {
					body["plugin_deps"] = rowDeps
				}
				if allow {
					body["allow_missing_plugins"] = true
				}
				return body
			}
			rows := func(t *testing.T, name string) int {
				t.Helper()
				defs, err := store.ListWorkflowDefs(ctx, name)
				if err != nil {
					t.Fatalf("ListWorkflowDefs(%s): %v", name, err)
				}
				return len(defs)
			}
			unique := func(base string) string { return base + "-" + uuid.New().String()[:8] }
			missingOf := func(t *testing.T, resp map[string]any) map[string]string { // name -> declared by
				t.Helper()
				out := map[string]string{}
				list, _ := resp["missing_plugins"].([]any)
				for _, e := range list {
					m, _ := e.(map[string]any)
					out[m["Name"].(string)] = m["DeclaredBy"].(string)
				}
				return out
			}

			t.Run("a plugin named by the request body only is refused, named, and nothing is written", func(t *testing.T) {
				name := unique("deps-row")
				code, resp := upload(t, request(name, binary(t, name, nil), map[string]string{"never-registered": ">=1"}, false))
				if code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422; body = %v", code, resp)
				}
				got := missingOf(t, resp)
				if len(got) != 1 || got["never-registered"] != engine.DeclaredByRow {
					t.Errorf("missing = %v, want never-registered declared by the definition", got)
				}
				if n := rows(t, name); n != 0 {
					t.Errorf("%d rows written by a refused deploy, want 0", n)
				}
			})

			t.Run("a plugin named by the binary only is refused and says it was the binary", func(t *testing.T) {
				name := unique("deps-binary")
				code, resp := upload(t, request(name, binary(t, name, map[string]string{"binary-only-plugin": ">=2"}), nil, false))
				if code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422; body = %v", code, resp)
				}
				if got := missingOf(t, resp); len(got) != 1 || got["binary-only-plugin"] != engine.DeclaredByBinary {
					t.Errorf("missing = %v, want binary-only-plugin declared by the WASM binary", got)
				}
				if n := rows(t, name); n != 0 {
					t.Errorf("%d rows written by a refused deploy, want 0", n)
				}
			})

			t.Run("every missing plugin is named in one refusal, and a registered one is not", func(t *testing.T) {
				name := unique("deps-many")
				code, resp := upload(t, request(name,
					binary(t, name, map[string]string{"missing-one": "", "registered-plugin": ">=1"}),
					map[string]string{"missing-two": ">=3"}, false))
				if code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422; body = %v", code, resp)
				}
				got := missingOf(t, resp)
				if len(got) != 2 || got["missing-one"] == "" || got["missing-two"] == "" {
					t.Errorf("missing = %v, want exactly missing-one and missing-two", got)
				}
				if n := rows(t, name); n != 0 {
					t.Errorf("%d rows written by a refused deploy, want 0", n)
				}
			})

			t.Run("a registered plugin deploys", func(t *testing.T) {
				name := unique("deps-ok")
				code, resp := upload(t, request(name, binary(t, name, map[string]string{"registered-plugin": ">=1"}), nil, false))
				if code != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body = %v", code, resp)
				}
				if n := rows(t, name); n != 1 {
					t.Errorf("%d rows after a deploy that was allowed, want 1", n)
				}
			})

			t.Run("the explicit override deploys, records itself before the write, and logs the missing names at WARN", func(t *testing.T) {
				var buf bytes.Buffer
				prev := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
				t.Cleanup(func() { slog.SetDefault(prev) })

				name := unique("deps-allow")
				before := len(src.overrides)
				code, resp := upload(t, request(name, binary(t, name, nil), map[string]string{"bootstrap-plugin": ">=1"}, true))
				if code != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body = %v", code, resp)
				}
				if n := rows(t, name); n != 1 {
					t.Errorf("%d rows after an overridden deploy, want 1", n)
				}
				if len(src.overrides) != before+1 {
					t.Fatalf("recorded %d overrides, want exactly one for this deploy", len(src.overrides)-before)
				}
				rec := src.overrides[len(src.overrides)-1]
				if rec.Definition != name || rec.Via != "rest" || rec.Version != 1 || rec.TenantID != engine.DefaultTenantUUID ||
					len(rec.Missing) != 1 || rec.Missing[0].Name != "bootstrap-plugin" {
					t.Errorf("record = %+v, want this definition, via rest, version 1, the default tenant and bootstrap-plugin", rec)
				}
				out := buf.String()
				for _, want := range []string{"level=WARN", "bootstrap-plugin", "allow-missing-plugins"} {
					if !strings.Contains(out, want) {
						t.Errorf("the override's log %q lacks %q", out, want)
					}
				}
			})

			t.Run("the override changes only the deploy that carries it", func(t *testing.T) {
				first := unique("deps-once-a")
				if code, resp := upload(t, request(first, binary(t, first, nil), map[string]string{"bootstrap-plugin": ">=1"}, true)); code != http.StatusCreated {
					t.Fatalf("the overridden deploy: status = %d; body = %v", code, resp)
				}
				second := unique("deps-once-b")
				code, resp := upload(t, request(second, binary(t, second, nil), map[string]string{"bootstrap-plugin": ">=1"}, false))
				if code != http.StatusUnprocessableEntity {
					t.Errorf("the next deploy of the same missing plugin without the field: status = %d, want 422; body = %v", code, resp)
				}
				if n := rows(t, second); n != 0 {
					t.Errorf("%d rows written by the deploy that did not carry the override, want 0", n)
				}
			})

			t.Run("an override that cannot be recorded is refused and writes nothing", func(t *testing.T) {
				src.mu.Lock()
				src.recordErr = errors.New("audit table unwritable")
				src.mu.Unlock()
				t.Cleanup(func() { src.mu.Lock(); src.recordErr = nil; src.mu.Unlock() })
				name := unique("deps-unrecordable")
				code, resp := upload(t, request(name, binary(t, name, nil), map[string]string{"bootstrap-plugin": ">=1"}, true))
				if code == http.StatusCreated {
					t.Fatalf("an override whose record failed deployed; body = %v", resp)
				}
				if n := rows(t, name); n != 0 {
					t.Errorf("%d rows written by an override that could not be recorded, want 0", n)
				}
			})

			t.Run("a definition with no plugin_deps never asks the registry", func(t *testing.T) {
				before := src.calls
				name := unique("deps-none")
				if code, resp := upload(t, request(name, binary(t, name, nil), nil, false)); code != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body = %v", code, resp)
				}
				if src.calls != before {
					t.Errorf("the registry was asked %d times for a definition that declares nothing", src.calls-before)
				}
			})
		})
	}
}

// With no registry to ask, a definition that declares dependencies is refused and nothing is
// written; it is not read as "every plugin is known".
func TestADeployWithNoPluginRegistryRefusesWhatDeclaresDependenciesAndWritesNothing(t *testing.T) {
	store := &mockStore{}
	api := &apiServer{store: store, worker: newTestWorker(&mockStore{}), maxBodySize: 1 << 20}
	mux := http.NewServeMux()
	registerRoutes(mux, api)
	bin, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"),
		&wasm.Metadata{WorkflowName: "w", WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 1, PluginDeps: map[string]string{"x": ""}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"name": "w", "wasm_bytes_base64": base64.StdEncoding.EncodeToString(bin)})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(raw))))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body = %s", rec.Code, rec.Body.String())
	}
}
