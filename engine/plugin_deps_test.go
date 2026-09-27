package engine

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// pluginDepsBackend builds a store *and* keeps the *sql.DB, which the shared
// forEachBackend helper does not expose. These tests have to reach past the
// store to write the raw column.
type pluginDepsBackend struct {
	name    string
	dialect testutil.Dialect
	// update carries this dialect's placeholder syntax. A wrong placeholder is a
	// syntax error, so getting it wrong fails loudly rather than silently.
	update string
	// newStore builds the store the way production builds it. For SQL Server
	// that means going through the factory: NewMSSQLStore on a plain pool never
	// sets sp_set_session_context, so under the shipped security policies it
	// cannot see rows it just wrote (see openMSSQLTenantStore).
	newStore func(*testing.T, *sql.DB) WorkflowStore
	// prepareRawAccess runs before direct SQL against workflow_defs. SQL
	// Server's FILTER PREDICATE hides every row from a connection with no tenant
	// session context, so without this a direct UPDATE matches zero rows and
	// silently does nothing -- the test would then report a failure that is
	// really its own.
	prepareRawAccess func(*testing.T, *sql.DB) *sql.Conn
}

func pluginDepsBackends() []pluginDepsBackend {
	return []pluginDepsBackend{
		{
			name: "postgres", dialect: testutil.DialectPostgres,
			update:   `UPDATE workflow_defs SET plugin_deps = $1 WHERE name = $2 AND version = 1`,
			newStore: func(_ *testing.T, db *sql.DB) WorkflowStore { return NewPostgresStore(db) },
		},
		{
			name: "mysql", dialect: testutil.DialectMySQL,
			update:   `UPDATE workflow_defs SET plugin_deps = ? WHERE name = ? AND version = 1`,
			newStore: func(_ *testing.T, db *sql.DB) WorkflowStore { return NewMySQLStore(db) },
		},
		{
			name: "mssql", dialect: testutil.DialectMSSQL,
			update: `UPDATE workflow_defs SET plugin_deps = @p1 WHERE name = @p2 AND version = 1`,
			newStore: func(t *testing.T, _ *sql.DB) WorkflowStore {
				return openMSSQLTenantStore(t, DefaultTenantUUID)
			},
			prepareRawAccess: func(t *testing.T, db *sql.DB) *sql.Conn {
				t.Helper()
				// Pinned, because the session context lives on the connection
				// and the pool is otherwise free to hand the next statement a
				// different one.
				conn, err := db.Conn(context.Background())
				if err != nil {
					t.Fatalf("pinning a connection: %v", err)
				}
				if _, err := conn.ExecContext(context.Background(),
					"EXEC sp_set_session_context @key=N'tenant_id', @value=N'"+DefaultTenantUUID+"'"); err != nil {
					t.Fatalf("setting tenant session context: %v", err)
				}
				return conn
			},
		},
	}
}

func setupPluginDepsDB(t *testing.T, b pluginDepsBackend) (*sql.DB, WorkflowStore) {
	t.Helper()
	// TestDB first, and deliberately: it owns the per-dialect "was this database
	// asked for" decision (engine/testutil/schema.go) and skips before anything
	// else runs. Adding a second skip here would give one condition two
	// mechanisms, which is the shape scripts/check-skips.sh exists to prevent.
	db := testutil.TestDB(t, b.dialect)
	testutil.SetupFullSchema(t, db, b.dialect)
	testutil.CleanupAllTestData(t, db, b.dialect)
	t.Cleanup(func() {
		testutil.CleanupAllTestData(t, db, b.dialect)
		db.Close()
	})
	return db, b.newStore(t, db)
}

// TestPluginDepsRoundTrip is the test that should have existed, and the one that
// found the defect this file is really about.
//
// MSSQLStore.DeployWorkflowDef passed the marshalled JSON as []byte.
// go-mssqldb binds []byte as VARBINARY, and the implicit conversion into the
// column's NVARCHAR(MAX) reinterprets the UTF-8 bytes as UTF-16, so
//
//	{"llm":"1.2.0"}   came back as   ≻汬≭∺⸱⸲∰}
//
// PostgreSQL (JSONB) and MySQL (JSON) were unaffected: both are validating JSON
// types and both drivers bind []byte correctly for them.
//
// Nothing noticed because the read discarded its json.Unmarshal error and
// defaulted the nil map to an empty one, so every caller on SQL Server saw the
// entirely plausible answer "this workflow declares no plugin dependencies".
// Every plugin_deps row ever written by SQL Server is mangled.
//
// It propagates, too: cleatctl deploy carries the previous version's PluginDeps
// onto the new one, so the loss travels down the version chain and each new
// version records it as fact.
func TestPluginDepsRoundTrip(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			_, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			const defName = "plugin-deps-roundtrip"
			want := map[string]string{"llm": "1.2.0", "slack-notify": "2.0.1"}
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
				PluginDeps: want,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			got, err := store.GetWorkflowDef(ctx, defName, 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef: %v", err)
			}
			if got == nil {
				t.Fatal("GetWorkflowDef returned (nil, nil) for a definition just deployed")
			}
			for k, v := range want {
				if got.PluginDeps[k] != v {
					t.Errorf("plugin_deps did not round-trip: got %v, want %v.\n\n"+
						"An empty or mangled map here means every caller believes this "+
						"workflow declares no plugin dependencies, and cleatctl deploy "+
						"carries that emptiness onto every later version.",
						got.PluginDeps, want)
					break
				}
			}

			defs, err := store.ListWorkflowDefs(ctx, defName)
			if err != nil {
				t.Fatalf("ListWorkflowDefs: %v", err)
			}
			if len(defs) != 1 || defs[0].PluginDeps["llm"] != "1.2.0" {
				t.Errorf("ListWorkflowDefs plugin_deps did not round-trip: %+v", defs)
			}
		})
	}
}

// TestUnparseablePluginDepsIsLoggedNotFatal pins the deliberately permissive
// half.
//
// Returning the error would be strictly more correct, and is what the sibling
// dropped-Unmarshal fixes do. It cannot be done here: every plugin_deps row
// written by SQL Server before the round-trip fix above is mangled, and making
// the read fail would turn a latent data bug into an outage -- a GetWorkflowDef
// that errors means the workflow cannot be loaded at all. The read stays
// permissive and self-heals on the next deploy of each definition.
//
// What changed is that it is no longer silent: decodePluginDeps logs. This test
// pins the contract that an unreadable blob yields an empty map and no error, so
// that tightening it later is a deliberate decision rather than an accident.
func TestUnparseablePluginDepsIsLoggedNotFatal(t *testing.T) {
	for _, backend := range pluginDepsBackends() {
		t.Run(backend.name, func(t *testing.T) {
			db, store := setupPluginDepsDB(t, backend)
			ctx := context.Background()

			const defName = "plugin-deps-unparseable"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
				PluginDeps: map[string]string{"llm": "1.2.0"},
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			// Valid JSON, wrong shape: a version written as a number rather than
			// a string. Postgres JSONB and MySQL JSON both accept it -- neither
			// will store syntactically invalid JSON at all, which is why this is
			// the reachable case rather than a truncated blob. SQL Server's
			// NVARCHAR(MAX) would accept anything.
			var exec interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			} = db
			if backend.prepareRawAccess != nil {
				conn := backend.prepareRawAccess(t, db)
				defer conn.Close()
				exec = conn
			}
			if _, err := exec.ExecContext(ctx, backend.update, `{"llm": 1}`, defName); err != nil {
				t.Fatalf("writing a wrong-shaped plugin_deps: %v", err)
			}

			got, err := store.GetWorkflowDef(ctx, defName, 1)
			if err != nil {
				t.Fatalf("GetWorkflowDef on an unparseable plugin_deps returned an error: %v\n\n"+
					"That is the behaviour this test exists to prevent: it would break every "+
					"SQL Server definition written before the round-trip fix.", err)
			}
			if got == nil {
				t.Fatal("GetWorkflowDef returned (nil, nil)")
			}
			if len(got.PluginDeps) != 0 {
				t.Errorf("PluginDeps = %v, want an empty map", got.PluginDeps)
			}
		})
	}
}
