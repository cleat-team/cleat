package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// dialectOnlyFactory is the part of a store factory the deploy route's plugin check uses: which
// dialect the worker's database speaks, from which it names the registry table.
type dialectOnlyFactory struct {
	engine.StoreFactory
	d engine.Dialect
}

func (f dialectOnlyFactory) Dialect() engine.Dialect { return f.d }

func depBinary(t *testing.T, name string, deps map[string]string) []byte {
	t.Helper()
	out, err := wasm.WriteMetadata([]byte("\x00asm\x01\x00\x00\x00"),
		&wasm.Metadata{WorkflowName: name, WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 1, PluginDeps: deps})
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	return out
}

func postDefinition(t *testing.T, mux *http.ServeMux, name string, wasmBytes []byte, allow bool) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"name": name, "wasm_bytes_base64": base64.StdEncoding.EncodeToString(wasmBytes)}
	if allow {
		body["allow_missing_plugins"] = true
	}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/definitions", strings.NewReader(string(raw))))
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func missingNamesIn(resp map[string]any) []string {
	var out []string
	list, _ := resp["missing_plugins"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)
		out = append(out, m["Name"].(string))
	}
	return out
}

// cleat#3600. The deploy route reading the REAL registry, not a fake: a plugin registered through
// a real worker registration lets a deploy pass, an unregistered name is refused with a 422 that
// names it, and an override writes a real audit row in the real table. On all three dialects,
// because the table, its name and its SQL are per dialect.
func TestADeployReadsTheRealPluginRegistryAndAuditsItsOverride(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		be := be
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()
			ctx := context.Background()
			testutil.SetupFullSchema(t, be.DB, be.Dialect)
			d := engine.Dialect(be.Dialect)

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
			// No fake: the route reads be.DB, the way a worker's --db is read in production.
			api := &apiServer{store: store, worker: newTestWorker(&mockStore{}), maxBodySize: 1 << 20,
				db: be.DB, factory: dialectOnlyFactory{d: d}}
			mux := http.NewServeMux()
			registerRoutes(mux, api)

			suffix := uuid.New().String()[:8]
			registered, missing := "real-registered-"+suffix, "real-missing-"+suffix
			workerID := "real-worker-" + suffix
			regTable, ovTable := "admin.registered_plugins", "admin.plugin_dep_overrides"
			if d == engine.DialectMySQL {
				regTable, ovTable = "registered_plugins", "plugin_dep_overrides"
			}
			ph := func(n int) string {
				switch d {
				case engine.DialectPostgres:
					return fmt.Sprintf("$%d", n)
				case engine.DialectMSSQL:
					return fmt.Sprintf("@p%d", n)
				default:
					return "?"
				}
			}
			t.Cleanup(func() {
				_, _ = be.DB.Exec(`DELETE FROM `+regTable+` WHERE plugin_name IN (`+ph(1)+`, `+ph(2)+`)`, registered, missing)
				_, _ = be.DB.Exec(`DELETE FROM `+ovTable+` WHERE definition_name LIKE `+ph(1), "%"+suffix)
				_ = (&engine.WorkerRegistry{DB: be.DB, Dialect: d}).Deregister(context.Background(), workerID)
			})

			// A real registration, the call a worker makes at startup, records the plugin.
			if err := (&engine.WorkerRegistry{DB: be.DB, Dialect: d}).Register(ctx,
				engine.WorkerRegistration{WorkerID: workerID, Hostname: "h", PID: 1, Plugins: []string{registered}}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			rows := func(name string) int {
				defs, err := store.ListWorkflowDefs(ctx, name)
				if err != nil {
					t.Fatalf("ListWorkflowDefs(%s): %v", name, err)
				}
				return len(defs)
			}
			auditRows := func(name string) int {
				var n int
				if err := be.DB.QueryRow(`SELECT count(*) FROM `+ovTable+` WHERE definition_name = `+ph(1), name).Scan(&n); err != nil {
					t.Fatalf("count audit rows: %v", err)
				}
				return n
			}

			t.Run("a plugin a real worker registered lets the deploy pass", func(t *testing.T) {
				name := "real-ok-" + suffix
				code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{registered: ">=1"}), false)
				if code != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body = %v", code, resp)
				}
				if rows(name) != 1 {
					t.Errorf("%d rows, want 1", rows(name))
				}
				if auditRows(name) != 0 {
					t.Error("an audit row exists for a deploy that needed no override")
				}
			})

			t.Run("an unregistered name is a 422 that names it, and nothing is written", func(t *testing.T) {
				name := "real-refused-" + suffix
				code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{registered: ">=1", missing: ">=2"}), false)
				if code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422; body = %v", code, resp)
				}
				if got := missingNamesIn(resp); len(got) != 1 || got[0] != missing {
					t.Errorf("missing = %v, want only %q (the registered one must not be listed)", got, missing)
				}
				if rows(name) != 0 || auditRows(name) != 0 {
					t.Errorf("a refused deploy left %d definition rows and %d audit rows, want none", rows(name), auditRows(name))
				}
			})

			t.Run("the override deploys and writes one real audit row", func(t *testing.T) {
				name := "real-override-" + suffix
				code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{missing: ">=2"}), true)
				if code != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body = %v", code, resp)
				}
				if rows(name) != 1 {
					t.Errorf("%d definition rows, want 1", rows(name))
				}
				var tenant, via, missingJSON string
				var version int
				if err := be.DB.QueryRow(`SELECT for_tenant_id, definition_version, via, missing_plugins FROM `+ovTable+` WHERE definition_name = `+ph(1), name).
					Scan(&tenant, &version, &via, &missingJSON); err != nil {
					t.Fatalf("read the audit row: %v (no row means the override was not recorded)", err)
				}
				if tenant != engine.DefaultTenantUUID || version != 1 || via != "rest" || !strings.Contains(missingJSON, missing) {
					t.Errorf("audit row = tenant %q version %d via %q missing %q, want the default tenant, version 1, rest and %q", tenant, version, via, missingJSON, missing)
				}
				if auditRows(name) != 1 {
					t.Errorf("%d audit rows, want exactly 1", auditRows(name))
				}
			})

			t.Run("the field changes only the deploy that carried it", func(t *testing.T) {
				name := "real-after-" + suffix
				code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{missing: ">=2"}), false)
				if code != http.StatusUnprocessableEntity {
					t.Errorf("a later deploy of the same missing plugin without the field: status = %d, want 422; body = %v", code, resp)
				}
			})

			t.Run("the override is not needed, and not recorded, for a registered plugin", func(t *testing.T) {
				name := "real-unneeded-" + suffix
				code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{registered: ">=1"}), true)
				if code != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body = %v", code, resp)
				}
				if auditRows(name) != 0 {
					t.Error("an override was recorded for a deploy whose plugins were all registered")
				}
			})
		})
	}
}

// cleat#3600. What the check does on a ShardedStore, which a deploy fans out over. The route does
// NOT reach the registry through the store (no type assertion that a sharded store could quietly
// fail): it reads the worker's own database, which in a sharded worker is the FIRST shard's, the
// same one worker registration writes (main.go: "the first shard's database for plugin
// migrations and administration"). So on two real databases: a refusal writes nothing on EITHER
// shard; an allowed deploy lands on BOTH; the registry that counts is shard 0's, so a name
// registered only on shard 1 does not satisfy it; and an override's audit row is written once, on
// shard 0. A registry that cannot be read is a refusal, never a pass.
func TestAShardedDeployChecksTheFirstShardsRegistryAndWritesEveryShardOrNone(t *testing.T) {
	admin := os.Getenv("CLEAT_TEST_POSTGRES")
	if admin == "" {
		admin = os.Getenv("CLEAT_TEST_DB")
	}
	if admin == "" {
		t.Skip("CLEAT_TEST_POSTGRES/CLEAT_TEST_DB not set, skipping")
	}
	ctx := context.Background()
	adb, err := sql.Open("postgres", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()
	if err := adb.Ping(); err != nil {
		t.Fatalf("%s is set but unreachable: %v", admin, err)
	}

	stamp := time.Now().UnixNano() % 1_000_000_000
	var dbs [2]*sql.DB
	var names [2]string
	for i := range dbs {
		names[i] = fmt.Sprintf("cleat_3600_shard%d_%d", i, stamp)
		if _, err := adb.Exec(`CREATE DATABASE ` + names[i]); err != nil {
			t.Fatal(err)
		}
		name := names[i]
		t.Cleanup(func() {
			a, err := sql.Open("postgres", admin)
			if err != nil {
				return
			}
			defer a.Close()
			_, _ = a.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
			_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name)
		})
		db, err := sql.Open("postgres", replaceDBName(t, admin, names[i]))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
		dbs[i] = db
	}
	stores := []engine.WorkflowStore{engine.NewPostgresStore(dbs[0]), engine.NewPostgresStore(dbs[1])}
	sharded, err := engine.NewShardedStore(
		[]engine.ShardConfig{{Name: "shard-0"}, {Name: "shard-1"}}, stores,
		[]func() error{func() error { return nil }, func() error { return nil }})
	if err != nil {
		t.Fatalf("NewShardedStore: %v", err)
	}

	// db is shard 0's, as main.go sets it for a sharded worker.
	api := &apiServer{store: sharded, worker: newTestWorker(&mockStore{}), maxBodySize: 1 << 20,
		db: dbs[0], factory: dialectOnlyFactory{d: engine.DialectPostgres}}
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	defsOn := func(shard int, name string) int {
		var n int
		if err := dbs[shard].QueryRow(`SELECT count(*) FROM workflow_defs WHERE name = $1`, name).Scan(&n); err != nil {
			t.Fatalf("count defs on shard %d: %v", shard, err)
		}
		return n
	}
	auditOn := func(shard int, name string) int {
		var n int
		if err := dbs[shard].QueryRow(`SELECT count(*) FROM admin.plugin_dep_overrides WHERE definition_name = $1`, name).Scan(&n); err != nil {
			t.Fatalf("count audit rows on shard %d: %v", shard, err)
		}
		return n
	}
	plugin := fmt.Sprintf("sharded-plugin-%d", stamp)

	t.Run("an unregistered plugin is refused and nothing is written on either shard", func(t *testing.T) {
		name := fmt.Sprintf("sharded-refused-%d", stamp)
		code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{plugin: ">=1"}), false)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", code, resp)
		}
		if defsOn(0, name) != 0 || defsOn(1, name) != 0 {
			t.Errorf("a refused sharded deploy left rows: shard 0 has %d, shard 1 has %d", defsOn(0, name), defsOn(1, name))
		}
	})

	t.Run("a name registered only on shard 1 does not satisfy the check: shard 0's registry is the one read", func(t *testing.T) {
		if err := (&engine.PluginNameRegistry{DB: dbs[1], Dialect: engine.DialectPostgres}).Record(ctx, []string{plugin}); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("sharded-wrong-shard-%d", stamp)
		code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{plugin: ">=1"}), false)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (the registry that counts is shard 0's, where a worker registers); body = %v", code, resp)
		}
		if defsOn(0, name) != 0 || defsOn(1, name) != 0 {
			t.Errorf("rows were written by a refused deploy: shard 0 %d, shard 1 %d", defsOn(0, name), defsOn(1, name))
		}
	})

	t.Run("registered on shard 0 the deploy passes and lands on BOTH shards", func(t *testing.T) {
		if err := (&engine.PluginNameRegistry{DB: dbs[0], Dialect: engine.DialectPostgres}).Record(ctx, []string{plugin}); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("sharded-ok-%d", stamp)
		code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{plugin: ">=1"}), false)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %v", code, resp)
		}
		if defsOn(0, name) != 1 || defsOn(1, name) != 1 {
			t.Errorf("definition rows: shard 0 %d, shard 1 %d, want one on each", defsOn(0, name), defsOn(1, name))
		}
	})

	t.Run("an override on a sharded deploy lands on both shards and its audit row is written once, on shard 0", func(t *testing.T) {
		name := fmt.Sprintf("sharded-override-%d", stamp)
		code, resp := postDefinition(t, mux, name, depBinary(t, name, map[string]string{"sharded-never-" + fmt.Sprint(stamp): ">=1"}), true)
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %v", code, resp)
		}
		if defsOn(0, name) != 1 || defsOn(1, name) != 1 {
			t.Errorf("definition rows: shard 0 %d, shard 1 %d, want one on each", defsOn(0, name), defsOn(1, name))
		}
		if auditOn(0, name) != 1 || auditOn(1, name) != 0 {
			t.Errorf("audit rows: shard 0 %d, shard 1 %d, want 1 and 0", auditOn(0, name), auditOn(1, name))
		}
	})
}
